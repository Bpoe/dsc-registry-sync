package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/config"
)

func TestRegisterCLIParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.json")
	for _, args := range [][]string{
		{"foobar", "--server", "https://localhost"},
		{"foobar", "--server=https://localhost", "--config", path, "--token-stdin"},
		{"--server", "https://localhost", "foobar"},
		{"foobar", "--server", "http://localhost"},
		{"foobar", "--server", "http://192.168.1.10:8080"},
		{"foobar", "--server", "http://registry.internal"},
	} {
		options, err := parseRegister(args)
		if err != nil || options.name != "foobar" || options.server == "" || options.path == "" {
			t.Fatal("registration command was not parsed")
		}
	}
	secret := rand.Text()
	for _, args := range [][]string{
		{}, {"foobar"}, {"--server", "https://localhost"}, {"foobar", "--server"},
		{"", "--server", "https://localhost"},
		{"foobar", "--server", "https://localhost", secret},
		{"foobar", "--server", "https://localhost", "--token", secret},
		{"foobar", "--server", "https://localhost", "--token=" + secret},
		{"foobar", "--server", "http://localhost", "--allow-insecure-loopback"},
		{"foobar", "--server", "http://localhost", "--allow-insecure-loopback=false"},
	} {
		var output bytes.Buffer
		if code := runRegister(context.Background(), args, strings.NewReader(""), &output, &output, nil); code != 2 ||
			strings.Contains(output.String(), secret) {
			t.Fatal("invalid registration command did not fail privately")
		}
	}
	for _, args := range [][]string{{"--help"}, {"foobar", "--help"}} {
		var output bytes.Buffer
		if runRegister(context.Background(), args, nil, &output, &output, nil) != 0 ||
			!strings.Contains(output.String(), "--token-stdin") ||
			strings.Contains(output.String(), "--allow-insecure-loopback") {
			t.Fatal("registration help missing")
		}
	}
	var help bytes.Buffer
	if run(context.Background(), []string{"-help"}, &help) != 0 ||
		!strings.Contains(help.String(), "register <machine-name> --server") {
		t.Fatal("daemon help does not describe registration")
	}
}

func TestRegistrationTokenInput(t *testing.T) {
	token := "dscr_" + rand.Text()
	for _, suffix := range []string{"", "\n", "\r\n"} {
		var prompt bytes.Buffer
		got, err := readToken(context.Background(), strings.NewReader(token+suffix+"ignored\n"), &prompt, true, nil)
		if suffix == "" {
			got, err = readToken(context.Background(), strings.NewReader(token), &prompt, true, nil)
		}
		if err != nil || got != token || prompt.Len() != 0 {
			t.Fatal("stdin token was not read as one unprompted line")
		}
	}
	var prompt bytes.Buffer
	password := []byte(token)
	called := false
	got, err := readToken(context.Background(), strings.NewReader(""), &prompt, false, func(context.Context, io.Reader) ([]byte, error) {
		called = true
		return password, nil
	})
	if err != nil || got != token || !called || prompt.String() != "Registration token: \n" {
		t.Fatal("interactive token input did not use the no-echo reader")
	}
	if strings.Trim(string(password), "\x00") != "" {
		t.Fatal("terminal token buffer was not cleared")
	}
	for _, input := range []string{"", "\n", "\r\n", "  \n", token + " \n", token + "\x00\n", strings.Repeat("x", 8193)} {
		got, err := readToken(context.Background(), strings.NewReader(input), io.Discard, true, nil)
		if err == nil || got != "" || strings.Contains(err.Error(), token) {
			t.Fatal("invalid token input did not fail privately")
		}
	}
	_, err = readToken(context.Background(), strings.NewReader(""), &prompt, false, func(context.Context, io.Reader) ([]byte, error) {
		return nil, errors.New(token)
	})
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(prompt.String(), token) {
		t.Fatal("terminal input error leaked a token")
	}
	if _, err := terminalToken(context.Background(), strings.NewReader(token)); err == nil {
		t.Fatal("interactive input accepted a nonterminal")
	}
}

