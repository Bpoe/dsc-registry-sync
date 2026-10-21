// Package provision installs a new service configuration without replacing
// existing state. All paths are checked before making a registration request.
package provision

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Bpoe/dsc-registry-sync/internal/config"
)

type stagedFile struct {
	name, target string
	file         *os.File
	info         os.FileInfo
	published    bool
}

type directory struct {
	parent, root *os.Root
	name         string
	info         os.FileInfo
	created      bool
}

type Files struct {
	dirs      []directory
	config    stagedFile
	key       stagedFile
	probe     string
	committed bool
}

var errConfigured = errors.New("machine is already configured locally; configuration/key destinations must not exist")

// Prepare validates every ancestor and stages both files in the destination
// directory. A hard-link probe verifies atomic no-replace publication support.
func Prepare(path string, cfg config.Config) (_ *Files, err error) {
	if err := privileged(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) ||
		!filepath.IsLocal(filepath.Base(path)) || filepath.Base(path) == "." ||
		samePath(path, cfg.APIKeyFile) ||
		filepath.Dir(path) != filepath.Dir(cfg.APIKeyFile) {
		return nil, errors.New("configuration and key must be distinct absolute files in the same protected directory")
	}
	files := &Files{}
	defer func() {
		if err != nil {
			cleanupErr := files.Close()
			if !errors.Is(err, errConfigured) {
				// OS errors may include caller-controlled paths or arbitrary text.
				err = errors.New("cannot prepare local configuration (check protected directories, permissions and filesystem support)")
			}
			err = errors.Join(err, cleanupErr)
		}
	}()
	if err = files.openDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	root := files.root()
	for _, name := range []string{filepath.Base(path), filepath.Base(cfg.APIKeyFile)} {
		if _, statErr := root.Lstat(name); statErr == nil {
			return nil, errConfigured
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
	}
	if err = files.stage(&files.config, filepath.Base(path)); err != nil {
		return nil, err
	}
	if err = files.stage(&files.key, filepath.Base(cfg.APIKeyFile)); err != nil {
		return nil, err
	}
	if err = json.NewEncoder(files.config.file).Encode(cfg); err != nil {
		return nil, err
	}
	if err = files.config.file.Sync(); err != nil {
		return nil, err
	}
	probe := ".dscregsync-register-" + rand.Text() + ".probe"
	if err = root.Link(files.key.name, probe); err != nil {
		return nil, err
	}
	files.probe = probe
	if err = root.Remove(probe); err != nil {
		return nil, errors.New("cannot remove local publication probe")
	}
	files.probe = ""
	if err = syncDirectory(root); err != nil {
		return nil, err
	}
	return files, nil
}

func (f *Files) root() *os.Root { return f.dirs[len(f.dirs)-1].root }

func (f *Files) openDirectory(path string) error {
	volume := filepath.VolumeName(path)
	if !safeVolume(volume) {
		return errors.New("unsupported filesystem volume")
	}
	anchor := volume + string(filepath.Separator)
	root, err := os.OpenRoot(anchor)
	if err != nil {
		return err
	}
	info, err := root.Stat(".")
	f.dirs = append(f.dirs, directory{root: root, info: info})
	if err != nil {
		return err
	}
	if err := protectedDirectory(root, false); err != nil {
		return err
	}
	relative := strings.TrimPrefix(filepath.Clean(path), anchor)
	if relative == "" {
		return errors.New("configuration cannot be placed at a filesystem root")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	for i, part := range parts {
		if !filepath.IsLocal(part) || part == "." {
			return errors.New("invalid configuration directory")
		}
		parent := root
		info, err := parent.Lstat(part)
		created := false
		if errors.Is(err, os.ErrNotExist) {
			if err := makeDirectory(parent, part); err != nil {
				return err
			}
			created = true
			info, err = parent.Lstat(part)
		}
		if err != nil || !plainDirectory(info) {
			return errors.New("configuration ancestor is not a plain directory")
		}
		root, err = parent.OpenRoot(part)
		if err != nil {
			return err
		}
		f.dirs = append(f.dirs, directory{parent: parent, root: root, name: part, info: info, created: created})
		opened, err := root.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			return errors.New("configuration directory changed")
		}
		if err := protectedDirectory(root, i == len(parts)-1); err != nil {
			return err
		}
	}
	return nil
}

func (f *Files) check() error {
	for _, dir := range f.dirs {
		if dir.parent != nil {
			current, err := dir.parent.Lstat(dir.name)
			if err != nil || !plainDirectory(current) || !os.SameFile(dir.info, current) {
				return errors.New("configuration directory changed")
			}
		}
		if err := protectedDirectory(dir.root, dir.root == f.root()); err != nil {
			return err
		}
	}
	return nil
}

func (f *Files) stage(stage *stagedFile, target string) error {
	name := ".dscregsync-register-" + rand.Text() + ".tmp"
	file, err := createFile(f.root(), name)
	if err != nil {
		return err
	}
	*stage = stagedFile{name: name, target: target, file: file}
	stage.info, err = file.Stat()
	return err
}

func (f *Files) Publish(ctx context.Context, key string) error {
	if f.committed || f.key.file == nil || f.config.file == nil {
		return errors.New("provisioning attempt is already completed or closed")
	}
	if err := config.ValidateAPIKey(key); err != nil {
		return err
	}
	if _, err := f.key.file.WriteString(key); err != nil {
		return errors.New("cannot write machine credential")
	}
	if err := f.key.file.Sync(); err != nil {
		return errors.New("cannot flush machine credential")
	}
	for _, stage := range []*stagedFile{&f.key, &f.config} {
		if ctx.Err() != nil || f.check() != nil {
			return errors.New("local provisioning interrupted or directory changed")
		}
		info, err := f.root().Lstat(stage.name)
		if err != nil || !info.Mode().IsRegular() || !os.SameFile(stage.info, info) {
			return errors.New("staged provisioning file changed")
		}
		// Link is atomic and refuses every existing destination, including symlinks.
		if err := f.root().Link(stage.name, stage.target); err != nil {
			return errors.New("cannot publish local provisioning file without replacing existing state")
		}
		stage.published = true
		if err := syncDirectory(f.root()); err != nil {
			return errors.New("cannot flush local provisioning directory")
		}
	}
	f.committed = true
	return nil
}

// Close removes only this attempt's files, and on failure its empty directories.
// Identity checks prevent cleanup from deleting a concurrent writer's replacement.
func (f *Files) Close() error {
	var errs []error
	for _, stage := range []*stagedFile{&f.config, &f.key} {
		if stage.file == nil {
			continue
		}
		errs = append(errs, stage.file.Close())
		stage.file = nil
		names := []string{stage.name}
		if stage == &f.key && f.probe != "" {
			names = append(names, f.probe)
		}
		if stage.published && !f.committed {
			names = append(names, stage.target)
		}
		for _, name := range names {
			info, err := f.root().Lstat(name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err == nil && info.Mode().IsRegular() && os.SameFile(stage.info, info) {
				err = f.root().Remove(name)
			} else if err == nil {
				err = errors.New("provisioning cleanup identity changed")
			}
			errs = append(errs, err)
		}
	}
	for i := len(f.dirs) - 1; i >= 0; i-- {
		dir := f.dirs[i]
		errs = append(errs, dir.root.Close())
		if dir.created && !f.committed {
			current, err := dir.parent.Lstat(dir.name)
			if err == nil && plainDirectory(current) && os.SameFile(dir.info, current) {
				err = dir.parent.Remove(dir.name)
			}
			errs = append(errs, err)
		}
	}
	f.dirs = nil
	if errors.Join(errs...) != nil {
		return errors.New("local provisioning cleanup incomplete; inspect protected destination files before retrying")
	}
	return nil
}
