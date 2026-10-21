package registry

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry-sync/internal/config"
)

func enrollmentConfig(origin string) config.Config {
	cfg := config.Defaults()
	cfg.RegistryURL = origin
	return cfg
}

func TestRegisterRequestAndResponse(t *testing.T) {
	token, key := "dscr_"+rand.Text(), "dscm_"+rand.Text()+"."+rand.Text()
	name := "Example Host"
	id := "550e8400-e29b-41d4-a716-446655440000"
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "POST" || r.URL.RequestURI() != "/v1/agent/register" ||
			r.Header.Get("Authorization") != "Bearer "+token ||
			r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong registration request metadata")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"name":"Example Host"}` {
			t.Error("wrong registration request body")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"id": id, "name": name, "apiKey": key, "additive": true})
	}))
	defer server.Close()
	// Test trust is installed in the default transport, never through a production bypass.
	original := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = original }()
	result, err := Register(context.Background(), enrollmentConfig(server.URL), name, token)
	if err != nil || result.ID != id || result.Name != name || result.APIKey != key || requests != 1 {
		t.Fatal("registration did not return the validated machine credential")
	}
}

func TestRegisterFailuresArePrivateAndNeverRetried(t *testing.T) {
	token, key := "dscr_"+rand.Text(), "dscm_"+rand.Text()+"."+rand.Text()
	valid := `{"id":"550e8400-e29b-41d4-a716-446655440000","name":"foobar","apiKey":"` + key + `"}`
	for _, test := range []struct {
		name, body, media, want string
		status                  int
	}{
		{"expired", token + key, "application/json", "invalid or expired", 401},
		{"duplicate", token + key, "application/json", "already registered", 409},
		{"server", token + key, "application/json", "HTTP 503", 503},
		{"invalid-name", token + key, "application/json", "HTTP 400", 400},
		{"unexpected-success", valid, "application/json", "HTTP 200", 200},
		{"malformed", `{` + token + key, "application/json", "invalid or incomplete", 201},
		{"oversized", valid + strings.Repeat(" ", MaxRegistrationBytes), "application/json", "invalid or incomplete", 201},
		{"missing-key", strings.Replace(valid, `"apiKey"`, `"APIKEY"`, 1), "application/json", "invalid or incomplete", 201},
		{"wrong-key", strings.Replace(valid, "dscm_", "dsca_", 1), "application/json", "invalid or incomplete", 201},
		{"wrong-name", strings.Replace(valid, "foobar", "other", 1), "application/json", "invalid or incomplete", 201},
		{"bad-id", strings.Replace(valid, "550e8400", "notauuid", 1), "application/json", "invalid or incomplete", 201},
		{"null", "null", "application/json", "invalid or incomplete", 201},
		{"trailing", valid + `{}`, "application/json", "invalid or incomplete", 201},
		{"duplicate-field", strings.Replace(valid, `"name":`, `"name":"foobar","name":`, 1), "application/json", "invalid or incomplete", 201},
		{"wrong-media", valid, "text/plain", "invalid or incomplete", 201},
		{"wrong-charset", valid, "application/json;charset=latin1", "invalid or incomplete", 201},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", test.media)
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			result, err := Register(context.Background(), enrollmentConfig(server.URL), "foobar", token)
			if err == nil || !strings.Contains(err.Error(), test.want) || requests != 1 || result != (Registration{}) {
				t.Fatal("registration failure did not fail closed with a useful error")
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), key) {
				t.Fatal("registration failure leaked credentials")
			}
		})
	}
}

func TestRegisterTransportPolicy(t *testing.T) {
	token := "dscr_" + rand.Text()
	for _, status := range []int{301, 302, 303, 307, 308} {
		hits := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.Header().Set("Location", "/"+token)
			w.WriteHeader(status)
		}))
		_, err := Register(context.Background(), enrollmentConfig(server.URL), "foobar", token)
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "redirect refused") || strings.Contains(err.Error(), token) || hits != 1 {
			t.Fatal("registration followed or disclosed a redirect")
		}
	}
	tls := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("untrusted TLS request reached handler")
	}))
	tls.Config.ErrorLog = log.New(io.Discard, "", 0)
	tls.StartTLS()
	_, err := Register(context.Background(), enrollmentConfig(tls.URL), "foobar", token)
	tls.Close()
	if err == nil || !strings.Contains(err.Error(), "TLS trust") {
		t.Fatal("registration did not enforce normal TLS trust")
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	origin := closed.URL
	closed.Close()
	if _, err := Register(context.Background(), enrollmentConfig(origin), "foobar", token); err == nil ||
		!strings.Contains(err.Error(), "connectivity") || strings.Contains(err.Error(), token) {
		t.Fatal("network failure was not sanitized")
	}
	for _, origin := range []string{"ftp://localhost", "https://user:" + token + "@localhost", "https://localhost/" + token, "https://localhost?token=" + token} {
		cfg := enrollmentConfig(origin)
		if _, err := Register(context.Background(), cfg, "foobar", token); err == nil || strings.Contains(err.Error(), token) {
			t.Fatal("invalid origin accepted or leaked")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	if _, err := Register(ctx, enrollmentConfig(server.URL), "foobar", token); err == nil {
		t.Fatal("registration ignored cancellation")
	}
	timeoutServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer timeoutServer.Close()
	cfg := enrollmentConfig(timeoutServer.URL)
	cfg.RequestTimeoutSeconds = 1
	start := time.Now()
	if _, err := Register(context.Background(), cfg, "foobar", token); err == nil ||
		!strings.Contains(err.Error(), "timed out") || time.Since(start) > 5*time.Second {
		t.Fatal("registration request timeout was not enforced")
	}
}

func TestMachineDisplayNames(t *testing.T) {
	for _, name := range []string{"foobar", "Windows Host", "HOST-1", "Caf\u00e9", strings.Repeat("x", 256)} {
		if !ValidMachineName(name) {
			t.Error("valid display name rejected")
		}
	}
	for _, name := range []string{"", " host", "host ", "host\n", "\x1bhost", string([]byte{0xff}), strings.Repeat("x", 257)} {
		if ValidMachineName(name) {
			t.Error("invalid display name accepted")
		}
	}
}
