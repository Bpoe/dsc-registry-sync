//go:build windows

package publish

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func holdConfiguration(t *testing.T, path string) *os.File {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(handle), path)
}

func TestWindowsETagReplacementFailureRetainsOldValidator(t *testing.T) {
	p := newPublisher(t)
	apply(t, p, collection(revision1, nil), `"old"`)
	file := holdConfiguration(t, filepath.Join(p.configDir, "dscregsync", "machine.etag"))
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := p.Apply(ctx, collection(revision2, nil), `"new"`); err == nil {
		t.Fatal("replaced protected ETag")
	}
	tagIs(t, p, `"old"`)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	apply(t, p, collection(revision2, nil), `"new"`)
	tagIs(t, p, `"new"`)
}

func TestWindowsAccessDeniedIsNotSharingContention(t *testing.T) {
	if retryable(syscall.ERROR_ACCESS_DENIED) {
		t.Fatal("permanent ACL failure retried as sharing violation")
	}
}

func TestWindowsPrivateACLs(t *testing.T) {
	p := newPublisher(t)
	apply(t, p, collection(revision1, []byte(`{"private":true}`)), `"one"`)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"S-1-5-18": true, "S-1-5-32-544": true, user.User.Sid.String(): true}
	names := []string{"."}
	for name := range files(t, p.configDir) {
		names = append(names, name)
	}
	for _, name := range names {
		file, err := os.Open(filepath.Join(p.configDir, "dscregsync", name))
		if err != nil {
			t.Fatal(err)
		}
		sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatal("managed ACL permits public inheritance")
		}
		dacl, _, err := sd.DACL()
		if err != nil || dacl == nil || dacl.AceCount != 3 {
			t.Fatal("missing private DACL")
		}
		for i := uint16(0); i < dacl.AceCount; i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
				t.Fatal(err)
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !allowed[sid.String()] {
				t.Fatal("managed file accessible to unexpected principal")
			}
		}
	}
}
