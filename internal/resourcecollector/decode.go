// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"
)

// DecodeQuery requires one bounded object, unique keys and the exact DTO shape.
// JSON null must not silently turn a request into the struct's zero value.
func DecodeQuery[T any](input []byte) (T, error) {
	var value T
	if len(input) == 0 || len(input) > 4096 || !utf8.Valid(input) {
		return value, ErrInvalidQuery
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return value, ErrInvalidQuery
	}
	keys := make(map[string]bool)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return value, ErrInvalidQuery
		}
		key, ok := keyToken.(string)
		if !ok || keys[key] {
			return value, ErrInvalidQuery
		}
		keys[key] = true
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return value, ErrInvalidQuery
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return value, ErrInvalidQuery
	}
	switch any(&value).(type) {
	case *ProcessSelector:
		if !keys["pid"] {
			return value, ErrInvalidQuery
		}
	case *ProcessGroupQuery:
		if !keys["rootProcessKey"] {
			return value, ErrInvalidQuery
		}
	}
	decoder = json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil {
		return value, ErrInvalidQuery
	}
	return value, nil
}
