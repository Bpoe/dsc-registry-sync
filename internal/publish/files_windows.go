//go:build windows

package publish

import (
	"context"
	"errors"
	"os"
	"syscall"

	"github.com/Bpoe/dsc-registry-sync/internal/privatefs"
	"golang.org/x/sys/windows"
)

func openRegular(root *os.Root, name string) (*os.File, error) {
	return privatefs.OpenRelative(root, name, windows.FILE_GENERIC_READ, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE, nil)
}

func privateDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user.User.Sid.String() + ")")
}

func makePrivateDirectory(root *os.Root, name string) error {
	sd, err := privateDescriptor()
	if err != nil {
		return err
	}
	file, err := privatefs.OpenRelative(root, name, windows.FILE_GENERIC_READ, windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE, sd)
	if err != nil {
		return err
	}
	return file.Close()
}

func createPrivateFile(root *os.Root, name string) (*os.File, error) {
	sd, err := privateDescriptor()
	if err != nil {
		return nil, err
	}
	return privatefs.OpenRelative(root, name, windows.FILE_GENERIC_WRITE, windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE, sd)
}

func retryable(err error) bool {
	// dscd opens with FILE_SHARE_READ only; do not retry ordinary ACL denial.
	return errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33)) ||
		errors.Is(err, syscall.Errno(303))
}

func (d *directory) removeConfiguration(ctx context.Context, name string) error {
	return d.remove(ctx, name)
}

// Windows has no portable directory fsync. Staged regular files are flushed
// before same-directory replacement; sudden power-loss durability is OS-owned.
func syncDirectory(root *os.Root) error { return nil }
