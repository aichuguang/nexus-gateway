package statecheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"

	"w103d/gateway/internal/model"
)

func decodeStrict(data []byte, state *model.State) error {
	return decodeStrictVersion(data, state, false)
}

func decodeStrictVersion(data []byte, state *model.State, allowLegacy bool) error {
	if !utf8.Valid(data) {
		return errors.New("state must be UTF-8 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := checkDuplicates(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("state must contain exactly one JSON value with no trailing content")
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(data, &header); err != nil || header == nil {
		return errors.New("state must be a JSON object")
	}
	var version int
	if value, present := header["schemaVersion"]; !present || json.Unmarshal(value, &version) != nil || version == 0 {
		return errors.New("schemaVersion is required and must be a nonzero integer")
	}
	if version != model.SchemaVersion && !(allowLegacy && (version == 4 || version == 5)) {
		return fmt.Errorf("unsupported schemaVersion; this build requires version %d; no migration was performed", model.SchemaVersion)
	}
	if err := checkShape(data, reflect.TypeOf(model.State{}), "state"); err != nil {
		return err
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(state); err != nil {
		// Never echo an unknown field, string or raw provider value from a file.
		return errors.New("state contains invalid JSON field types")
	}
	return model.UpgradeSchema(state)
}

// encoding/json accepts duplicate object keys and case-insensitive field names.
// Neither is safe for a schema preflight: the operator needs one interpretation
// of each field, not whichever alias or duplicate happens to win at runtime.
func checkDuplicates(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting exceeds the supported limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return errors.New("invalid or truncated JSON")
	}
	delimiter, nested := token.(json.Delim)
	if !nested {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok {
				return errors.New("invalid JSON object")
			}
			if seen[name] {
				return errors.New("duplicate JSON object key; values were not merged")
			}
			seen[name] = true
			if err := checkDuplicates(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := checkDuplicates(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON container")
	}
	if _, err := decoder.Token(); err != nil {
		return errors.New("invalid or truncated JSON container")
	}
	return nil
}

// Field paths are constructed only from model tags and array indices, never
// from untrusted keys or values. Required fields follow the model's json tags;
// omitempty fields may be absent, and slices may retain their valid null form.
func checkShape(data []byte, expected reflect.Type, location string) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if expected.Kind() == reflect.Pointer || expected.Kind() == reflect.Slice {
			return nil
		}
		return fmt.Errorf("%s must not be null", location)
	}
	if expected.Kind() == reflect.Pointer {
		return checkShape(data, expected.Elem(), location)
	}
	switch expected.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if json.Unmarshal(data, &object) != nil {
			return fmt.Errorf("%s must be an object", location)
		}
		known := map[string]bool{}
		for i := 0; i < expected.NumField(); i++ {
			field := expected.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")
			name := tag[0]
			if name == "-" || field.PkgPath != "" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			known[name] = true
			value, present := object[name]
			optional := false
			for _, option := range tag[1:] {
				optional = optional || option == "omitempty"
			}
			if !present {
				if !optional {
					return fmt.Errorf("%s.%s is required", location, name)
				}
				continue
			}
			if err := checkShape(value, field.Type, location+"."+name); err != nil {
				return err
			}
		}
		for name := range object {
			if !known[name] {
				return fmt.Errorf("%s contains an unknown or incorrectly cased field", location)
			}
		}
	case reflect.Slice:
		var array []json.RawMessage
		if json.Unmarshal(data, &array) != nil {
			return fmt.Errorf("%s must be an array", location)
		}
		for index, value := range array {
			if err := checkShape(value, expected.Elem(), fmt.Sprintf("%s[%d]", location, index)); err != nil {
				return err
			}
		}
	default:
		if json.Unmarshal(data, reflect.New(expected).Interface()) != nil {
			return fmt.Errorf("%s has an invalid %s value", location, expected.Kind())
		}
	}
	return nil
}
