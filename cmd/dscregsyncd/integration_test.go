package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/config"
	"github.com/Bpoe/dsc-registry-sync/internal/registry"
)

// This is opt-in: it starts real peer executables against a disposable SQLite
// database and a private resource, never an installed service or production API.
func TestRealServiceRoundtrip(t *testing.T) {
	if os.Getenv("DSCREGSYNC_INTEGRATION") != "1" {
		t.Skip("set DSCREGSYNC_INTEGRATION=1 and DSCREGD_BINARY, DSCD_BINARY, DSC_BINARY")
	}
	peer := func(name string) string {
		path, err := filepath.Abs(os.Getenv(name))
		if os.Getenv(name) == "" || err != nil {
			t.Fatalf("%s must identify a real executable", name)
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s executable unavailable", name)
		}
		return path
	}
	registryBinary, reconcilerBinary, dscBinary := peer("DSCREGD_BINARY"), peer("DSCD_BINARY"), peer("DSC_BINARY")
	root := t.TempDir()
	db := filepath.Join(root, "registry.db")
	bootstrap, err := exec.Command(registryBinary, "bootstrap", "-database-dsn", db).Output()
	if err != nil {
		t.Fatal("real Registry bootstrap failed")
	}
	var admin struct{ Key string }
	if json.Unmarshal(bootstrap, &admin) != nil || admin.Key == "" {
		t.Fatal("Registry bootstrap returned no credential")
	}
	reports := filepath.Join(root, "reports")
	mustMkdir(t, reports)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	startPeer(t, registryBinary, nil, "-listen", address, "-development-http",
		"-database-dsn", db, "-report-root", reports, "-poll-max-age", "5s")
	origin := "http://" + address
	httpClient := &http.Client{Timeout: 5 * time.Second}
	defer httpClient.CloseIdleConnections()
	eventually(t, "real Registry readiness", func() bool {
		response, err := httpClient.Get(origin + "/health/ready")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == 200
	})
	api := func(method, path, key string, body any, expected int, out any) {
		t.Helper()
		var data []byte
		if body != nil {
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		request, err := http.NewRequest(method, origin+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal("cannot construct integration request")
		}
		request.Header.Set("Authorization", "ApiKey "+key)
		request.Header.Set("Content-Type", "application/json")
		response, err := httpClient.Do(request)
		if err != nil {
			t.Fatal("real Registry request failed")
		}
		defer response.Body.Close()
		if response.StatusCode != expected {
			t.Fatalf("%s integration endpoint returned %d, want %d", method, response.StatusCode, expected)
		}
		if out != nil && json.NewDecoder(response.Body).Decode(out) != nil {
			t.Fatal("cannot decode integration response")
		}
	}
	var machine struct{ ID string }
	api("POST", "/v1/machines", admin.Key, map[string]string{"name": "sync-integration"}, 201, &machine)
	machinePath := "/v1/machines/" + machine.ID
	var credential struct{ Key string }
	api("POST", machinePath+"/api-keys", admin.Key, map[string]string{"description": "disposable smoke"}, 201, &credential)

	resourceDir := filepath.Join(root, "resources")
	mustMkdir(t, resourceDir)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := func(mode string) map[string]any {
		return map[string]any{"executable": self, "args": []any{
			"-test.run=^TestDSCResourceHelper$", "--", mode,
			map[string]any{"jsonInputArg": "-input", "mandatory": true},
		}}
	}
	manifest := map[string]any{
		"$schema": "https://aka.ms/dsc/schemas/v3/bundled/resource/manifest.json",
		"type":    "Sync.Integration/Echo", "version": "1.0.0",
		"get": command("get"), "set": command("set"),
		"schema": map[string]any{"command": map[string]any{
			"executable": self, "args": []string{"-test.run=^TestDSCResourceHelper$", "--", "schema"},
		}},
	}
	writeJSON(t, filepath.Join(resourceDir, "echo.dsc.resource.json"), manifest)
	template := json.RawMessage(`{"$schema":"https://aka.ms/dsc/schemas/v3/bundled/config/document.json","metadata":{"dscd":{"operation":"test"}},"parameters":{"Output":{"type":"string"}},"resources":[{"name":"Echo","type":"Sync.Integration/Echo","properties":{"output":"[parameters('Output')]"}}]}`)
	put := func(value string, status int) string {
		var desired struct{ Revision string }
		api("PUT", machinePath+"/configurations/smoke", admin.Key,
			map[string]any{"template": template, "parameters": map[string]string{"Output": value}}, status, &desired)
		if desired.Revision == "" {
			t.Fatal("Registry omitted content revision")
		}
		return desired.Revision
	}
	first := put("first", 201)

	// The proxy only observes real traffic and delays the first immutable upload.
	// Responses, authentication and storage still come from the real Registry.
	target, _ := url.Parse(origin)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = nil
	var collections, unchanged, duplicates atomic.Int32
	uploadStarted, releaseUpload, uploadAccepted := make(chan struct{}), make(chan struct{}), make(chan int, 1)
	var firstUpload atomic.Bool
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseUpload) }) }
	t.Cleanup(release)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.URL.Path == "/v1/agent/configurations" {
			switch response.StatusCode {
			case 200:
				collections.Add(1)
			case 304:
				unchanged.Add(1)
			}
		} else if response.Request.URL.Path == "/v1/agent/reports" {
			if response.StatusCode == 200 {
				duplicates.Add(1)
			}
			if response.Request.Header.Get("X-Sync-Integration-First") == "1" {
				uploadAccepted <- response.StatusCode
			}
		}
		return nil
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/reports" && firstUpload.CompareAndSwap(false, true) {
			close(uploadStarted)
			select {
			case <-releaseUpload:
			case <-r.Context().Done():
				return
			}
			r.Header.Set("X-Sync-Integration-First", "1")
		}
		proxy.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	defer release()
	cfg := config.Defaults()
	cfg.RegistryURL = gateway.URL
	cfg.ConfigDir, cfg.ResultsDir = filepath.Join(root, "config.d"), filepath.Join(root, "results.d")
	cfg.APIKeyFile = filepath.Join(root, "machine.key")
	cfg.RequestTimeoutSeconds = 120
	if err := os.WriteFile(cfg.APIKeyFile, []byte(credential.Key), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "sync.json")
	writeJSON(t, configPath, cfg)
	startSync := func() func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() { done <- run(ctx, []string{"-config", configPath}, io.Discard) }()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				cancel()
				select {
				case code := <-done:
					if code != 0 {
						t.Error("sync daemon exited with failure")
					}
				case <-time.After(35 * time.Second):
					t.Error("sync daemon did not stop")
				}
			})
		}
		t.Cleanup(stop)
		return stop
	}
	stopSync := startSync()
	managed := filepath.Join(cfg.ConfigDir, "dscregsync")
	revisionPath := filepath.Join(managed, ".smoke.revision")
	eventually(t, "initial 200 publication", func() bool { return fileEquals(revisionPath, first) })
	marker, unblock := filepath.Join(root, "resource-started"), filepath.Join(root, "resource-release")
	writeJSON(t, unblock, true)
	env := []string{"DSCREGSYNC_RESOURCE_HELPER=1", "DSCREGSYNC_RESOURCE_MARKER=" + marker,
		"DSCREGSYNC_RESOURCE_RELEASE=" + unblock, "DSC_RESOURCE_PATH=" + resourceDir}
	startDSCD := func() func() {
		return startPeer(t, reconcilerBinary, env, "-config-dir", cfg.ConfigDir, "-results-dir", cfg.ResultsDir,
			"-dsc-path", dscBinary, "-interval", "1s", "-execution-timeout", "90s")
	}
	stopDSCD := startDSCD()
	eventually(t, "real dscd result snapshot", func() bool {
		select {
		case <-uploadStarted:
			return true
		default:
			return false
		}
	})
	resultPath := filepath.Join(cfg.ResultsDir, "dscregsync", "smoke.json.result.json")
	original, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Configuration string
		Parameters    string
		Outcome       string
		DSCResult     json.RawMessage
	}
	if json.Unmarshal(original, &envelope) != nil || envelope.Configuration != "dscregsync/smoke.json" ||
		envelope.Parameters != "dscregsync/smoke.parameters.json" || envelope.Outcome != "succeeded" ||
		len(envelope.DSCResult) == 0 || bytes.Equal(envelope.DSCResult, []byte("null")) {
		t.Fatal("real DSC did not produce the expected successful relative-path envelope")
	}
	eventually(t, "conditional 304", func() bool { return unchanged.Load() > 0 })
	etag, err := os.ReadFile(filepath.Join(managed, "machine.etag"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(unblock); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	eventually(t, "real dscd holding input protection", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})
	if err := verifyPeerProtection(filepath.Join(managed, "smoke.json")); err != nil {
		t.Fatal(err)
	}
	second := put("second", 200)
	eventually(t, "changed 200 received while dscd is protected", func() bool { return collections.Load() >= 2 })
	time.Sleep(300 * time.Millisecond)
	if !fileEquals(revisionPath, first) || !fileEquals(filepath.Join(managed, "machine.etag"), string(etag)) {
		t.Fatal("sync bypassed real dscd protection")
	}
	if !fileEquals(filepath.Join(managed, "smoke.parameters.json"), `{"Output":"first"}`) {
		t.Fatal("parameters changed while real dscd held protection")
	}
	writeJSON(t, unblock, true)
	eventually(t, "parameter update after protection release", func() bool {
		return fileEquals(revisionPath, second) &&
			fileEquals(filepath.Join(managed, "smoke.parameters.json"), `{"Output":"second"}`)
	})
	api("DELETE", machinePath+"/configurations/smoke", admin.Key, nil, 204, nil)
	eventually(t, "authoritative deletion", func() bool {
		_, err := os.Stat(revisionPath)
		_, resultErr := os.Stat(resultPath)
		return os.IsNotExist(err) && os.IsNotExist(resultErr)
	})
	release()
	select {
	case status := <-uploadAccepted:
		if status != 201 {
			t.Fatalf("removed-revision upload returned %d, want 201", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("historical upload did not complete")
	}
	client := registry.NewClient(origin, credential.Key, 5*time.Second)
	defer client.Close()
	report, err := registry.NewReport("smoke", first, original)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Upload(context.Background(), report)
	if err != nil || response.StatusCode != 200 {
		t.Fatal("exact historical retry was not deduplicated")
	}
	var history struct {
		Reports []struct {
			ID                    string
			ConfigurationRevision string
		}
	}
	api("GET", machinePath+"/reports", admin.Key, nil, 200, &history)
	if len(history.Reports) != 1 || history.Reports[0].ConfigurationRevision != first {
		t.Fatal("history contains duplicate or mislabeled execution")
	}
	var stored json.RawMessage
	api("GET", machinePath+"/reports/"+history.Reports[0].ID+"/content", admin.Key, nil, 200, &stored)
	if !bytes.Equal(bytes.TrimSpace(original), stored) {
		t.Fatal("Registry did not preserve exact nested result bytes")
	}
	third := put("third", 201)
	eventually(t, "recreated configuration and report", func() bool {
		api("GET", machinePath+"/reports", admin.Key, nil, 200, &history)
		return len(history.Reports) > 1 && history.Reports[0].ConfigurationRevision == third
	})
	stopDSCD()
	latest, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	latestReport, err := registry.NewReport("smoke", third, latest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Upload(context.Background(), latestReport); err != nil {
		t.Fatal("final complete snapshot could not be stored before restart")
	}
	stopSync()
	// Account for a final complete execution that arrived during shutdown.
	api("GET", machinePath+"/reports", admin.Key, nil, 200, &history)
	count := len(history.Reports)
	before304 := unchanged.Load()
	beforeDuplicates := duplicates.Load()
	stopSync = startSync()
	eventually(t, "restart uses committed validator", func() bool { return unchanged.Load() > before304 })
	eventually(t, "restart scan deduplicates latest result", func() bool {
		return duplicates.Load() > beforeDuplicates
	})
	api("GET", machinePath+"/reports", admin.Key, nil, 200, &history)
	if len(history.Reports) != count {
		t.Fatal("unchanged restart snapshot created duplicate history")
	}
	// A second execution after restarting dscd must remain a distinct history entry.
	startDSCD()
	eventually(t, "reconciler restart produces a distinct report", func() bool {
		api("GET", machinePath+"/reports", admin.Key, nil, 200, &history)
		return len(history.Reports) > count
	})
	stopSync()
}

func TestDSCResourceHelper(t *testing.T) {
	if os.Getenv("DSCREGSYNC_RESOURCE_HELPER") != "1" {
		t.Skip("subprocess resource helper")
	}
	for _, arg := range os.Args {
		if arg == "schema" {
			fmt.Print(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"output":{"type":"string"}},"required":["output"],"additionalProperties":false}`)
			os.Exit(0)
		}
	}
	if err := os.WriteFile(os.Getenv("DSCREGSYNC_RESOURCE_MARKER"), []byte("started"), 0600); err != nil {
		os.Exit(1)
	}
	for deadline := time.Now().Add(60 * time.Second); ; {
		if _, err := os.Stat(os.Getenv("DSCREGSYNC_RESOURCE_RELEASE")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(1)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i, arg := range os.Args {
		if arg == "-input" && i+1 < len(os.Args) {
			fmt.Print(os.Args[i+1])
			os.Exit(0)
		}
	}
	os.Exit(1)
}

func startPeer(t *testing.T, binary string, env []string, args ...string) func() {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = os.Environ()
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		for i := len(cmd.Env) - 1; i >= 0; i-- {
			if strings.EqualFold(strings.SplitN(cmd.Env[i], "=", 2)[0], key) {
				cmd.Env = append(cmd.Env[:i], cmd.Env[i+1:]...)
			}
		}
		cmd.Env = append(cmd.Env, entry)
	}
	// Peer diagnostics can contain resource values; never echo them to test logs.
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	terminate, err := startPeerProcess(cmd)
	if err != nil {
		t.Fatal("cannot start real integration peer")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if err := terminate(); err != nil {
				t.Error("cannot stop integration peer process tree")
			}
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("integration peer did not exit")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, scenario string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", scenario)
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func fileEquals(path, expected string) bool {
	data, err := os.ReadFile(path)
	return err == nil && string(data) == expected
}
