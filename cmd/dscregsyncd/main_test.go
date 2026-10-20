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
	"github.com/Bpoe/dsc-registry-sync/internal/publish"
	"github.com/Bpoe/dsc-registry-sync/internal/registry"
)

func TestPollCommitsOnlyAppliedCollections(t *testing.T) {
	for _, committed := range []string{"", `W/"committed"`} {
		key := "dscm_" + rand.Text() + "." + rand.Text()
		secret := rand.Text()
		root := t.TempDir()
		configDir, resultsDir := filepath.Join(root, "config.d"), filepath.Join(root, "results.d")
		managed := filepath.Join(configDir, "dscregsync")
		if committed != "" {
			if err := os.MkdirAll(managed, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(managed, "machine.etag"), []byte(committed), 0600); err != nil {
				t.Fatal(err)
			}
		}
		statuses := []int{200, 304, 429, 503, 401, 200, 200, 200, 200}
		tags := []string{committed, `"one"`, `"one"`, `"one"`, `"one"`, `"one"`, `"one"`, `"one"`, `"two"`}
		revision1 := "11111111-1111-4111-8111-111111111111"
		revision2 := "22222222-2222-4222-8222-222222222222"
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if requests >= len(statuses) {
				t.Error("unexpected extra poll")
				w.WriteHeader(500)
				return
			}
			if r.Header.Get("If-None-Match") != tags[requests] {
				t.Error("uncommitted validator was used")
			}
			if r.Header.Get("Authorization") != "ApiKey "+key {
				t.Error("missing machine authentication")
			}
			status := statuses[requests]
			requests++
			w.Header().Set("Content-Type", "application/json")
			tag := `"two"`
			if requests == 1 {
				tag = `"one"`
			}
			if requests == 9 {
				tag = `"empty"`
			}
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "private, max-age=60")
			if status == 429 {
				w.Header().Set("Retry-After", "120")
			}
			w.WriteHeader(status)
			if status == 200 && requests != 6 {
				if requests == 9 {
					io.WriteString(w, `{"configurationRevision":3,"configurations":[]}`)
					return
				}
				revision := revision2
				if requests == 1 {
					revision = revision1
				}
				io.WriteString(w, `{"configurationRevision":2,"configurations":[{"name":"docker","revision":"`+revision+`","template":{"secret":"`+secret+`"},"parameters":{}}]}`)
			} else {
				io.WriteString(w, secret)
			}
		}))
		client := registry.NewClient(server.URL, key, time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		var logs bytes.Buffer
		var delays []time.Duration
		log := slog.New(slog.NewJSONHandler(&logs, nil))
		publisher := publish.New(configDir, resultsDir, log)
		var installed map[string]string
		poll(ctx, client, publisher, committed, 300*time.Second, log,
			func(ctx context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				switch len(delays) {
				case 1:
					installed = snapshot(t, root)
				case 2, 3, 4, 5, 6:
					if !reflect.DeepEqual(installed, snapshot(t, root)) {
						t.Error("304 or failure mutated desired state")
					}
					if len(delays) == 6 {
						if err := os.Mkdir(filepath.Join(managed, "blocked.json"), 0700); err != nil {
							t.Fatal(err)
						}
					}
				case 7:
					tag, err := registry.ReadETag(configDir)
					if err != nil || tag != `"one"` {
						t.Error("failed publisher committed validator")
					}
					if err := os.Remove(filepath.Join(managed, "blocked.json")); err != nil {
						t.Fatal(err)
					}
				case 8:
					if data, err := os.ReadFile(filepath.Join(managed, ".docker.revision")); err != nil || string(data) != revision2 {
						t.Error("retry did not publish new revision")
					}
				}
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
		if delays[0] < time.Minute || delays[1] < time.Minute || delays[2] < 2*time.Minute ||
			delays[3] < 10*time.Second || delays[4] < 20*time.Second ||
			delays[5] < 40*time.Second || delays[6] < 80*time.Second || delays[7] < time.Minute {
			t.Fatal("poller did not honor cache/retry/recovery scheduling")
		}
		if strings.Contains(logs.String(), key) || strings.Contains(logs.String(), secret) ||
			strings.Contains(logs.String(), `"etag"`) {
			t.Fatal("poll logs leaked response or credential content")
		}
		tag, err := registry.ReadETag(configDir)
		if err != nil || tag != `"empty"` {
			t.Fatal("authoritative empty collection did not commit")
		}
		entries, err := os.ReadDir(managed)
		if err != nil || len(entries) != 1 || entries[0].Name() != "machine.etag" {
			t.Fatal("empty collection did not remove managed inputs")
		}
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			if !json.Valid([]byte(line)) {
				t.Fatal("logs are not structured JSON")
			}
		}
	}
}

func TestForegroundErrorPreservesStateAndShutdown(t *testing.T) {
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

func TestPollingContinuesDuringBlockedReportUpload(t *testing.T) {
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
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	p := publish.New(cfg.ConfigDir, cfg.ResultsDir, log)
	c := &registry.Collection{Configurations: []registry.Configuration{{
		Name: "docker", Revision: "11111111-1111-4111-8111-111111111111", Template: []byte("{}"),
	}}}
	if err := p.Apply(context.Background(), c, `"one"`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "results", "input-failure.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.ResultsDir, "dscregsync"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ResultsDir, "dscregsync", "docker.json.result.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	uploadStarted, subsequentPoll := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/reports":
			_, _ = io.Copy(io.Discard, r.Body)
			close(uploadStarted)
			<-r.Context().Done()
		case "/v1/agent/configurations":
			select {
			case <-uploadStarted:
				close(subsequentPoll)
				cancel()
			default:
			}
			w.Header().Set("Cache-Control", "private, max-age=5")
			w.WriteHeader(304)
		default:
			t.Error("unexpected endpoint")
		}
	}))
	defer server.Close()
	cfg.RegistryURL = server.URL
	configData, _ := json.Marshal(cfg)
	path := filepath.Join(root, "service.json")
	if err := os.WriteFile(path, configData, 0600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	var logs bytes.Buffer
	if code := run(ctx, []string{"-config", path}, &logs); code != 0 {
		t.Fatal("daemon failed")
	}
	select {
	case <-subsequentPoll:
	default:
		t.Fatal("report I/O blocked conditional polling")
	}
	if !reflect.DeepEqual(before, snapshot(t, root)) {
		t.Fatal("upload outage modified cached desired state/results")
	}
}