func TestRegistrationTokenCancellation(t *testing.T) {
	for _, stdin := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal", true: "stdin"}[stdin], func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := readToken(ctx, reader, io.Discard, stdin, readTokenLine)
				done <- err
			}()
			// An incomplete line proves the input read started and is blocked.
			if _, err := writer.Write([]byte("dscr_partial")); err != nil {
				t.Fatal(err)
			}
			cancel()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "canceled") {
					t.Fatal("blocked token input did not report cancellation")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation waited for token input")
			}
		})
	}
}

func TestRegistrationEndToEndAndPreflight(t *testing.T) {
	requireRegistrationPrivilege(t)
	for _, scenario := range []string{"success", "existing-config", "existing-key", "expired", "duplicate", "server", "local-failure", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(t.TempDir(), "private", "custom.json")
			token, key := "dscr_"+rand.Text(), "dscm_"+rand.Text()+"."+rand.Text()
			id := "550e8400-e29b-41d4-a716-446655440000"
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("registration bearer authentication missing")
				}
				if scenario == "canceled" {
					io.Copy(io.Discard, r.Body)
					cancel()
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				status := map[string]int{"expired": 401, "duplicate": 409, "server": 503}[scenario]
				if status != 0 {
					w.WriteHeader(status)
					io.WriteString(w, token+key)
					return
				}
				if scenario == "local-failure" {
					if err := os.WriteFile(path, []byte("concurrent config"), 0600); err != nil {
						t.Error(err)
					}
				}
				w.WriteHeader(201)
				json.NewEncoder(w).Encode(map[string]string{"id": id, "name": "foobar", "apiKey": key})
			}))
			defer server.Close()
			var output, diagnostics bytes.Buffer
			args := []string{"foobar", "--server", server.URL, "--config", path, "--token-stdin"}
			// Preseed a valid installation using the same protected provisioning
			// path, then ensure either existing destination blocks before HTTP.
			if strings.HasPrefix(scenario, "existing-") {
				if runRegister(context.Background(), args, strings.NewReader(token), &output, &diagnostics, nil) != 0 {
					t.Fatal("cannot seed existing installation")
				}
				target := path
				if scenario == "existing-config" {
					target = filepath.Join(filepath.Dir(path), "dscregsyncd.key")
				}
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				requests = 0
				output.Reset()
				diagnostics.Reset()
			}
			code := runRegister(ctx, args, strings.NewReader(token+"\n"), &output, &diagnostics, nil)
			if strings.Contains(output.String()+diagnostics.String(), token) ||
				strings.Contains(output.String()+diagnostics.String(), key) {
				t.Fatal("registration output leaked credentials")
			}
			keyPath := filepath.Join(filepath.Dir(path), "dscregsyncd.key")
			if scenario == "success" {
				if code != 0 || requests != 1 ||
					diagnostics.String() != "WARNING: HTTP connections are not encrypted. Registration tokens\nand machine API keys may be exposed in transit.\n\n" {
					t.Fatal("registration did not complete")
				}
				if !strings.Contains(output.String(), "Machine registered successfully.\nName: foobar\nID: "+id) ||
					!strings.Contains(output.String(), "Configuration: "+path) {
					t.Fatal("missing safe registration confirmation")
				}
				cfg, err := config.Load(path)
				if err != nil || cfg.RegistryURL != server.URL || cfg.APIKeyFile != keyPath {
					t.Fatal("generated configuration cannot be loaded by daemon")
				}
				if got, err := config.ReadAPIKey(keyPath); err != nil || got != key {
					t.Fatal("daemon cannot consume generated key")
				}
			} else {
				if code == 0 || output.Len() != 0 || strings.Contains(diagnostics.String(), "successfully") {
					t.Fatal("failed registration claimed success")
				}
				if strings.HasPrefix(scenario, "existing-") {
					if requests != 0 {
						t.Fatal("local preflight failure contacted Registry")
					}
				} else {
					if requests != 1 {
						t.Fatal("registration was retried")
					}
					if _, err := os.Lstat(keyPath); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("failed registration left a credential behind")
					}
				}
				if scenario == "local-failure" && !strings.Contains(diagnostics.String(), "administrator-assisted recovery") {
					t.Fatal("lost local credential did not explain recovery")
				}
				if scenario == "canceled" {
					if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("canceled registration left staging files or its private directory")
					}
				}
			}
		})
	}
}

