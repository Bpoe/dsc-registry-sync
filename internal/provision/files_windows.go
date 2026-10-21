package provision

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/Bpoe/dsc-registry-sync/internal/privatefs"
	"golang.org/x/sys/windows"
)

func privileged() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("registration provisioning requires an elevated Administrator or SYSTEM")
	}
	return nil
}

func safeVolume(volume string) bool {
	if len(volume) != 2 || volume[1] != ':' {
		return false
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	return err == nil && windows.GetDriveType(root) == windows.DRIVE_FIXED
}

func samePath(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }

func plainDirectory(info os.FileInfo) bool {
	if info == nil || !info.IsDir() {
		return false
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

func protectedDirectory(root *os.Root, destination bool) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	trusted := func(sid *windows.SID) bool {
		return sid.IsWellKnown(windows.WinLocalSystemSid) ||
			sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) ||
			!destination && (sid.Equals(user.User.Sid) ||
				sid.String() == "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464") // TrustedInstaller
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !trusted(owner) {
		return errors.New("configuration directory has an untrusted owner")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("configuration directory requires a restricted DACL")
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("unsupported configuration directory ACL")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		const deleteChild = 0x40 // FILE_DELETE_CHILD directory access right.
		unsafeRights := uint32(windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | deleteChild |
			windows.GENERIC_ALL | windows.GENERIC_WRITE)
		if destination {
			unsafeRights = ^uint32(0)
		}
		if !trusted(sid) && uint32(ace.Mask)&unsafeRights != 0 {
			return errors.New("configuration directory grants untrusted access")
		}
	}
	return nil
}

func descriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
}

func makeDirectory(root *os.Root, name string) error {
	sd, err := descriptor()
	if err != nil {
		return err
	}
	file, err := privatefs.OpenRelative(root, name, windows.FILE_GENERIC_READ, windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE, sd)
	if err != nil {
		return err
	}
	return file.Close()
}

func createFile(root *os.Root, name string) (*os.File, error) {
	sd, err := descriptor()
	if err != nil {
		return nil, err
	}
	return privatefs.OpenRelative(root, name, windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE, sd)
}

func syncDirectory(root *os.Root) error { return nil }
