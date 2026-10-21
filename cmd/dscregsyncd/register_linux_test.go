package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func requireRegistrationPrivilege(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root; privileged CI runs onboarding tests")
	}
}

func TestRegistrationCommandProcess(t *testing.T) {
	if os.Getenv("DSCREGSYNC_TEST_REGISTER_PROCESS") != "1" {
		t.Skip("registration command subprocess")
	}
	os.Args = append(os.Args[:1], os.Args[3:]...)
	main()
}

func TestRegistrationSignalCleanup(t *testing.T) {
	requireRegistrationPrivilege(t)
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		for _, phase := range []string{"stdin", "terminal", "request"} {
			t.Run(sig.String()+"/"+phase, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "private", "service.json")
				requested := make(chan struct{})
				release := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					close(requested)
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				defer server.Close()
				defer close(release)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRegistrationCommandProcess$", "--",
					"register", "foobar", "--server", server.URL, "--config", path)
				cmd.Env = append(os.Environ(), "DSCREGSYNC_TEST_REGISTER_PROCESS=1")
				var output bytes.Buffer
				cmd.Stdout, cmd.Stderr = &output, &output
				var terminal *os.File
				var original *unix.Termios
				if phase == "terminal" {
					terminal = registrationTerminal(t)
					var err error
					original, err = unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
					if err != nil {
						t.Fatal(err)
					}
					cmd.Stdin = terminal
				} else {
					cmd.Args = append(cmd.Args, "--token-stdin")
					if phase == "stdin" {
						input, writer, err := os.Pipe()
						if err != nil {
							t.Fatal(err)
						}
						defer input.Close()
						defer writer.Close()
						cmd.Stdin = input
					} else {
						cmd.Stdin = strings.NewReader("dscr_test-token\n")
					}
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				var waitErr error
				go func() {
					waitErr = cmd.Wait()
					close(done)
				}()
				defer func() {
					cancel()
					<-done
				}()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for ready := false; !ready; {
					select {
					case <-done:
						t.Fatalf("registration exited before signal: %v", waitErr)
					case <-ctx.Done():
						t.Fatal("registration did not reach interruption point")
					case <-ticker.C:
						switch phase {
						case "stdin":
							staged, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".dscregsync-register-*.tmp"))
							if err != nil {
								t.Fatal(err)
							}
							ready = len(staged) == 2
						case "terminal":
							mode, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
							if err != nil {
								t.Fatal(err)
							}
							ready = mode.Lflag&unix.ECHO == 0
						case "request":
							select {
							case <-requested:
								ready = true
							default:
							}
						}
					}
				}
				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("registration did not exit promptly on signal")
				}
				var exit *exec.ExitError
				if !errors.As(waitErr, &exit) || exit.ExitCode() != 1 {
					t.Fatalf("registration did not return its handled failure status: %v", waitErr)
				}
				if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("interrupted registration left staging files or its private directory")
				}
				if strings.Contains(output.String(), "dscr_test-token") || strings.Contains(output.String(), "successfully") {
					t.Fatal("interrupted registration leaked a token or claimed success")
				}
				if terminal != nil {
					mode, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
					if err != nil || *mode != *original {
						t.Fatal("interrupted registration did not restore terminal settings")
					}
				}
			})
		}
	}
}

func registrationTerminal(t *testing.T) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminal.Close() })
	return terminal
}
