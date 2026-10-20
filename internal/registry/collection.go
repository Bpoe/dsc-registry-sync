package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Bpoe/dsc-registry-sync/internal/jsondoc"
)

const (
	MaxCollectionBytes = 16 << 20
	MaxDocumentBytes   = 1 << 20
)

type Configuration struct {
	Name       string          `json:"name"`
	Revision   string          `json:"revision"`
	Template   json.RawMessage `json:"template"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

type Collection struct {
	ConfigurationRevision int64           `json:"configurationRevision"`
	Configurations        []Configuration `json:"configurations"`
}

func decodeCollection(data []byte) (*Collection, error) {
	invalid := errors.New("invalid desired-state collection")
	if len(data) > MaxCollectionBytes || jsondoc.Validate(data) != nil {
		return nil, invalid
	}
	// Exact required property names prevent case-insensitive struct matching
	// from accepting a malformed authoritative collection.
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return nil, invalid
	}
	var revision *int64
	var units []json.RawMessage
	if json.Unmarshal(fields["configurationRevision"], &revision) != nil ||
		revision == nil || *revision < 0 ||
		json.Unmarshal(fields["configurations"], &units) != nil || units == nil {
		return nil, invalid
	}
	collection := &Collection{
		ConfigurationRevision: *revision,
		Configurations:        make([]Configuration, 0, len(units)),
	}
	names := make(map[string]bool, len(units))
	for _, raw := range units {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return nil, invalid
		}
		var unit Configuration
		if json.Unmarshal(fields["name"], &unit.Name) != nil ||
			json.Unmarshal(fields["revision"], &unit.Revision) != nil ||
			!ValidName(unit.Name) || !validRevision(unit.Revision) || names[unit.Name] {
			return nil, invalid
		}
		unit.Template = fields["template"]
		unit.Parameters = fields["parameters"]
		if bytes.Equal(bytes.TrimSpace(unit.Parameters), []byte("null")) {
			unit.Parameters = nil
		}
		if !object(unit.Template) || len(unit.Parameters) != 0 && !object(unit.Parameters) ||
			len(unit.Template)+len(unit.Parameters) > MaxDocumentBytes {
			return nil, invalid
		}
		names[unit.Name] = true
		collection.Configurations = append(collection.Configurations, unit)
	}
	return collection, nil
}

func object(data []byte) bool {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	_, format := fields["format"]
	_, content := fields["content"]
	return !(format && content)
}

// ValidName uses Registry's stem grammar and additionally rejects Windows
// device names on both platforms so the same collection is safe everywhere.
func ValidName(name string) bool {
	if len(name) == 0 || len(name) > 64 || name[len(name)-1] == '.' ||
		strings.HasSuffix(name, ".parameters") {
		return false
	}
	for i, c := range []byte(name) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		if i == 0 || c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	base, _, _ := strings.Cut(name, ".")
	switch base {
	case "con", "prn", "aux", "nul":
		return false
	}
	return !(len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) &&
		base[3] >= '1' && base[3] <= '9')
}

func validRevision(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range []byte(value) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}
