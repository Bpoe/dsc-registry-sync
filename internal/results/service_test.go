package results

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/publish"
	"github.com/Bpoe/dsc-registry-sync/internal/registry"
	"github.com/Bpoe/dsc-registry-sync/internal/testutil"
	"github.com/fsnotify/fsnotify"
)

const (
	revision1 = "11111111-1111-4111-8111-111111111111"
	revision2 = "22222222-2222-4222-8222-222222222222"
)

func desired(revision string) *registry.Collection {
	return &registry.Collection{Configurations: []registry.Configuration{{
		Name: "docker", Revision: revision, Template: []byte("{}"),
	}}}
}

func testService(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := registry.NewClient(server.URL, "dscm_test.secret", time.Second)
	t.Cleanup(client.Close)
	root := t.TempDir()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	p := publish.New(filepath.Join(root, "config.d"), filepath.Join(root, "results.d"), log)
	if err := p.Apply(context.Background(), desired(revision1), `"one"`); err != nil {
		t.Fatal(err)
	}
	s := New(p, client, filepath.Join(root, "results.d"), log)
	s.recovery = 20 * time.Millisecond
	return s
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "results", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func publishResult(t *testing.T, s *Service, data []byte) {
	t.Helper()
	dir := filepath.Join(s.root, "dscregsync")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(dir, ".dscd-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := testutil.ReplaceResult(file.Name(), filepath.Join(dir, "docker.json.result.json")); err != nil {
		t.Fatal(err)
	}
}

func start(t *testing.T, s *Service) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("result service did not cancel and join")
		}
	}
	t.Cleanup(stop)
	return stop
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event")
		var zero T
		return zero
	}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func observeWatchers(s *Service) <-chan *fsnotify.Watcher {
	watchers := make(chan *fsnotify.Watcher, 32)
	s.newWatcher = func() (*fsnotify.Watcher, error) {
		w, err := fsnotify.NewWatcher()
		if err == nil {
			watchers <- w
		}
		return w, err
	}
	return watchers
}

func TestStartupCoalescingAndRestartDeduplication(t *testing.T) {
	requests := make(chan []byte, 16)
	history := make(map[string][]byte)
	var count atomic.Int32
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var report struct {
			ID     string          `json:"uploadId"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(body, &report) != nil {
			t.Error("invalid report")
		}
		count.Add(1)
		if previous, exists := history[report.ID]; exists {
			if !bytes.Equal(previous, report.Result) {
				t.Error("same ID with changed bytes")
			}
			w.WriteHeader(200)
		} else {
			history[report.ID] = report.Result
			w.WriteHeader(201)
		}
		requests <- body
	})
	data := fixture(t, "input-failure")
	publishResult(t, s, data) // Exists before watcher setup.
	stop := start(t, s)
	first := receive(t, requests)
	for range 20 {
		publishResult(t, s, data)
	}
	time.Sleep(3 * s.debounce)
	if count.Load() != 1 {
		t.Fatal("duplicate notifications produced duplicate HTTP attempts")
	}
	stop()
	restarted := New(s.publisher, s.client, s.root, s.log)
	stopAgain := start(t, restarted)
	if next := receive(t, requests); !bytes.Equal(first, next) {
		t.Fatal("startup retry did not preserve identity and bytes")
	}
	stopAgain()
	if len(history) != 1 || count.Load() != 2 {
		t.Fatal("restart duplicated server history")
	}
}

func TestMissingDirectoryAndCompleteDestinations(t *testing.T) {
	requests := make(chan []byte, 4)
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(201)
		requests <- body
	})
	watchers := observeWatchers(s)
	start(t, s)
	first := receive(t, watchers)
	eventually(t, func() bool { return slices.Contains(first.WatchList(), filepath.Dir(s.root)) })
	data := fixture(t, "succeeded")
	// Create all missing levels and publish immediately; setup must scan after
	// attaching, not rely on receiving an event from a not-yet-watched child.
	publishResult(t, s, data)
	if body := receive(t, requests); !bytes.Contains(body, data) {
		t.Fatal("new directory publication was lost or altered")
	}
	dir := filepath.Join(s.root, "dscregsync")
	if err := os.WriteFile(filepath.Join(dir, ".dscd-incomplete"), []byte(`{"partial":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker.json.result.json"), []byte(`{"partial":`), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * s.debounce)
	select {
	case <-requests:
		t.Fatal("uploaded incomplete JSON or a staging file")
	default:
	}
	publishResult(t, s, fixture(t, "null-optionals"))
	receive(t, requests)
}

func TestClosedWatcherRecoveryScan(t *testing.T) {
	requests := make(chan bool, 4)
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		requests <- true
	})
	publishResult(t, s, fixture(t, "input-failure"))
	watchers := observeWatchers(s)
	start(t, s)
	first := receive(t, watchers)
	receive(t, requests)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	publishResult(t, s, fixture(t, "succeeded"))
	receive(t, watchers)
	receive(t, requests)
}

