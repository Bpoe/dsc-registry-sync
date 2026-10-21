package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Bpoe/dsc-registry-sync/internal/config"
	"github.com/Bpoe/dsc-registry-sync/internal/jsondoc"
)

const MaxRegistrationBytes = 16 << 10

type Registration struct {
	ID     string
	Name   string
	APIKey string
}

// ValidMachineName follows Registry's display-name rules, not configuration stems.
func ValidMachineName(name string) bool {
	if len(name) == 0 || len(name) > 256 || !utf8.ValidString(name) || strings.TrimSpace(name) != name {
		return false
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// Register makes exactly one attempt: a lost response may already have created
// the machine, so retry/recovery must be an explicit administrator decision.
func Register(ctx context.Context, cfg config.Config, name, token string) (Registration, error) {
	if err := cfg.Validate(); err != nil {
		return Registration{}, err
	}
	if !ValidMachineName(name) {
		return Registration{}, errors.New("machine name must be 1-256 UTF-8 bytes without surrounding whitespace or control characters")
	}
	if !ValidRegistrationToken(token) {
		return Registration{}, errors.New("missing or invalid registration token")
	}
	client := NewClient(cfg.RegistryURL, "", time.Duration(cfg.RequestTimeoutSeconds)*time.Second)
	defer client.Close()
	body, _ := json.Marshal(struct {
		Name string `json:"name"`
	}{name})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.RegistryURL+"/v1/agent/register", bytes.NewReader(body))
	if err != nil {
		return Registration{}, errors.New("cannot construct registration request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	res, err := client.do(request)
	if err != nil {
		return Registration{}, fmt.Errorf("%w; registration outcome is uncertain; ask an administrator before retrying", err)
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return Registration{}, errors.New("registration token is invalid or expired (HTTP 401)")
	case res.StatusCode == http.StatusConflict:
		return Registration{}, errors.New("machine name is already registered (HTTP 409); administrator-assisted recovery may be required")
	case res.StatusCode >= 300 && res.StatusCode < 400:
		return Registration{}, errors.New("Registry registration redirect refused")
	case res.StatusCode >= 500:
		return Registration{}, fmt.Errorf("Registry registration failed (HTTP %d); ask an administrator to check registration before retrying", res.StatusCode)
	case res.StatusCode != http.StatusCreated:
		return Registration{}, fmt.Errorf("Registry rejected registration (HTTP %d)", res.StatusCode)
	}
	invalid := errors.New("invalid or incomplete Registry registration response; the machine may already be registered; administrator-assisted recovery may be required")
	mediaType, params, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" ||
		params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8") {
		return Registration{}, invalid
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxRegistrationBytes+1))
	if err != nil || len(data) > MaxRegistrationBytes || jsondoc.Validate(data) != nil {
		return Registration{}, invalid
	}
	var fields map[string]json.RawMessage
	var result Registration
	if json.Unmarshal(data, &fields) != nil ||
		json.Unmarshal(fields["id"], &result.ID) != nil ||
		json.Unmarshal(fields["name"], &result.Name) != nil ||
		json.Unmarshal(fields["apiKey"], &result.APIKey) != nil ||
		!validRevision(result.ID) || result.ID == "00000000-0000-0000-0000-000000000000" ||
		result.Name != name || config.ValidateAPIKey(result.APIKey) != nil {
		return Registration{}, invalid
	}
	return result, nil
}

func ValidRegistrationToken(token string) bool {
	if len(token) == 0 || len(token) > 8192 {
		return false
	}
	for _, c := range []byte(token) {
		if c <= ' ' || c >= 127 {
			return false
		}
	}
	return true
}
