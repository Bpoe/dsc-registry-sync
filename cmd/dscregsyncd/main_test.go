package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/config"
	"github.com/Bpoe/dsc-registry-sync/internal/registry"
)

func TestPollNeverPromotesETag(t *testing.T) {
	for _, committed := range []string{"", `W/"committed"`} {
		key := "dscm_" + rand.Text() + "." + rand.Text()
		secret := rand.Text()
		statuses := []int{200, 429, 503, 401, 200, 200}
		if committed != "" {
			statuses[0] = 304
		}
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("If-None-Match") != committed {
				t.Error("uncommitted validator was used")
			}
			if r.Header.Get("Authorization") != "ApiKey "+key {
				t.Error("missing machine authentication")
			}
			status := statuses[requests]
			requests++
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"uncommitted"`)
			w.Header().Set("Cache-Control", "private, max-age=60")
			if status == 429 {
				w.Header().Set("Retry-After", "120")
			}
			w.WriteHeader(status)
			if status == 200 && requests != 5 {
				io.WriteString(w, `{"configurationRevision":0,"configurations":[],"opaque":"`+secret+`"}`)
			} else {
				io.WriteString(w, secret)
			}
		}))
		client := registry.NewClient(server.URL, key, time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		var logs bytes.Buffer
		var delays []time.Duration
		poll(ctx, client, committed, 300*time.Second, slog.New(slog.NewJSONHandler(&logs, nil)),
			func(ctx context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				if len(delays) == len(statuses) {
					cancel()
					return ctx.Err()
				}
				return nil
			})
		cancel()
		client.Close()
		server.Close()
		if requests != len(statuses) {
			t.Fatal("poller did not continue through transient and invalid responses")
		}
		if delays[0] < time.Minute || delays[1] < 2*time.Minute ||
			delays[2] < 10*time.Second || delays[3] < 20*time.Second ||
			delays[4] < 40*time.Second || delays[5] < time.Minute {
			t.Fatal("poller did not honor cache/retry/recovery scheduling")
		}
		if strings.Contains(logs.String(), key) || strings.Contains(logs.String(), secret) ||
			strings.Contains(logs.String(), "uncommitted") {
			t.Fatal("poll logs leaked response or credential content")
		}
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			if !json.Valid([]byte(line)) {
				t.Fatal("logs are not structured JSON")
			}
		}
	}
}

func TestForegroundReadOnlyAndShutdown(t *testing.T) {
	for _, cached := range []bool{false, true} {
		root := t.TempDir()
		cfg := config.Defaults()
		cfg.ConfigDir = filepath.Join(root, "config.d")
		cfg.ResultsDir = filepath.Join(root, "results.d")
		cfg.APIKeyFile = filepath.Join(root, "key")
		cfg.AllowInsecureLoopback = true
		key := "dscm_" + rand.Text() + "." + rand.Text()
		if err := os.WriteFile(cfg.APIKeyFile, []byte(key), 0600); err != nil {
			t.Fatal(err)
		}
		if cached {
			for path, content := range map[string]string{
				filepath.Join(cfg.ConfigDir, "dscregsync", "machine.etag"):             "W/\"committed\"\n",
				filepath.Join(cfg.ConfigDir, "dscregsync", "docker.json"):              "{}",
				filepath.Join(cfg.ConfigDir, "dscregsync", "docker.parameters.json"):   "{}",
				filepath.Join(cfg.ConfigDir, "dscregsync", ".docker.revision"):         "installed",
				filepath.Join(cfg.ConfigDir, "local", "owned.json"):                    "{}",
				filepath.Join(cfg.ResultsDir, "dscregsync", "docker.json.result.json"): "{}",
			} {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		requested := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requested = true
			if r.Header.Get("Authorization") != "ApiKey "+key ||
				(r.Header.Get("If-None-Match") == `W/"committed"`) != cached {
				t.Error("startup did not load the credential and committed validator")
			}
			cancel()
			w.WriteHeader(503)
		}))
		cfg.RegistryURL = server.URL
		data, _ := json.Marshal(cfg)
		path := filepath.Join(root, "service.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, root)
		var logs bytes.Buffer
		code := run(ctx, []string{"-config", path}, &logs)
		cancel()
		server.Close()
		if code != 0 || !requested || !strings.Contains(logs.String(), "daemon stopped") {
			t.Fatal("foreground lifecycle did not start and cancel cleanly")
		}
		if !reflect.DeepEqual(before, snapshot(t, root)) {
			t.Fatal("read-only daemon changed input, results or cache files")
		}
		if strings.Contains(logs.String(), key) {
			t.Fatal("lifecycle logs leaked credentials")
		}
	}
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			files[path] = "<directory>"
			return nil
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestCLIAndStartupFailuresArePrivate(t *testing.T) {
	secret := rand.Text()
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"-help"}, 0},
		{[]string{"-unknown=" + secret}, 2},
		{[]string{secret}, 2},
		{[]string{"-config", filepath.Join(t.TempDir(), secret)}, 1},
	} {
		var logs bytes.Buffer
		if code := run(context.Background(), test.args, &logs); code != test.code {
			t.Errorf("exit code %d, want %d", code, test.code)
		}
		if strings.Contains(logs.String(), secret) {
			t.Fatal("startup log echoed potentially sensitive input")
		}
	}
}

func TestWaitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := wait(ctx, time.Hour); err != context.Canceled {
		t.Fatal("wait did not stop on cancellation")
	}
	if err := wait(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}
