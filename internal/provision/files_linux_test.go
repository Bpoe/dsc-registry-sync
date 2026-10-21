package provision

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func replacementBlocked(err error) bool { return false }

func checkPermissions(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != 0 {
			t.Fatal("credential/config is not root-owned mode 0600")
		}
		info, err = os.Stat(filepath.Dir(path))
		if err != nil || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != 0 {
			t.Fatal("new parent directory is not root-owned mode 0700")
		}
	}
}

func TestProvisionRejectsUnprotectedLinuxParents(t *testing.T) {
	for _, owner := range []bool{false, true} {
		path, cfg := setup(t)
		if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if owner {
			if err := os.Chown(filepath.Dir(path), 65534, 65534); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Chmod(filepath.Dir(path), 0777); err != nil {
			t.Fatal(err)
		}
		if _, err := Prepare(path, cfg); err == nil {
			t.Fatal("unsafe ownership/permissions accepted")
		}
		assertNoStaging(t, filepath.Dir(path))
	}
}
