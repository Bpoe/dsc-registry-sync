package registry

import (
	"bytes"
	"context"
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
)

func TestReportExactEnvelopeAndRestartIdentity(t *testing.T) {
	for _, fixture := range []string{"input-failure", "succeeded", "null-optionals"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "results", fixture+".json"))
			if err != nil {
				t.Fatal(err)
			}
			key := machineKey()
			var first []byte
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/agent/reports" || r.URL.RawQuery != "" ||
					r.Header.Get("Authorization") != "ApiKey "+key || r.Header.Get("Content-Type") != "application/json" {
					t.Error("incorrect machine report request")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				var fields map[string]json.RawMessage
				if json.Unmarshal(body, &fields) != nil || len(fields) != 4 ||
					string(fields["configurationName"]) != `"docker"` ||
					string(fields["configurationRevision"]) != `"`+revision+`"` ||
					!bytes.Equal(fields["result"], bytes.TrimSpace(data)) ||
					!bytes.Contains(body, append(append([]byte(`"result":`), data...), '}')) {
					t.Error("envelope or wrapper changed")
				}
				// This endpoint deliberately has no current desired revision:
				// historical and removed units use exactly the same upload path.
				calls++
				if calls == 1 {
					first = body
					w.WriteHeader(201)
				} else {
					if !bytes.Equal(body, first) {
						t.Error("retry changed exact request bytes")
					}
					w.WriteHeader(200)
				}
			}))
			defer server.Close()
			var id string
			for i := range 3 {
				// Reconstruct client and report, as on process restart.
				client := NewClient(server.URL, key, time.Second)
				report, err := NewReport("docker", revision, data)
				if err != nil {
					t.Fatal(err)
				}
				if i > 0 && id != report.ID() {
					t.Fatal("restart changed ID")
				}
				id = report.ID()
				response, err := client.Upload(context.Background(), report)
				client.Close()
				if err != nil || response.Permanent || response.StatusCode != []int{201, 200, 200}[i] {
					t.Fatalf("response: %+v, %v", response, err)
				}
			}
		})
	}
}

func TestUploadIdentityAndOwnedBytes(t *testing.T) {
	data := []byte("{\"schemaVersion\":1}\n")
	report, err := NewReport("docker", revision, data)
	if err != nil {
		t.Fatal(err)
	}
	// Independently computed with Python hashlib/uuid (RFC 9562 UUIDv5).
	if report.ID() != "8d9330d3-1b7e-5980-9256-dac0bfe18b18" {
		t.Fatalf("upload ID algorithm changed: %s", report.ID())
	}
	if !validRevision(report.ID()) || report.ID()[14] != '5' || !strings.ContainsRune("89ab", rune(report.ID()[19])) {
		t.Fatalf("not a canonical UUIDv5: %s", report.ID())
	}
	for _, test := range []struct{ name, revision, result string }{
		{"other", revision, string(data)},
		{"docker", "11111111-1111-4111-8111-111111111111", string(data)},
		{"docker", revision, `{"schemaVersion":1}`},
		{"docker", revision, "{\"schemaVersion\": 1}\n"},
		{"docker", revision, "{\"schemaVersion\":2}\n"},
	} {
		next, err := NewReport(test.name, test.revision, []byte(test.result))
		if err != nil || next.ID() == report.ID() {
			t.Fatal("distinct input tuple reused upload identity")
		}
	}
	data[0] = '!'
	if !json.Valid(report.body) || !bytes.Contains(report.body, []byte("{\"schemaVersion\":1}\n")) {
		t.Fatal("caller mutation changed immutable report")
	}
}

func TestInvalidReportBoundaries(t *testing.T) {
	for _, test := range []struct{ name, revision, result string }{
		{"../outside", revision, "{}"},
		{"con", revision, "{}"},
		{"docker", "unknown", "{}"},
		{"docker", revision, ""},
		{"docker", revision, "null"},
		{"docker", revision, "[]"},
		{"docker", revision, `{"secret":`},
		{"docker", revision, `{"x":1,"x":2}`},
		{"docker", revision, "{}{}"},
		{"docker", revision, "{\"x\":\"\xff\"}"},
		{"docker", revision, `{"x":"` + strings.Repeat("x", MaxResultBytes) + `"}`},
		{"docker", revision, `{"x":` + strings.Repeat("[", 128) + "0" + strings.Repeat("]", 128) + "}"},
	} {
		if _, err := NewReport(test.name, test.revision, []byte(test.result)); !errors.Is(err, ErrInvalidReport) {
			t.Fatal("accepted invalid report")
		}
	}
	// Do not interpret resources or duplicate the server's evolving envelope schema.
	data := []byte(`{"future":null}`)
	if _, err := NewReport("docker", revision, data); err != nil {
		t.Fatal(err)
	}
}

func TestUploadResponsePolicyAndPrivacy(t *testing.T) {
	for _, code := range []int{200, 201, 204, 302, 400, 401, 403, 404, 408, 409, 413, 415, 429, 500, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			key, secret := machineKey(), "private-result-and-error"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.Header().Set("Location", "/"+secret)
				w.WriteHeader(code)
				_, _ = io.WriteString(w, secret)
			}))
			defer server.Close()
			client := NewClient(server.URL, key, time.Second)
			defer client.Close()
			report, _ := NewReport("docker", revision, []byte(`{"private":"`+secret+`"}`))
			response, err := client.Upload(context.Background(), report)
			success := code == 200 || code == 201
			permanent := !success && code < 500 && code != 401 && code != 403 && code != 408 && code != 429
			if (err == nil) != success || response.Permanent != permanent || response.StatusCode != code ||
				response.Header.Get("Retry-After") != "120" {
				t.Fatalf("incorrect policy: %+v, %v", response, err)
			}
			if err != nil && (strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), key)) {
				t.Fatal("error leaked response content")
			}
		})
	}
}

func TestUploadCancellationTimeoutAndNetworkFailure(t *testing.T) {
	report, _ := NewReport("docker", revision, []byte("{}"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	client := NewClient(server.URL, machineKey(), 50*time.Millisecond)
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Upload(ctx, report); !errors.Is(err, context.Canceled) {
		t.Fatal("request ignored cancellation")
	}
	if response, err := client.Upload(context.Background(), report); err == nil || response.Permanent {
		t.Fatal("timeout was not retryable")
	}
	server.Close()
	if response, err := client.Upload(context.Background(), report); err == nil || response.Permanent {
		t.Fatal("offline transport was not retryable")
	}
}

func TestAmbiguousAcceptanceRetryKeepsIdentity(t *testing.T) {
	var accepted []byte
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls++
		if calls == 1 {
			accepted = body // Model a committed receipt with a lost HTTP response.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		if !bytes.Equal(accepted, body) {
			t.Error("ambiguous retry changed committed upload identity/content")
			w.WriteHeader(409)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	client := NewClient(server.URL, machineKey(), time.Second)
	defer client.Close()
	for i := range 2 {
		report, _ := NewReport("removed-unit", revision, []byte("{ \"opaque\": \"<>&\" }\n"))
		response, err := client.Upload(context.Background(), report)
		if i == 0 && (err == nil || response.Permanent) ||
			i == 1 && (err != nil || response.StatusCode != 200) {
			t.Fatalf("ambiguous acceptance/retry: %+v, %v", response, err)
		}
	}
	if calls != 2 {
		t.Fatal("unexpected duplicate attempts")
	}
}
