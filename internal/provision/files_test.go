package provision

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-registry-sync/internal/config"
)

const testKey = "dscm_testid.testsecret"

func setup(t *testing.T) (string, config.Config) {
	t.Helper()
	if err := privileged(); err != nil {
		t.Skip("native credential provisioning requires root/elevation; privileged CI runs this coverage")
	}
	path := filepath.Join(t.TempDir(), "private", "dscregsyncd.json")
	cfg := config.Defaults()
	cfg.RegistryURL = "https://registry.example"
	cfg.APIKeyFile = filepath.Join(filepath.Dir(path), "dscregsyncd.key")
	return path, cfg
}

func assertNoStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".dscregsync-register-") {
			t.Fatal("attempt left staging files behind")
		}
	}
}

func TestProvisionSuccessfulAndSafeRerun(t *testing.T) {
	path, cfg := setup(t)
	files, err := Prepare(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preflight published configuration")
	}
	checkPermissions(t, filepath.Join(filepath.Dir(path), files.config.name), filepath.Join(filepath.Dir(path), files.key.name))
	if err := files.Publish(context.Background(), testKey); err != nil {
		t.Fatal(err)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil || loaded != cfg {
		t.Fatal("published configuration is not daemon-consumable")
	}
	if key, err := config.ReadAPIKey(cfg.APIKeyFile); err != nil || key != testKey {
		t.Fatal("published credential is not daemon-consumable")
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), testKey) {
		t.Fatal("JSON contains machine credential")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 6 {
		t.Fatal("unexpected persisted configuration model")
	}
	if _, err := Prepare(path, cfg); err == nil {
		t.Fatal("rerun replaced configured machine")
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != string(data) {
		t.Fatal("rerun changed configuration")
	}
	assertNoStaging(t, filepath.Dir(path))
	checkPermissions(t, path, cfg.APIKeyFile)
}

func TestProvisionExistingDestinations(t *testing.T) {
	for _, keyExists := range []bool{false, true} {
		path, cfg := setup(t)
		prepared, err := Prepare(path, cfg)
		if err != nil {
			t.Fatal(err)
		}
		// Preserve a protected directory, without installing either file.
		prepared.committed = true
		if err := prepared.Close(); err != nil {
			t.Fatal(err)
		}
		target := path
		if keyExists {
			target = cfg.APIKeyFile
		}
		if err := os.WriteFile(target, []byte("existing"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Prepare(path, cfg); err == nil {
			t.Fatal("existing destination was accepted")
		}
		data, _ := os.ReadFile(target)
		if string(data) != "existing" {
			t.Fatal("existing destination was modified")
		}
		assertNoStaging(t, filepath.Dir(path))
	}
}

func TestProvisionFailureCleanup(t *testing.T) {
	for _, failure := range []string{"abandoned", "key-write", "config-collision", "key-collision", "canceled", "invalid-key"} {
		t.Run(failure, func(t *testing.T) {
			path, cfg := setup(t)
			files, err := Prepare(path, cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			key := testKey
			collision := ""
			switch failure {
			case "key-write":
				files.key.file.Close()
			case "config-collision":
				collision = path
			case "key-collision":
				collision = cfg.APIKeyFile
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "invalid-key":
				key = "invalid"
			}
			if collision != "" {
				if err := os.WriteFile(collision, []byte("concurrent"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if failure != "abandoned" && files.Publish(ctx, key) == nil {
				t.Fatal("failed provisioning succeeded")
			}
			if failure == "canceled" {
				data, err := files.root().ReadFile(files.key.name)
				if err != nil || string(data) != testKey {
					t.Fatal("cancellation test did not stage a credential before cleanup")
				}
			}
			// Closed handles and nonempty concurrent directories can make cleanup
			// report an error; it must still clean every owned file it can.
			closeErr := files.Close()
			if closeErr != nil && strings.Contains(closeErr.Error(), testKey) {
				t.Fatal("cleanup error exposed credential")
			}
			for _, target := range []string{path, cfg.APIKeyFile} {
				if target == collision {
					data, _ := os.ReadFile(target)
					if string(data) != "concurrent" {
						t.Fatal("cleanup removed another writer's file")
					}
				} else if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed provisioning left a published file")
				}
			}
			assertNoStaging(t, filepath.Dir(path))
		})
	}
}

func TestProvisionRejectsSymlinkAndDirectoryTargets(t *testing.T) {
	for _, targetType := range []string{"ancestor", "config", "key", "directory"} {
		t.Run(targetType, func(t *testing.T) {
			path, cfg := setup(t)
			outside := t.TempDir()
			if targetType == "ancestor" {
				if err := os.Symlink(outside, filepath.Dir(path)); err != nil {
					t.Skip("symlink creation unavailable")
				}
			} else {
				files, err := Prepare(path, cfg)
				if err != nil {
					t.Fatal(err)
				}
				files.committed = true
				if err := files.Close(); err != nil {
					t.Fatal(err)
				}
				target := path
				if targetType == "key" {
					target = cfg.APIKeyFile
				}
				if targetType == "directory" {
					if err := os.Mkdir(target, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(filepath.Join(outside, "untouched"), target); err != nil {
					t.Skip("symlink creation unavailable")
				}
			}
			if _, err := Prepare(path, cfg); err == nil {
				t.Fatal("unsafe filesystem destination accepted")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatal("provisioning followed an unsafe redirect")
			}
		})
	}
}

func TestProvisionDetectsAncestorReplacement(t *testing.T) {
	path, cfg := setup(t)
	files, err := Prepare(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(path)
	moved := parent + "-moved"
	if err := os.Rename(parent, moved); err != nil {
		if closeErr := files.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if replacementBlocked(err) {
			return
		}
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if files.Publish(context.Background(), testKey) == nil {
		t.Fatal("published after ancestor replacement")
	}
	files.Close()
	assertNoStaging(t, moved)
	for _, dir := range []string{moved, parent} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatal("ancestor replacement caused publication or unsafe cleanup")
		}
	}
}

func TestProvisionCleanupPreservesReplacement(t *testing.T) {
	path, cfg := setup(t)
	files, err := Prepare(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.root().Link(files.key.name, files.key.target); err != nil {
		t.Fatal(err)
	}
	files.key.published = true
	if err := os.Remove(cfg.APIKeyFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.APIKeyFile, []byte("another writer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := files.Close(); err == nil {
		t.Fatal("changed cleanup identity was not reported")
	}
	data, err := os.ReadFile(cfg.APIKeyFile)
	if err != nil || string(data) != "another writer" {
		t.Fatal("cleanup removed a concurrent replacement")
	}
	assertNoStaging(t, filepath.Dir(path))
}

func TestProvisionPrivilegeRequired(t *testing.T) {
	if privileged() == nil {
		t.Skip("requires an unprivileged process")
	}
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.APIKeyFile = filepath.Join(root, "dscregsyncd.key")
	_, err := Prepare(filepath.Join(root, "dscregsyncd.json"), cfg)
	if err == nil {
		t.Fatal("unprivileged provisioning accepted")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("unprivileged provisioning created files")
	}
}