func TestRegistrationTransportWarning(t *testing.T) {
	requireRegistrationPrivilege(t)
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			for _, stdin := range []bool{false, true} {
				t.Run(map[bool]string{false: "terminal", true: "stdin"}[stdin], func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "private", "service.json")
					token, key := "dscr_"+rand.Text(), "dscm_"+rand.Text()+"."+rand.Text()
					requests := 0
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests++
						if r.Method != "POST" || r.URL.Path != "/v1/agent/register" ||
							r.Header.Get("Authorization") != "Bearer "+token {
							t.Error("incorrect registration request")
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(201)
						json.NewEncoder(w).Encode(map[string]string{
							"id": "550e8400-e29b-41d4-a716-446655440000", "name": "foobar", "apiKey": key,
						})
					})
					server := httptest.NewUnstartedServer(handler)
					if scheme == "https" {
						server.StartTLS()
						original := http.DefaultTransport
						http.DefaultTransport = server.Client().Transport
						defer func() { http.DefaultTransport = original }()
					} else {
						server.Start()
					}
					defer server.Close()
					var output, diagnostics bytes.Buffer
					args := []string{"foobar", "--server", server.URL, "--config", path}
					warning := ""
					if scheme == "http" {
						warning = "WARNING: HTTP connections are not encrypted. Registration tokens\nand machine API keys may be exposed in transit.\n\n"
					}
					wantDiagnostics := warning
					if stdin {
						args = append(args, "--token-stdin")
					} else {
						wantDiagnostics += "Registration token: \n"
					}
					readPassword := func(context.Context, io.Reader) ([]byte, error) {
						if diagnostics.String() != warning+"Registration token: " {
							t.Error("transport warning was not emitted before requesting the token")
						}
						return []byte(token), nil
					}
					code := runRegister(context.Background(), args, strings.NewReader(token+"\n"), &output, &diagnostics, readPassword)
					if code != 0 || requests != 1 || diagnostics.String() != wantDiagnostics {
						t.Fatal("registration did not succeed with exactly the expected transport warning and prompt")
					}
					if strings.Contains(output.String()+diagnostics.String(), token) ||
						strings.Contains(output.String()+diagnostics.String(), key) {
						t.Fatal("registration transport warning or output leaked credentials")
					}
					cfg, err := config.Load(path)
					if err != nil || cfg.RegistryURL != server.URL {
						t.Fatal("registration did not persist the selected transport")
					}
					data, err := os.ReadFile(path)
					if err != nil || bytes.Contains(data, []byte("allowInsecureLoopback")) {
						t.Fatal("generated configuration includes the obsolete transport setting")
					}
					if got, err := config.ReadAPIKey(cfg.APIKeyFile); err != nil || got != key {
						t.Fatal("registration did not provision the machine key")
					}
				})
			}
		})
	}
}

func TestRegistrationRejectsOriginBeforeProvisioning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "config.json")
	var output bytes.Buffer
	code := runRegister(context.Background(), []string{"foobar", "--server", "ftp://localhost", "--config", path},
		strings.NewReader(""), &output, &output, nil)
	if code == 0 || !strings.Contains(output.String(), "HTTPS") {
		t.Fatal("unsupported origin accepted")
	}
	if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid server created local state")
	}
}