func TestRecoveryScanAfterOverflowAndDirectoryReplacement(t *testing.T) {
	requests := make(chan []byte, 8)
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(201)
		requests <- body
	})
	publishResult(t, s, fixture(t, "input-failure"))
	watchers := observeWatchers(s)
	start(t, s)
	first := receive(t, watchers)
	receive(t, requests)
	// Simulate lost notifications, then a genuine watcher overflow indication.
	if err := first.Remove(filepath.Join(s.root, "dscregsync")); err != nil {
		t.Fatal(err)
	}
	data := fixture(t, "succeeded")
	publishResult(t, s, data)
	time.Sleep(2 * s.debounce)
	select {
	case <-requests:
		t.Fatal("result was polled without a notification or recovery scan")
	default:
	}
	select {
	case first.Errors <- fsnotify.ErrEventOverflow:
	case <-time.After(time.Second):
		t.Fatal("watcher did not consume overflow")
	}
	receive(t, watchers)
	if body := receive(t, requests); !bytes.Contains(body, data) {
		t.Fatal("recovery missed latest result")
	}
	dir := filepath.Join(s.root, "dscregsync")
	if err := os.Rename(dir, filepath.Join(s.root, "old-results")); err != nil {
		t.Fatal(err)
	}
	publishResult(t, s, fixture(t, "null-optionals"))
	receive(t, requests)
}

func TestMixedResultNamesDuringStartupAndRecovery(t *testing.T) {
	for _, phase := range []string{"startup", "recovery"} {
		t.Run(phase, func(t *testing.T) {
			requests := make(chan []byte, 4)
			s := testService(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				w.WriteHeader(201)
				requests <- body
			})
			var logs bytes.Buffer
			s.log = slog.New(slog.NewJSONHandler(&logs, nil))
			s.publisher = publish.New(filepath.Join(filepath.Dir(s.root), "config.d"), s.root, s.log)
			managed := filepath.Join(s.root, "dscregsync")
			invalid := func() {
				t.Helper()
				for _, name := range []string{"UPPER", "BAD-private-filename"} {
					if err := os.WriteFile(filepath.Join(managed, name+".json.result.json"), []byte("private-result"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			data := fixture(t, "input-failure")
			publishResult(t, s, data)
			if phase == "startup" {
				invalid()
			}
			watchers := observeWatchers(s)
			stop := start(t, s)
			first := receive(t, watchers)
			if body := receive(t, requests); !bytes.Contains(body, data) {
				t.Fatal("startup scan lost valid result")
			}
			if phase == "recovery" {
				if err := first.Remove(managed); err != nil {
					t.Fatal(err)
				}
				invalid()
				data = fixture(t, "succeeded")
				publishResult(t, s, data)
				select {
				case first.Errors <- fsnotify.ErrEventOverflow:
				case <-time.After(time.Second):
					t.Fatal("watcher did not consume recovery trigger")
				}
				receive(t, watchers)
				if body := receive(t, requests); !bytes.Contains(body, data) {
					t.Fatal("recovery scan lost valid result")
				}
			}
			stop()
			warnings := 0
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var entry map[string]any
				if json.Unmarshal([]byte(line), &entry) != nil {
					t.Fatal("unstructured watcher log")
				}
				if entry["msg"] == "ignoring unsafe result scan name" {
					if entry["level"] != "WARN" {
						t.Fatal("invalid hint was not a warning")
					}
					warnings++
				}
			}
			if warnings < 2 {
				t.Fatal("scan did not warn about both invalid hints")
			}
			if strings.Contains(logs.String(), "UPPER") || strings.Contains(logs.String(), "private-") {
				t.Fatal("scan logged arbitrary filename or content")
			}
		})
	}
}

func TestWatcherCreationFailureRecoversAndCancels(t *testing.T) {
	requests := make(chan bool, 2)
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		requests <- true
	})
	publishResult(t, s, fixture(t, "input-failure"))
	var creates atomic.Int32
	s.newWatcher = func() (*fsnotify.Watcher, error) {
		if creates.Add(1) < 3 {
			return nil, errors.New("private watcher error")
		}
		return fsnotify.NewWatcher()
	}
	stop := start(t, s)
	receive(t, requests)
	stop()
	if creates.Load() < 3 {
		t.Fatal("watcher did not recover creation failures")
	}
	s = New(s.publisher, s.client, s.root, s.log)
	s.recovery = time.Hour
	s.newWatcher = func() (*fsnotify.Watcher, error) { return nil, errors.New("failed") }
	start(t, s)()
}

