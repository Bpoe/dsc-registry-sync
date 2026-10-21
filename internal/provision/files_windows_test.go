package provision

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func replacementBlocked(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func TestWindowsRejectsCaseInsensitiveKeyAlias(t *testing.T) {
	path, cfg := setup(t)
	path = filepath.Join(filepath.Dir(path), "DSCREGSYNCD.KEY")
	if _, err := Prepare(path, cfg); err == nil {
		t.Fatal("configuration aliases credential destination")
	}
}

func checkPermissions(t *testing.T, paths ...string) {
	t.Helper()
	paths = append(paths, filepath.Dir(paths[0]))
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		assertPrivateDescriptor(t, sd)
	}
}

func assertPrivateDescriptor(t *testing.T, sd *windows.SECURITY_DESCRIPTOR) {
	t.Helper()
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("credential ACL permits inheritance")
	}
	owner, _, err := sd.Owner()
	if err != nil || !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		t.Fatal("credential owner is not Administrators")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		t.Fatal("credential ACL must contain only SYSTEM and Administrators")
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			!sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			t.Fatal("unexpected credential ACL principal")
		}
	}
}

func TestWindowsCredentialDescriptor(t *testing.T) {
	sd, err := descriptor()
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateDescriptor(t, sd)
}

func TestWindowsPreflightAncestors(t *testing.T) {
	if privileged() != nil {
		t.Skip("requires elevation")
	}
	files := &Files{}
	err := files.openDirectory(filepath.Join(t.TempDir(), "private"))
	defer files.Close()
	if err != nil {
		if len(files.dirs) != 0 {
			t.Log("last checked directory:", files.root().Name())
		}
		t.Fatal(err)
	}
	if err := files.stage(&files.key, "key"); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionRejectsUnprotectedWindowsParent(t *testing.T) {
	path, cfg := setup(t)
	files, err := Prepare(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	files.committed = true
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(filepath.Dir(path), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(path, cfg); err == nil {
		t.Fatal("publicly accessible credential parent accepted")
	}
	assertNoStaging(t, filepath.Dir(path))
}
