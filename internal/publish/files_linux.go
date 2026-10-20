//go:build linux

package publish

import (
	"context"
	"errors"
	"os"
	"syscall"
)

func openRegular(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func makePrivateDirectory(root *os.Root, name string) error { return root.Mkdir(name, 0700) }

func createPrivateFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}

func retryable(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR)
}

func (d *directory) removeConfiguration(ctx context.Context, name string) error {
	return retry(ctx, func() (err error) {
		info, err := d.regular(name)
		if err != nil || info == nil {
			return err
		}
		file, err := openRegular(d.root, name)
		if errors.Is(err, os.ErrNotExist) {
			return syscall.EAGAIN
		}
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, file.Close()) }()
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return err
		}
		// A competing cooperating reader may have opened the old inode before
		// publication. Never delete a replacement while holding that old lock.
		return d.removeOpenedConfiguration(file, name)
	})
}

func (d *directory) removeOpenedConfiguration(file *os.File, name string) error {
	current, err := d.regular(name)
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if current == nil || !opened.Mode().IsRegular() || !os.SameFile(opened, current) {
		return syscall.EAGAIN
	}
	return d.root.Remove(name)
}

func syncDirectory(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
