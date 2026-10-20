package registry

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadETagMissing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	for _, path := range []string{root, t.TempDir()} {
		if tag, err := ReadETag(path); err != nil || tag != "" {
			t.Fatal("missing cache must mean unconditional retrieval")
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("reader created the config directory")
	}
}

func TestReadETag(t *testing.T) {
	for _, test := range []struct {
		data string
		want string
	}{
		{`"machine-config-7"`, `"machine-config-7"`},
		{"W/\"opaque tag\"\n", ""},
		{"W/\"opaque\"\r\n", `W/"opaque"`},
		{"\"opaque\"\n", `"opaque"`},
		{`""`, `""`},
		{"", ""}, {"7", ""}, {" \"tag\"", ""}, {"\"tag\" ", ""},
		{"\"tag\"\r", ""}, {"\"tag\"\n\n", ""}, {"\"one\"\n\"two\"", ""},
		{`"a","b"`, ""}, {`*`, ""}, {"\"a\x7fb\"", ""},
		{`"` + strings.Repeat("x", maxETagBytes-2) + `"`, `"` + strings.Repeat("x", maxETagBytes-2) + `"`},
		{`"` + strings.Repeat("x", maxETagBytes) + `"`, ""},
	} {
		root := t.TempDir()
		dir := filepath.Join(root, "dscregsync")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "machine.etag")
		if err := os.WriteFile(file, []byte(test.data), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadETag(root)
		if test.want == "" {
			if err == nil {
				t.Error("accepted invalid ETag")
			}
		} else if err != nil || got != test.want {
			t.Error("ETag bytes were not preserved")
		}
		after, err := os.ReadFile(file)
		if err != nil || string(after) != test.data {
			t.Fatal("reader changed ETag file")
		}
	}
}

func TestReadETagRejectsNonregularEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dscregsync"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadETag(root); err == nil {
		t.Error("accepted file as managed directory")
	}
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dscregsync", "machine.etag"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadETag(root); err == nil {
		t.Error("accepted directory as ETag")
	}
}

func TestReadETagRejectsSymlinks(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "directory"}[directory], func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			target := filepath.Join(outside, "machine.etag")
			if err := os.WriteFile(target, []byte(`"outside"`), 0600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "dscregsync")
			if directory {
				target = outside
			} else {
				if err := os.Mkdir(link, 0700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(link, "machine.etag")
			}
			if err := os.Symlink(target, link); err != nil {
				if runtime.GOOS != "windows" {
					t.Fatal(err)
				}
				t.Skip("symlink creation unavailable:", err)
			}
			if _, err := ReadETag(root); err == nil {
				t.Fatal("followed managed symlink")
			}
		})
	}
}
