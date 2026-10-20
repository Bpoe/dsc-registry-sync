//go:build linux

package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func holdConfiguration(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		t.Fatal(err)
	}
	return file
}

func TestLinuxStaleInodeCannotDeleteReplacement(t *testing.T) {
	p := newPublisher(t)
	apply(t, p, collection(revision1, nil), `"old"`)
	dir, err := openDirectory(p.configDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.close()
	path := filepath.Join(p.configDir, "dscregsync", "docker.json")
	old := holdConfiguration(t, path)
	defer old.Close()
	if err := dir.root.Rename("docker.json", ".old-inode"); err != nil {
		t.Fatal(err)
	}
	put(t, p.configDir, "docker.json", "new protected inode")
	current := holdConfiguration(t, path)
	defer current.Close()
	if err := dir.removeOpenedConfiguration(old, "docker.json"); !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("stale inode was not rejected: %v", err)
	}
	if contents(t, p.configDir, "docker.json") != "new protected inode" {
		t.Fatal("replacement deleted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := dir.removeConfiguration(ctx, "docker.json"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("replacement's lock was bypassed")
	}
}

func TestLinuxFIFORejectedWithoutBlocking(t *testing.T) {
	p := newPublisher(t)
	put(t, p.configDir, "machine.etag", `"old"`)
	if err := syscall.Mkfifo(filepath.Join(p.configDir, "dscregsync", "docker.json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background(), collection(revision1, nil), `"new"`); err == nil {
		t.Fatal("accepted FIFO")
	}
	tagIs(t, p, `"old"`)
}
