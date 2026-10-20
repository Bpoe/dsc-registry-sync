package registry

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const revision = "77701b57-ddb9-4e7b-b235-b04ed87e3468"

func unit(name, template, parameters string) string {
	fields := `{"name":"` + name + `","revision":"` + revision + `","template":` + template
	if parameters != "" {
		fields += `,"parameters":` + parameters
	}
	return fields + "}"
}

func collection(units ...string) string {
	return `{"configurationRevision":7,"configurations":[` + strings.Join(units, ",") + `],"future":true}`
}

func machineKey() string { return "dscm_" + rand.Text() + "." + rand.Text() }

func TestFetchMachineContract(t *testing.T) {
	key := machineKey()
	tag := `W/"opaque-validator"`
	template := `{"resources":[],"large":123456789012345678901234567890,"unknown":1e9999}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/agent/configurations" ||
			r.URL.RawQuery != "" || r.Header.Get("Authorization") != "ApiKey "+key ||
			r.Header.Get("If-None-Match") != tag || r.Header.Get("Accept") != "application/json" {
			t.Error("incorrect machine-scoped request")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("ETag", `"new"`)
		w.Header().Set("Cache-Control", "private, max-age=300")
		io.WriteString(w, collection(unit("absent", template, ""), unit("null", "{}", "null"), unit("empty", "{}", "{}")))
	}))
	defer server.Close()
	client := NewClient(server.URL, key, time.Second)
	defer client.Close()
	response, err := client.Fetch(context.Background(), tag)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || response.ETag != `"new"` || response.Collection == nil ||
		response.Collection.ConfigurationRevision != 7 || len(response.Collection.Configurations) != 3 {
		t.Fatal("incorrect typed collection")
	}
	units := response.Collection.Configurations
	if string(units[0].Template) != template || len(units[0].Parameters) != 0 ||
		len(units[1].Parameters) != 0 || string(units[2].Parameters) != "{}" {
		t.Fatal("opaque numbers or absent/null/empty parameters changed")
	}
}

func TestFetchResponses(t *testing.T) {
	valid := collection(unit("docker", "{}", ""))
	for _, test := range []struct {
		name, body, etag, contentType string
		status                        int
		conditional, valid            bool
	}{
		{"empty", collection(), `"e"`, "application/json", 200, false, true},
		{"not modified", "not JSON", "", "", 304, true, true},
		{"unsolicited 304", "", "", "", 304, false, false},
		{"missing tag", valid, "", "application/json", 200, false, false},
		{"unquoted tag", valid, "e", "application/json", 200, false, false},
		{"wrong media", valid, `"e"`, "text/html", 200, false, false},
		{"wrong charset", valid, `"e"`, "application/json; charset=utf-16", 200, false, false},
		{"missing media", valid, `"e"`, "", 200, false, false},
		{"truncated", valid[:len(valid)-1], `"e"`, "application/json", 200, false, false},
		{"missing list", `{"configurationRevision":0}`, `"e"`, "application/json", 200, false, false},
		{"null list", `{"configurationRevision":0,"configurations":null}`, `"e"`, "application/json", 200, false, false},
		{"missing revision", `{"configurations":[]}`, `"e"`, "application/json", 200, false, false},
		{"null revision", `{"configurationRevision":null,"configurations":[]}`, `"e"`, "application/json", 200, false, false},
		{"negative revision", `{"configurationRevision":-1,"configurations":[]}`, `"e"`, "application/json", 200, false, false},
		{"noninteger revision", `{"configurationRevision":1.5,"configurations":[]}`, `"e"`, "application/json", 200, false, false},
		{"null body", "null", `"e"`, "application/json", 200, false, false},
		{"trailing value", valid + `{}`, `"e"`, "application/json", 200, false, false},
		{"partial collection", collection(unit("ok", "{}", ""), "null"), `"e"`, "application/json", 200, false, false},
		{"duplicated names", collection(unit("a", "{}", ""), unit("a", "{}", "")), `"e"`, "application/json", 200, false, false},
		{"duplicated fields", `{"configurationRevision":1,"configurations":[],"configurations":[]}`, `"e"`, "application/json", 200, false, false},
		{"invalid revision", strings.ReplaceAll(valid, revision, "invalid"), `"e"`, "application/json", 200, false, false},
		{"null template", collection(unit("a", "null", "")), `"e"`, "application/json", 200, false, false},
		{"array template", collection(unit("a", "[]", "")), `"e"`, "application/json", 200, false, false},
		{"string template", collection(unit("a", `"{}"`, "")), `"e"`, "application/json", 200, false, false},
		{"array parameters", collection(unit("a", "{}", "[]")), `"e"`, "application/json", 200, false, false},
		{"wrapped template", collection(unit("a", `{"format":"json","content":{}}`, "")), `"e"`, "application/json", 200, false, false},
		{"wrapped parameters", collection(unit("a", "{}", `{"format":"json","content":{}}`)), `"e"`, "application/json", 200, false, false},
		{"unsafe name", collection(unit("../outside", "{}", "")), `"e"`, "application/json", 200, false, false},
		{"reserved name", collection(unit("con", "{}", "")), `"e"`, "application/json", 200, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", test.etag)
				w.Header().Set("Content-Type", test.contentType)
				w.Header().Set("Cache-Control", "private, max-age=60")
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := NewClient(server.URL, machineKey(), time.Second)
			defer client.Close()
			tag := ""
			if test.conditional {
				tag = `"committed"`
			}
			response, err := client.Fetch(context.Background(), tag)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
			if (!test.valid || test.status == 304) && response.Collection != nil {
				t.Fatal("non-authoritative response returned a collection")
			}
			if response.Header.Get("Cache-Control") != "private, max-age=60" {
				t.Fatal("lost scheduling headers")
			}
		})
	}
}

func TestHTTPFailuresAndRedirectPrivacy(t *testing.T) {
	key := machineKey()
	for _, status := range []int{204, 301, 302, 307, 308, 400, 401, 403, 404, 429, 500, 503} {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.Header().Set("Location", "/redirect/"+key)
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(status)
			io.WriteString(w, key)
		}))
		client := NewClient(server.URL, key, time.Second)
		response, err := client.Fetch(context.Background(), "")
		client.Close()
		server.Close()
		if err == nil || strings.Contains(err.Error(), key) || response.Collection != nil || requests != 1 {
			t.Fatal("failed response or redirect was not handled privately")
		}
		if response.StatusCode != status || response.Header.Get("Retry-After") != "120" {
			t.Fatal("failure metadata missing")
		}
	}
}

func TestResponseLimits(t *testing.T) {
	for _, body := range []string{
		collection(unit("large", `{"value":"`+strings.Repeat("x", MaxDocumentBytes)+`"}`, "")),
		strings.Repeat(" ", MaxCollectionBytes+1),
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"e"`)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		}))
		client := NewClient(server.URL, machineKey(), 5*time.Second)
		response, err := client.Fetch(context.Background(), "")
		client.Close()
		server.Close()
		if err == nil || response.Collection != nil {
			t.Fatal("accepted oversized response")
		}
	}
}