func TestUploadPoliciesLatestOnlyAndPrivacy(t *testing.T) {
	for _, status := range []int{400, 401, 403, 409, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls int
			var bodies [][]byte
			s := testService(t, func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				bodies = append(bodies, data)
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "private-http-error")
				} else {
					w.WriteHeader(200)
				}
			})
			var logs bytes.Buffer
			s.log = slog.New(slog.NewJSONHandler(&logs, nil))
			publishResult(t, s, fixture(t, "input-failure"))
			state := &pending{}
			s.attempt(context.Background(), "docker", state)
			permanent := status == 400 || status == 409
			if state.done != permanent || state.due.IsZero() != permanent {
				t.Fatal("incorrect retry disposition")
			}
			if !permanent && time.Until(state.due) < 119*time.Second {
				t.Fatal("Retry-After was not honored")
			}
			firstID := state.id
			s.attempt(context.Background(), "docker", state)
			if permanent && calls != 1 || !permanent && (calls != 2 || !bytes.Equal(bodies[0], bodies[1])) {
				t.Fatal("unchanged retry identity/body or permanent suppression failed")
			}
			// A newer publication supersedes the old attempt, even while offline.
			next := fixture(t, "succeeded")
			publishResult(t, s, next)
			s.attempt(context.Background(), "docker", state)
			if state.id == firstID || !state.done || !bytes.Contains(bodies[len(bodies)-1], next) {
				t.Fatal("latest result did not replace failed/accepted snapshot")
			}
			if strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "private input") ||
				strings.Contains(logs.String(), "dscm_test.secret") || strings.Contains(logs.String(), "<>&") {
				t.Fatal("logs leaked credentials, result or response content")
			}
			if err := os.Remove(filepath.Join(s.root, "dscregsync", "docker.json.result.json")); err != nil {
				t.Fatal(err)
			}
			if !s.attempt(context.Background(), "docker", state) {
				t.Fatal("removed result retained retry state")
			}
		})
	}
}

func TestInFlightHistoricalUploadDoesNotBlockUpdate(t *testing.T) {
	entered, release := make(chan []byte, 2), make(chan struct{})
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		entered <- body
		select {
		case <-release:
			w.WriteHeader(201)
		case <-r.Context().Done():
		}
	})
	publishResult(t, s, fixture(t, "input-failure"))
	stop := start(t, s)
	body := receive(t, entered)
	if !bytes.Contains(body, []byte(revision1)) {
		t.Fatal("wrong original association")
	}
	updated := make(chan error, 1)
	go func() { updated <- s.publisher.Apply(context.Background(), desired(revision2), `"two"`) }()
	if err := receive(t, updated); err != nil {
		t.Fatal(err)
	}
	if err := s.publisher.Apply(context.Background(), &registry.Collection{Configurations: []registry.Configuration{}}, `"removed"`); err != nil {
		t.Fatal(err)
	}
	// Already captured old bytes may finish after desired state changes/removal.
	if !bytes.Contains(body, []byte(revision1)) {
		t.Fatal("old report was relabeled")
	}
	close(release)
	if err := s.publisher.Apply(context.Background(), desired(revision2), `"reinstalled"`); err != nil {
		t.Fatal(err)
	}
	publishResult(t, s, fixture(t, "succeeded"))
	if body = receive(t, entered); !bytes.Contains(body, []byte(revision2)) {
		t.Fatal("new report did not use new revision")
	}
	stop()
}

func TestCancellationDuringUpload(t *testing.T) {
	entered := make(chan bool, 1)
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- true
		<-r.Context().Done()
	})
	publishResult(t, s, fixture(t, "input-failure"))
	stop := start(t, s)
	receive(t, entered)
	stop()
}

func TestChangesCoalesceWithoutPayloadQueue(t *testing.T) {
	c := changes{wake: make(chan struct{}, 1), names: make(map[string]bool)}
	for range 1000 {
		c.notify("docker")
	}

	c.reconcile([]string{"docker", "other"})
	names, scan, scanned := c.take()
	if len(names) != 1 || len(scan) != 2 || !scanned || len(c.wake) != 1 {
		t.Fatal("notifications were not coalesced")
	}
	names, scan, scanned = c.take()
	if len(names) != 0 || scan != nil || scanned {
		t.Fatal("hints were not consumed")
	}
}

func TestBackoffEventsDoNotBypassRetryAfter(t *testing.T) {
	var calls atomic.Int32
	requests := make(chan bool, 8)
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
		requests <- true
	})
	data := fixture(t, "input-failure")
	publishResult(t, s, data)
	stop := start(t, s)
	receive(t, requests)
	for range 10 {
		publishResult(t, s, data)
	}
	time.Sleep(3 * s.debounce)
	stop()
	if calls.Load() != 1 {
		t.Fatal("events bypassed Retry-After")
	}
}

func TestOfflineAttemptsSupersedeRatherThanQueue(t *testing.T) {
	var bodies [][]byte
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, data)
		if len(bodies) == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(201)
		}
	})
	publishResult(t, s, fixture(t, "input-failure"))
	state := &pending{}
	s.attempt(context.Background(), "docker", state)
	if state.done || state.due.IsZero() {
		t.Fatal("offline result did not schedule retry")
	}
	oldID := state.id
	publishResult(t, s, fixture(t, "null-optionals")) // Superseded before a retry.
	latest := fixture(t, "succeeded")
	publishResult(t, s, latest)
	s.attempt(context.Background(), "docker", state)
	if !state.done || state.id == oldID || len(bodies) != 2 || !bytes.Contains(bodies[1], latest) {
		t.Fatal("offline work retained a historical backlog")
	}
}
