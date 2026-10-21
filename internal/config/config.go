// Package config loads the daemon's local configuration and machine credential.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/Bpoe/dsc-registry-sync/internal/jsondoc"
)

type Config struct {
	RegistryURL           string `json:"registryUrl"`
	APIKeyFile            string `json:"apiKeyFile"`
	ConfigDir             string `json:"configDir"`
	ResultsDir            string `json:"resultsDir"`
	RequestTimeoutSeconds int    `json:"requestTimeoutSeconds"`
	PollFallbackSeconds   int    `json:"pollFallbackSeconds"`
}

func Defaults() Config {
	base := "/etc/dsc"
	results := "/var/lib/dsc/results.d"
	if runtime.GOOS == "windows" {
		programData := os.Getenv("ProgramData")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		base = filepath.Join(programData, "dsc")
		results = filepath.Join(base, "results.d")
	}
	return Config{
		APIKeyFile:            filepath.Join(base, "dscregsyncd.key"),
		ConfigDir:             filepath.Join(base, "config.d"),
		ResultsDir:            results,
		RequestTimeoutSeconds: 30,
		PollFallbackSeconds:   300,
	}
}

func DefaultPath() string {
	return filepath.Join(filepath.Dir(Defaults().APIKeyFile), "dscregsyncd.json")
}

func Load(path string) (Config, error) {
	data, err := readFile(path, 64<<10)
	if err != nil {
		return Config{}, fmt.Errorf("read service configuration: %w", err)
	}
	if jsondoc.Validate(data) != nil {
		return Config{}, errors.New("service configuration must be valid, unambiguous JSON")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return Config{}, errors.New("service configuration must be an object")
	}
	cfg := Defaults()
	// Match encoding/json's case-insensitive field binding without a separate allowlist.
	configType := reflect.TypeOf(cfg)
	for i := range configType.NumField() {
		name := configType.Field(i).Tag.Get("json")
		found := false
		for key, v := range fields {
			if !strings.EqualFold(key, name) {
				continue
			}
			if found {
				return Config{}, errors.New("service configuration contains duplicate fields")
			}
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return Config{}, errors.New("service configuration contains null recognized fields")
			}
			found = true
		}
	}
	if json.Unmarshal(data, &cfg) != nil {
		return Config{}, errors.New("service configuration contains invalid field types")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks and normalizes configuration, including the Registry origin.
func (c *Config) Validate() error {
	u, err := url.Parse(c.RegistryURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || strings.Contains(c.RegistryURL, "#") ||
		u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return errors.New("registryUrl must be an HTTP or HTTPS origin without credentials, path, query or fragment")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("registryUrl contains an invalid port")
		}
	}
	if strings.HasSuffix(u.Host, ":") {
		return errors.New("registryUrl contains an empty port")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("registryUrl requires HTTP or HTTPS")
	}
	u.Path = ""
	c.RegistryURL = u.String()
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"apiKeyFile", &c.APIKeyFile},
		{"configDir", &c.ConfigDir},
		{"resultsDir", &c.ResultsDir},
	} {
		if !filepath.IsAbs(*field.value) || strings.ContainsRune(*field.value, 0) {
			return fmt.Errorf("%s must be an absolute filesystem path", field.name)
		}
		*field.value = filepath.Clean(*field.value)
	}
	for _, dir := range []string{c.ConfigDir, c.ResultsDir} {
		info, err := os.Stat(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("cannot inspect configured directories")
		}
		if err == nil && !info.IsDir() {
			return errors.New("configDir and resultsDir must identify directories")
		}
	}
	if c.RequestTimeoutSeconds < 1 || c.RequestTimeoutSeconds > 300 {
		return errors.New("requestTimeoutSeconds must be between 1 and 300")
	}
	if c.PollFallbackSeconds < 5 || c.PollFallbackSeconds > 3600 {
		return errors.New("pollFallbackSeconds must be between 5 and 3600")
	}
	return nil
}

// ReadAPIKey reads only at startup; rotation requires restarting the daemon.
func ReadAPIKey(path string) (string, error) {
	data, err := readFile(path, 8192)
	if err != nil {
		return "", fmt.Errorf("read machine key: %w", err)
	}
	key := strings.TrimSuffix(string(data), "\n")
	key = strings.TrimSuffix(key, "\r")
	if err := ValidateAPIKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// ValidateAPIKey accepts the same machine credential in files and enrollment responses.
func ValidateAPIKey(key string) error {
	if len(key) > 8192 {
		return errors.New("machine key exceeds size limit")
	}
	id, secret, ok := strings.Cut(key, ".")
	if !ok || !strings.HasPrefix(id, "dscm_") || len(id) <= len("dscm_") || secret == "" {
		return errors.New("key file must contain a machine API key")
	}
	for _, part := range []string{strings.TrimPrefix(id, "dscm_"), secret} {
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
				c >= '0' && c <= '9' || c == '_' || c == '-') {
				return errors.New("machine key contains invalid characters")
			}
		}
	}
	return nil
}

func readFile(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, errors.New("cannot inspect file (check its path and permissions)")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("file must be a readable regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open file (check its path and permissions)")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("file must be a readable regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, errors.New("cannot read file")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
}