func TestDocumentSizeBoundary(t *testing.T) {
	template := `{"value":"` + strings.Repeat("x", MaxDocumentBytes-len(`{"value":""}`)) + `"}`
	if _, err := decodeCollection([]byte(collection(unit("limit", template, "")))); err != nil {
		t.Fatal("rejected a document at the exact limit:", err)
	}
	if _, err := decodeCollection([]byte(collection(unit("limit", template, "{}")))); err == nil {
		t.Fatal("did not include parameters in the per-unit size limit")
	}
}

func TestCancellationAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client := NewClient(server.URL, machineKey(), 50*time.Millisecond)
	defer client.Close()
	start := time.Now()
	if _, err := client.Fetch(context.Background(), ""); err == nil || time.Since(start) > 5*time.Second {
		t.Fatal("request did not time out")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Fetch(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("request did not honor cancellation")
	}
}

func TestTLSVerification(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"e"`)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, collection())
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	client := NewClient(server.URL, machineKey(), 5*time.Second)
	defer client.Close()
	if _, err := client.Fetch(context.Background(), ""); err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
	client.http.Transport = server.Client().Transport
	if _, err := client.Fetch(context.Background(), ""); err != nil {
		t.Fatal("trusted test certificate rejected:", err)
	}
}

func TestNamesAndRevisions(t *testing.T) {
	for _, name := range []string{"a", "0", "unit_one.two-3", "com0", "lpt10", strings.Repeat("a", 64)} {
		if !ValidName(name) {
			t.Fatal("rejected safe stem")
		}
	}
	for _, name := range []string{
		"", ".", "..", ".hidden", "-unit", "a/b", `a\b`, "a:", "a ", "A", "é",
		"unit.", "unit.parameters", "con", "con.anything", "nul", "aux", "prn", "com1", "lpt9.log",
		strings.Repeat("a", 65),
	} {
		if ValidName(name) {
			t.Fatal("accepted unsafe stem")
		}
	}
	for _, bad := range []string{"", strings.ToUpper(revision), revision + " ", strings.ReplaceAll(revision, "-", ""), "{" + revision + "}"} {
		if validRevision(bad) {
			t.Fatal("accepted noncanonical revision")
		}
	}
}

func TestInvalidETagNeverSent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request sent with an invalid local validator")
	}))
	defer server.Close()
	client := NewClient(server.URL, machineKey(), time.Second)
	defer client.Close()
	if _, err := client.Fetch(context.Background(), "\"tag\"\r\ninjected: header"); err == nil {
		t.Fatal("accepted invalid local validator")
	}
}

func TestAmbiguousETagAndTruncatedTransfer(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"e"`)
			w.Header().Set("Content-Type", "application/json")
			if duplicate {
				w.Header().Add("ETag", `"another"`)
			} else {
				w.Header().Set("Content-Length", "99999")
			}
			io.WriteString(w, collection())
		}))
		client := NewClient(server.URL, machineKey(), time.Second)
		response, err := client.Fetch(context.Background(), "")
		client.Close()
		server.Close()
		if err == nil || response.Collection != nil || response.ETag != "" {
			t.Fatal("accepted incomplete transfer or ambiguous validator")
		}
	}
}

type failingTransport struct{ secret string }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New(f.secret)
}

func TestTransportErrorPrivacy(t *testing.T) {
	key := machineKey()
	client := NewClient("https://registry.example", key, time.Second)
	defer client.Close()
	client.http.Transport = failingTransport{secret: key}
	_, err := client.Fetch(context.Background(), "")
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatal("transport error was not sanitized")
	}
}
