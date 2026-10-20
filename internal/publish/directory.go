package publish

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

const tempPrefix = ".dscregsync-"

type directory struct {
	parent, root *os.Root
	info         os.FileInfo
}

func openDirectory(path string, create bool) (*directory, error) {
	if create {
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
	}
	parent, err := os.OpenRoot(path)
	if !create && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			parent.Close()
		}
	}()
	info, err := parent.Lstat("dscregsync")
	if create && errors.Is(err, os.ErrNotExist) {
		err = makePrivateDirectory(parent, "dscregsync")
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, err = parent.Lstat("dscregsync")
	}
	if !create && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("managed directory is not a regular directory")
	}
	root, err := parent.OpenRoot("dscregsync")
	if err != nil {
		return nil, err
	}
	dir := &directory{parent: parent, root: root, info: info}
	if err := dir.check(); err != nil {
		root.Close()
		return nil, err
	}
	success = true
	return dir, nil
}

func (d *directory) close() {
	d.root.Close()
	d.parent.Close()
}

func (d *directory) check() error {
	current, err := d.parent.Lstat("dscregsync")
	if err != nil {
		return err
	}
	opened, err := d.root.Stat(".")
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(d.info, current) || !os.SameFile(d.info, opened) {
		return errors.New("managed directory identity changed")
	}
	return nil
}

func (d *directory) regular(name string) (os.FileInfo, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	info, err := d.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("managed entry is not a regular file")
	}
	return info, nil
}

func (d *directory) entries() ([]fs.DirEntry, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	return fs.ReadDir(d.root.FS(), ".")
}

func (d *directory) current(name, revision string, parameters bool) (bool, error) {
	config, err := d.regular(name + ".json")
	if err != nil {
		return false, err
	}
	sidecar, err := d.regular(name + ".parameters.json")
	if err != nil {
		return false, err
	}
	info, err := d.regular("." + name + ".revision")
	if err != nil || info == nil || config == nil || (sidecar != nil) != parameters {
		return false, err
	}
	file, err := openRegular(d.root, "."+name+".revision")
	if err != nil {
		return false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return false, errors.New("revision identity changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, 39))
	if err != nil {
		return false, err
	}
	value := strings.TrimSuffix(string(data), "\n")
	value = strings.TrimSuffix(value, "\r")
	return value == revision, nil
}

func (d *directory) stage(data []byte) (name string, err error) {
	if err := d.check(); err != nil {
		return "", err
	}
	name = tempPrefix + rand.Text() + ".tmp"
	file, err := createPrivateFile(d.root, name)
	if err != nil {
		return "", err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return "", errors.Join(err, d.removeTemp(name))
	}
	return name, nil
}

func (d *directory) removeTemp(name string) error {
	// Rename consumes the staging name; absence requires no operation/check.
	_, err := d.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Cleanup must not defeat cancellation by waiting on an unexpected reader.
	// Leave a failed cleanup visible as an error; the next 200 can retry it.
	if _, err := d.regular(name); err != nil {
		return err
	}
	err = d.root.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (d *directory) cleanTemps(ctx context.Context) error {
	entries, err := d.entries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		random := strings.TrimSuffix(strings.TrimPrefix(name, tempPrefix), ".tmp")
		if strings.HasPrefix(name, tempPrefix) && strings.HasSuffix(name, ".tmp") &&
			len(random) == 26 && strings.Trim(random, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") == "" {
			if err := d.remove(ctx, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *directory) remove(ctx context.Context, name string) error {
	return retry(ctx, func() error {
		info, err := d.regular(name)
		if err != nil || info == nil {
			return err
		}
		err = d.root.Remove(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

func (d *directory) replace(ctx context.Context, temp, target string) error {
	return retry(ctx, func() error {
		if _, err := d.regular(target); err != nil {
			return err
		}
		if info, err := d.regular(temp); err != nil || info == nil {
			return errors.New("staged file missing or unsafe")
		}
		return d.root.Rename(temp, target)
	})
}

func retry(ctx context.Context, action func() error) error {
	delay := 25 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := action()
		if err == nil || !retryable(err) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 250*time.Millisecond)
	}
}
