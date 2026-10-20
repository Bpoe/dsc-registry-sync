// Package jsondoc checks bounded, unambiguous JSON without changing document bytes.
package jsondoc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// Validate rejects duplicate properties, invalid UTF-8, trailing values and
// nesting beyond the Registry's 128-level limit. Errors never include input.
func Validate(data []byte) error {
	invalid := errors.New("invalid or ambiguous JSON document")
	if !utf8.Valid(data) {
		return invalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if !value(d, 0) {
		return invalid
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid
	}
	return nil
}

func value(d *json.Decoder, depth int) bool {
	if depth > 128 {
		return false
	}
	token, err := d.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case '{':
		keys := make(map[string]bool)
		for d.More() {
			token, err := d.Token()
			key, ok := token.(string)
			if err != nil || !ok || keys[key] {
				return false
			}
			keys[key] = true
			if !value(d, depth+1) {
				return false
			}
		}
		token, err = d.Token()
		return err == nil && token == json.Delim('}')
	case '[':
		for d.More() {
			if !value(d, depth+1) {
				return false
			}
		}
		token, err = d.Token()
		return err == nil && token == json.Delim(']')
	default:
		return false
	}
}
