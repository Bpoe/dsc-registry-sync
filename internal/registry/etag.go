package registry

import (
	"errors"
	"io"
	"os"
	"strings"
)

const maxETagBytes = 4096

// ValidETag reports whether a single bounded HTTP validator can be committed.
func ValidETag(tag string) bool { return validETag(tag) }

func validETag(tag string) bool {
	if len(tag) > maxETagBytes {
		return false
	}
	value := strings.TrimPrefix(tag, "W/")
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return false
	}
	for _, c := range []byte(value[1 : len(value)-1]) {
		if c < 0x21 || c == '"' || c == 0x7f {
			return false
		}
	}
	return true
}

// ReadETag reads the committed validator without creating directories or files.
// The administrator-configured root is trusted; managed entries must not be
// symlinks. Root-relative opens also prevent substitution from escaping it.
func ReadETag(configDir string) (string, error) {
	root, err := os.OpenRoot(configDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("cannot open configDir to read machine.etag")
	}
	defer root.Close()
	info, err := root.Lstat("dscregsync")
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("managed input directory must be a regular directory")
	}
	managed, err := root.OpenRoot("dscregsync")
	if err != nil {
		return "", errors.New("cannot open managed input directory")
	}
	defer managed.Close()
	opened, err := managed.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("managed input directory changed while opening")
	}
	info, err = managed.Lstat("machine.etag")
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("machine.etag must be a regular file")
	}
	f, err := managed.Open("machine.etag")
	if err != nil {
		return "", errors.New("cannot open machine.etag")
	}
	defer f.Close()
	opened, err = f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("machine.etag changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxETagBytes+3))
	if err != nil {
		return "", errors.New("cannot read machine.etag")
	}
	tag := strings.TrimSuffix(string(data), "\n")
	if len(tag) != len(data) {
		tag = strings.TrimSuffix(tag, "\r")
	}
	if !validETag(tag) {
		return "", errors.New("machine.etag contains an invalid or oversized HTTP validator")
	}
	return tag, nil
}
