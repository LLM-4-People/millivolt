// Package adminjson owns the JSON trust boundary for operator mutations.
// It is intentionally separate from the transparent proxy request decoder.
package adminjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
)

const (
	// Administrative documents are small. These are abuse/stack safety
	// guardrails, not proxy request limits or user-tunable settings.
	maxBodyBytes = 1 << 20
	maxDepth     = 128
)

// Decode accepts exactly one non-null JSON object. Unknown struct fields,
// duplicate keys (including case aliases of struct fields), trailing data and
// oversized documents are rejected before dst is used by the caller. Only a
// genuinely empty body returns io.EOF; whitespace is not an empty-body command.
// Decode does not write an HTTP response. dst must be a pointer to an object.
func Decode(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil {
		return io.EOF
	}
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		// EOF is an operator command only when the body was successfully read
		// and empty. Never expose wrapped transport EOF as that sentinel.
		return fmt.Errorf("read JSON body: %v", err)
	}
	return Unmarshal(raw, dst)
}

// Unmarshal applies the same strict object boundary to already-bounded control
// data, including proxy header-injection documents. Only zero bytes mean EOF.
func Unmarshal(raw []byte, dst any) error {
	if len(raw) == 0 {
		return io.EOF
	}
	if len(raw) > maxBodyBytes {
		return fmt.Errorf("JSON object too large")
	}
	raw = bytes.Trim(raw, " \t\r\n")
	if len(raw) == 0 || raw[0] != '{' {
		return fmt.Errorf("JSON object required")
	}
	check := json.NewDecoder(bytes.NewReader(raw))
	check.UseNumber()
	if err := validateValue(check, reflect.TypeOf(dst), 0); err != nil {
		return err
	}
	if _, err := check.Token(); err != io.EOF {
		return fmt.Errorf("exactly one JSON object required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// OptionalID distinguishes an omitted scope from an invalid explicit scope.
// Only omission means "all" for stop/resume handlers; null/blank must never
// widen a malformed single-item operation into a global one.
func OptionalID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil || strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("id must be a non-empty string")
	}
	return strings.TrimSpace(id), nil
}

func concreteType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func validateValue(dec *json.Decoder, typ reflect.Type, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("JSON nesting too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %v", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	typ = concreteType(typ)
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return fmt.Errorf("invalid JSON key: %v", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key required")
			}
			identity := key
			var child reflect.Type
			if typ != nil {
				switch typ.Kind() {
				case reflect.Struct:
					// encoding/json accepts case-insensitive field aliases. Track
					// those together while keeping arbitrary map keys case-sensitive.
					for i := 0; i < typ.NumField(); i++ {
						field := typ.Field(i)
						name := strings.Split(field.Tag.Get("json"), ",")[0]
						if name == "" {
							name = field.Name
						}
						if field.IsExported() && name != "-" && strings.EqualFold(name, key) {
							identity, child = name, field.Type
							break
						}
					}
				case reflect.Map:
					child = typ.Elem()
				}
			}
			if _, exists := seen[identity]; exists {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[identity] = struct{}{}
			if err := validateValue(dec, child, depth+1); err != nil {
				return err
			}
		}
	case '[':
		var child reflect.Type
		if typ != nil && (typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array) {
			child = typ.Elem()
		}
		for dec.More() {
			if err := validateValue(dec, child, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("invalid JSON delimiter")
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("invalid JSON: %v", err)
	}
	return nil
}
