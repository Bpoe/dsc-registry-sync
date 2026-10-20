//go:build windows

package publish

import (
	"context"
	"errors"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openRegular(root *os.Root, name string) (*os.File, error) {
	return openRelative(root, name, windows.FILE_GENERIC_READ, windows.FILE_OPEN,
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
	file, err := openRelative(root, name, windows.FILE_GENERIC_READ, windows.FILE_CREATE,
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
	return openRelative(root, name, windows.FILE_GENERIC_WRITE, windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE, sd)
}

// Native handle-relative creation applies the private ACL atomically, without
// a publicly readable interval or a path-based ACL operation that can escape.
func openRelative(root *os.Root, name string, access, disposition, options uint32,
	sd *windows.SECURITY_DESCRIPTOR,
) (*os.File, error) {
	parent, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(parent.Fd()), ObjectName: objectName,
		Attributes: windows.OBJ_DONT_REPARSE, SecurityDescriptor: sd,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, access, &attributes, &windows.IO_STATUS_BLOCK{}, nil,
		windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition, options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		var status windows.NTStatus
		if errors.As(err, &status) {
			err = status.Errno()
		}
		return nil, err
	}
	return os.NewFile(uintptr(handle), name), nil
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
