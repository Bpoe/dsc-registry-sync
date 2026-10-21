package provision

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func privileged() error {
	if os.Geteuid() != 0 {
		return errors.New("registration provisioning requires root")
	}
	return nil
}

func safeVolume(volume string) bool { return volume == "" }

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

func plainDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func protectedDirectory(root *os.Root, destination bool) error {
	info, err := root.Stat(".")
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 &&
		(destination || info.Mode()&os.ModeSticky == 0) {
		return errors.New("configuration directories must be root-owned and protected from untrusted writes")
	}
	return nil
}

func makeDirectory(root *os.Root, name string) error {
	return root.Mkdir(name, 0700)
}

func createFile(root *os.Root, name string) (*os.File, error) {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		return nil, errors.Join(err, file.Close(), root.Remove(name))
	}
	info, err := file.Stat()
	if err != nil || info.Sys().(*syscall.Stat_t).Uid != 0 || info.Mode().Perm() != 0600 {
		return nil, errors.Join(errors.New("cannot establish root-owned mode 0600 file"), file.Close(), root.Remove(name))
	}
	return file, nil
}

func syncDirectory(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
