package publish

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/registry"
)

func TestCrashHelper(t *testing.T) {
	if os.Getenv("DSCREGSYNC_CRASH_HELPER") != "1" {
		return
	}
	p := New(os.Getenv("DSCREGSYNC_CRASH_CONFIG"), os.Getenv("DSCREGSYNC_CRASH_RESULTS"),
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
	p.after = func(name, step string) error {
		if step == os.Getenv("DSCREGSYNC_CRASH_STEP") {
			os.Exit(23) // Deliberately bypass deferred staging cleanup.
		}
		return nil
	}
	if err := p.Apply(context.Background(), collection(revision2, []byte(`{"new":true}`)), `"new"`); err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash point was not reached")
}

func TestAbruptProcessExitRecovery(t *testing.T) {
	for _, step := range []string{"stage-config", "stage-parameters", "stage-revision",
		"remove-config", "remove-parameters", "remove-result", "publish-parameters",
		"publish-revision", "publish-config", "stage-etag"} {
		t.Run(step, func(t *testing.T) {
			p := newPublisher(t)
			apply(t, p, collection(revision1, []byte(`{"old":true}`)), `"old"`)
			put(t, p.resultsDir, "docker.json.result.json", "old result")
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$")
			cmd.Env = append(os.Environ(), "DSCREGSYNC_CRASH_HELPER=1", "DSCREGSYNC_CRASH_STEP="+step,
				"DSCREGSYNC_CRASH_CONFIG="+p.configDir, "DSCREGSYNC_CRASH_RESULTS="+p.resultsDir)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("helper did not crash at boundary: %v: %s", err, output)
			}
			tagIs(t, p, `"old"`)
			p = New(p.configDir, p.resultsDir, p.log)
			apply(t, p, collection(revision2, []byte(`{"new":true}`)), `"new"`)
			tagIs(t, p, `"new"`)
			if len(files(t, p.configDir)) != 4 ||
				contents(t, p.configDir, ".docker.revision") != revision2 ||
				contents(t, p.configDir, "docker.parameters.json") != `{"new":true}` {
				t.Fatal("restart did not clean staging and complete publication")
			}
			absent(t, p.resultsDir, "docker.json.result.json")
		})
	}
}

// This child implements only the upstream file handshake, not DSC execution.
func TestProtectionHelper(t *testing.T) {
	if os.Getenv("DSCREGSYNC_LOCK_HELPER") != "1" {
		return
	}
	path := os.Getenv("DSCREGSYNC_LOCK_INPUT")
	file := holdConfiguration(t, path)
	fmt.Println("protected")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	result := os.Getenv("DSCREGSYNC_LOCK_RESULT")
	if err := os.MkdirAll(filepath.Dir(result), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result, []byte("last protected result"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedConfigurationAcrossProcesses(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "update", true: "remove"}[remove], func(t *testing.T) {
			p := newPublisher(t)
			apply(t, p, collection(revision1, []byte(`{"old":true}`)), `"old"`)
			path := filepath.Join(p.configDir, "dscregsync", "docker.json")
			cmd := exec.Command(os.Args[0], "-test.run=^TestProtectionHelper$")
			cmd.Env = append(os.Environ(), "DSCREGSYNC_LOCK_HELPER=1",
				"DSCREGSYNC_LOCK_INPUT="+path,
				"DSCREGSYNC_LOCK_RESULT="+filepath.Join(p.resultsDir, "dscregsync", "docker.json.result.json"))
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				stdin.Close()
				if !waited {
					cmd.Wait()
				}
			}()
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || line != "protected\n" {
				t.Fatalf("helper did not protect input: %q, %v", line, err)
			}
			c := collection(revision2, nil)
			if remove {
				c = &registry.Collection{Configurations: []registry.Configuration{}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			err = p.Apply(ctx, c, `"new"`)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("protection did not block/cancel: %v", err)
			}
			tagIs(t, p, `"old"`)
			if contents(t, p.configDir, ".docker.revision") != revision1 ||
				contents(t, p.configDir, "docker.parameters.json") != `{"old":true}` {
				t.Fatal("modified protected unit")
			}
			done := make(chan error, 1)
			go func() { done <- p.Apply(context.Background(), c, `"new"`) }()
			select {
			case err := <-done:
				t.Fatalf("did not wait for protection: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			stdin.Close()
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			waited = true
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("publisher did not resume after protection released")
			}
			tagIs(t, p, `"new"`)
			absent(t, p.resultsDir, "docker.json.result.json")
			if remove {
				absent(t, p.configDir, "docker.json")
			}
		})
	}
}
