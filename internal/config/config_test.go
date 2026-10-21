package config

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, []byte(`{"registryUrl":"https://registry.example/"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistryURL != "https://registry.example" ||
		cfg.RequestTimeoutSeconds != 30 || cfg.PollFallbackSeconds != 300 || cfg.AllowInsecureLoopback {
		t.Fatal("incorrect defaults")
	}
	for _, path := range []string{cfg.APIKeyFile, cfg.ConfigDir, cfg.ResultsDir, DefaultPath()} {
		if !filepath.IsAbs(path) {
			t.Fatal("default path is not absolute")
		}
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(cfg.APIKeyFile, filepath.Join(os.Getenv("ProgramData"), "dsc")) {
			t.Fatal("Windows default does not use ProgramData")
		}
	} else if cfg.ConfigDir != "/etc/dsc/config.d" || cfg.ResultsDir != "/var/lib/dsc/results.d" {
		t.Fatal("incorrect Linux defaults")
	}
}

func TestLoadOverrides(t *testing.T) {
	root := t.TempDir()
	expected := Config{
		RegistryURL: "http://127.0.0.1:8080", APIKeyFile: filepath.Join(root, "key"),
		ConfigDir: root, ResultsDir: filepath.Join(root, "results"),
		RequestTimeoutSeconds: 1, PollFallbackSeconds: 5, AllowInsecureLoopback: true,
	}
	data, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeConfig(t, data))
	if err != nil || cfg != expected {
		t.Fatal("valid configuration overrides were not retained")
	}
}

func TestLoadUnknownFields(t *testing.T) {
	expected := Defaults()
	expected.RegistryURL = "https://registry.example"
	for _, test := range []struct {
		name  string
		value string
	}{
		{"string", `"ignored"`},
		{"number", `123456789012345678901234567890e9999`},
		{"boolean", `true`},
		{"object", `{"registryUrl":null,"configDir":"relative","nested":{"enabled":true}}`},
		{"array", `["ignored",42,false,{"requestTimeoutSeconds":null},null]`},
		{"null", `null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"registryUrl":"https://registry.example/","futureSetting":` + test.value + `}`
			cfg, err := Load(writeConfig(t, []byte(body)))
			if err != nil {
				t.Fatal(err)
			}
			if cfg != expected {
				t.Fatal("unknown field changed configuration or omitted defaults")
			}
		})
	}
}

func TestLoadMixedKnownAndUnknownFields(t *testing.T) {
	root := t.TempDir()
	expected := Config{
		RegistryURL: "https://registry.example", APIKeyFile: filepath.Join(root, "key"),
		ConfigDir: root, ResultsDir: filepath.Join(root, "results"),
		RequestTimeoutSeconds: 45, PollFallbackSeconds: 600, AllowInsecureLoopback: true,
	}
	data, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"futureSetting":true,"anotherSetting":{"enabled":true},"deprecatedSetting":null,` + string(data[1:])
	cfg, err := Load(writeConfig(t, []byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg != expected {
		t.Fatal("unknown fields changed recognized overrides")
	}
}

func TestLoadInvalidRecognizedFieldTypes(t *testing.T) {
	for _, field := range []struct {
		name   string
		values []string
	}{
		{"registryUrl", []string{`null`, `42`, `true`, `{}`, `[]`}},
		{"apiKeyFile", []string{`null`, `42`, `true`, `{}`, `[]`}},
		{"configDir", []string{`null`, `42`, `true`, `{}`, `[]`}},
		{"resultsDir", []string{`null`, `42`, `true`, `{}`, `[]`}},
		{"requestTimeoutSeconds", []string{`null`, `"30"`, `true`, `{}`, `[]`, `1.5`, `1e1`}},
		{"pollFallbackSeconds", []string{`null`, `"300"`, `true`, `{}`, `[]`, `5.5`, `1e2`}},
		{"allowInsecureLoopback", []string{`null`, `"true"`, `1`, `{}`, `[]`}},
	} {
		for _, value := range field.values {
			t.Run(field.name+"/"+value, func(t *testing.T) {
				body := `{"futureSetting":null,"` + field.name + `":` + value
				if field.name != "registryUrl" {
					body += `,"registryUrl":"https://registry.example"`
				}
				if _, err := Load(writeConfig(t, []byte(body+`}`))); err == nil {
					t.Fatal("accepted invalid recognized field")
				}
			})
		}
	}
}

func TestLoadCaseInsensitiveFields(t *testing.T) {
	cfg, err := Load(writeConfig(t, []byte(`{"REGISTRYURL":"https://registry.example","REQUESTTIMEOUTSECONDS":45}`)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistryURL != "https://registry.example" || cfg.RequestTimeoutSeconds != 45 {
		t.Fatal("case-insensitive fields were not bound")
	}
	for _, body := range []string{
		`{"registryUrl":"https://registry.example","POLLfallbackSeconds":null}`,
		`{"registryUrl":"https://registry.example","pollFallbackSeconds":300,"POLLFALLBACKSECONDS":600}`,
		`{"registryUrl":"https://registry.example","allowInsecureLoopback":false,"AllowInsecureLoopback":true}`,
	} {
		if _, err := Load(writeConfig(t, []byte(body))); err == nil {
			t.Fatal("accepted null or duplicate case-insensitive field")
		}
	}
}

func TestLoadSizeLimit(t *testing.T) {
	body := `{"registryUrl":"https://registry.example","futureSetting":""}`
	body = strings.Replace(body, `"futureSetting":""`, `"futureSetting":"`+strings.Repeat("x", (64<<10)-len(body))+`"`, 1)
	if _, err := Load(writeConfig(t, []byte(body))); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writeConfig(t, []byte(body+" "))); err == nil {
		t.Fatal("accepted oversized configuration with unknown field")
	}
}

func TestInvalidConfiguration(t *testing.T) {
	for _, body := range []string{
		`null`, `[]`, `{}`, `{} {}`, `{"registryUrl":`,
		`{"registryUrl":"https://registry.example","registryUrl":"https://other.example"}`,
		`{"registryUrl":"https://registry.example","RegistryUrl":"https://other.example"}`,
		`{"registryUrl":"https://registry.example","futureSetting":true,"futureSetting":null}`,
		`{"registryUrl":"https://registry.example","futureSetting":true,"future\u0053etting":null}`,
		`{"registryUrl":"https://registry.example","futureSetting":{"enabled":true,"enabled":false}}`,
		`{"registryUrl":"https://registry.example","futureSetting":[{"enabled":true,"enabled":false}]}`,
		`{"registryUrl":"https://registry.example","futureSetting":[1,]}`,
		`{"registryUrl":"https://registry.example","futureSetting":null} {}`,
		`{"registryUrl":"https://registry.example","futureSetting":` + strings.Repeat("[", 130) + `0` + strings.Repeat("]", 130) + `}`,
		"{\"registryUrl\":\"https://registry.example\",\"futureSetting\":\"\xff\"}",
		`{"registryUrl":"https://registry.example","pollFallbackSeconds":null}`,
		`{"registryUrl":"https://registry.example","pollFallbackSeconds":4}`,
		`{"registryUrl":"https://registry.example","pollFallbackSeconds":3601}`,
		`{"registryUrl":"https://registry.example","requestTimeoutSeconds":0}`,
		`{"registryUrl":"https://registry.example","requestTimeoutSeconds":301}`,
		`{"registryUrl":"https://registry.example","requestTimeoutSeconds":"30"}`,
		`{"registryUrl":"https://registry.example","configDir":"relative"}`,
		`{"registryUrl":"https://registry.example","apiKeyFile":""}`,
		`{"registryUrl":"https://registry.example","resultsDir":null}`,
		strings.Repeat(" ", (64<<10)+1),
	} {
		if _, err := Load(writeConfig(t, []byte(body))); err == nil {
			t.Error("accepted invalid service configuration")
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("accepted missing service configuration")
	}
	path := writeConfig(t, []byte(`{}`))
	data, _ := json.Marshal(map[string]any{"registryUrl": "https://registry.example", "configDir": path})
	if _, err := Load(writeConfig(t, data)); err == nil {
		t.Error("accepted a regular file as configDir")
	}
}

func TestRegistryURLPolicyAndPrivacy(t *testing.T) {
	secret := rand.Text()
	for _, test := range []struct {
		url   string
		allow bool
		valid bool
	}{
		{"https://registry.example", false, true},
		{"http://localhost:8080", true, true},
		{"http://127.0.0.1:8080", true, true},
		{"http://[::1]:8080", true, true},
		{"http://localhost", false, false},
		{"http://registry.example", true, false},
		{"http://127.0.0.1.example", true, false},
		{"http://192.0.2.1", true, false},
		{"ftp://localhost", true, false},
		{"https://registry.example:0", false, false},
		{"https://registry.example:65536", false, false},
		{"https://registry.example:", false, false},
		{"https://registry.example/v1", false, false},
		{"https://registry.example/?machineId=" + secret, false, false},
		{"https://registry.example/?", false, false},
		{"https://registry.example/#" + secret, false, false},
		{"https://user:" + secret + "@registry.example", false, false},
		{"://" + secret, false, false},
	} {
		data, _ := json.Marshal(map[string]any{"registryUrl": test.url, "allowInsecureLoopback": test.allow})
		_, err := Load(writeConfig(t, data))
		if (err == nil) != test.valid {
			t.Error("incorrect URL policy result")
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Error("URL validation leaked a value")
		}
	}
	data, _ := json.Marshal(map[string]any{"registryUrl": "https://registry.example", "requestTimeoutSeconds": secret, secret: secret})
	_, err := Load(writeConfig(t, data))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("field-type validation was not private")
	}
}

func TestReadAPIKey(t *testing.T) {
	key := "dscm_" + rand.Text() + "." + rand.Text()
	for _, suffix := range []string{"", "\n", "\r\n"} {
		actual, err := ReadAPIKey(writeConfig(t, []byte(key+suffix)))
		if err != nil || actual != key {
			t.Fatal("could not read machine credential")
		}
	}
	for _, data := range []string{
		"", strings.Replace(key, "dscm_", "dsca_", 1), " " + key, key + "\n\n",
		key + "\nextra", key + "\x00", "dscm_.value", "dscm_id.", strings.Repeat("x", 8193),
	} {
		_, err := ReadAPIKey(writeConfig(t, []byte(data)))
		if err == nil || strings.Contains(err.Error(), key) {
			t.Error("invalid key not rejected privately")
		}
	}
	if _, err := ReadAPIKey(t.TempDir()); err == nil {
		t.Error("accepted a directory as key file")
	}
}
