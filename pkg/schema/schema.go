// Package schema implements a small JSON-Schema validator sufficient for tool
// parameter schemas (core-spec §4.5): object/string/number/integer/boolean/
// array types, properties, required, items, enum, and additionalProperties.
// Schemas are expressed as plain Go maps (json.RawMessage) so they serialize
// directly into provider requests.
package schema

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Schema is a JSON Schema document (draft-07 subset).
type Schema map[string]any

// --- builders --------------------------------------------------------------

// Object builds an object schema with the given required properties.
func Object(required []string, props map[string]Schema) Schema {
	p := make(map[string]any, len(props))
	for k, v := range props {
		p[k] = v
	}
	req := make([]any, len(required))
	for i, r := range required {
		req[i] = r
	}
	return Schema{
		"type":                 "object",
		"properties":           p,
		"required":             req,
		"additionalProperties": false,
	}
}

// Str builds a string property schema.
func Str(desc string) Schema {
	s := Schema{"type": "string"}
	if desc != "" {
		s["description"] = desc
	}
	return s
}

// Int builds an integer property schema.
func Int(desc string) Schema {
	s := Schema{"type": "integer"}
	if desc != "" {
		s["description"] = desc
	}
	return s
}

// Num builds a number property schema.
func Num(desc string) Schema {
	s := Schema{"type": "number"}
	if desc != "" {
		s["description"] = desc
	}
	return s
}

// Bool builds a boolean property schema.
func Bool(desc string) Schema {
	s := Schema{"type": "boolean"}
	if desc != "" {
		s["description"] = desc
	}
	return s
}

// Arr builds an array property schema.
func Arr(desc string, items Schema) Schema {
	s := Schema{"type": "array", "items": items}
	if desc != "" {
		s["description"] = desc
	}
	return s
}

// Enum builds a string enum schema.
func Enum(desc string, values ...string) Schema {
	vals := make([]any, len(values))
	for i, v := range values {
		vals[i] = v
	}
	s := Schema{"type": "enum", "enum": vals}
	if desc != "" {
		s["description"] = desc
	}
	return s
}

// Raw marshals the schema to json.RawMessage for tool definitions.
func (s Schema) Raw() (json.RawMessage, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// --- validation ------------------------------------------------------------

// ValidationError describes a failed validation with a JSON pointer path.
type ValidationError struct {
	Path string // e.g. "$.command"
	Msg  string
}

func (e *ValidationError) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Msg)
}

// Validate checks value (a decoded JSON value) against the schema.
func Validate(value any, schema Schema) error {
	return validateAt("$", value, schema)
}

// ValidateRaw decodes data and validates it.
func ValidateRaw(data json.RawMessage, schema Schema) error {
	var v any
	if len(data) == 0 || string(data) == "null" {
		v = nil
	} else if err := json.Unmarshal(data, &v); err != nil {
		return &ValidationError{Path: "$", Msg: fmt.Sprintf("arguments are not valid JSON: %v", err)}
	}
	return Validate(v, schema)
}

func validateAt(path string, value any, s Schema) error {
	if s == nil {
		return nil
	}
	typ, _ := s["type"].(string)
	// Tool calls may omit arguments entirely; treat null as an empty object.
	if typ == "object" && value == nil {
		value = map[string]any{}
	}
	// Type check.
	if typ != "" && typ != "enum" {
		if err := checkType(path, value, typ); err != nil {
			return err
		}
	}
	// Enum.
	if en, ok := s["enum"].([]any); ok {
		for _, e := range en {
			if jsonEqual(e, value) {
				return nil
			}
		}
		return &ValidationError{Path: path, Msg: fmt.Sprintf("value %v is not one of %v", value, en)}
	}
	switch typ {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return &ValidationError{Path: path, Msg: "expected an object"}
		}
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				name, _ := r.(string)
				if _, present := obj[name]; !present {
					return &ValidationError{Path: path, Msg: fmt.Sprintf("missing required property %q", name)}
				}
			}
		}
		if add, ok := s["additionalProperties"].(bool); ok && !add {
			for k := range obj {
				if props == nil {
					return &ValidationError{Path: path, Msg: fmt.Sprintf("additional property %q not allowed", k)}
				}
				if _, known := props[k]; !known {
					return &ValidationError{Path: path, Msg: fmt.Sprintf("additional property %q not allowed", k)}
				}
			}
		}
		// Sort property names for deterministic error reporting.
		names := make([]string, 0, len(obj))
		for k := range obj {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if sub, ok := asSchema(props[k]); ok {
				if err := validateAt(path+"."+k, obj[k], sub); err != nil {
					return err
				}
			}
		}
	case "array":
		arr, ok := value.([]any)
		if !ok {
			return &ValidationError{Path: path, Msg: "expected an array"}
		}
		if items, ok := asSchema(s["items"]); ok {
			for i, item := range arr {
				if err := validateAt(fmt.Sprintf("%s[%d]", path, i), item, items); err != nil {
					return err
				}
			}
		}
	case "string", "number", "integer", "boolean":
		// Additional constraints.
		if v, ok := s["minLength"].(float64); ok {
			if str, isStr := value.(string); isStr && len(str) < int(v) {
				return &ValidationError{Path: path, Msg: fmt.Sprintf("string shorter than minLength %d", int(v))}
			}
		}
		if v, ok := s["maxLength"].(float64); ok {
			if str, isStr := value.(string); isStr && len(str) > int(v) {
				return &ValidationError{Path: path, Msg: fmt.Sprintf("string longer than maxLength %d", int(v))}
			}
		}
		if v, ok := s["minimum"].(float64); ok {
			if num, isNum := value.(float64); isNum && num < v {
				return &ValidationError{Path: path, Msg: fmt.Sprintf("number %v below minimum %v", num, v)}
			}
		}
		if v, ok := s["maximum"].(float64); ok {
			if num, isNum := value.(float64); isNum && num > v {
				return &ValidationError{Path: path, Msg: fmt.Sprintf("number %v above maximum %v", num, v)}
			}
		}
	}
	return nil
}

func checkType(path string, value any, typ string) error {
	ok := false
	switch typ {
	case "object":
		_, ok = value.(map[string]any)
	case "array":
		_, ok = value.([]any)
	case "string":
		_, ok = value.(string)
	case "integer":
		f, isNum := value.(float64)
		ok = isNum && f == float64(int64(f))
	case "number":
		_, ok = value.(float64)
	case "boolean":
		_, ok = value.(bool)
	case "null":
		ok = value == nil
	default:
		return &ValidationError{Path: path, Msg: fmt.Sprintf("unsupported schema type %q", typ)}
	}
	if !ok {
		return &ValidationError{Path: path, Msg: fmt.Sprintf("expected type %q, got %s", typ, typeName(value))}
	}
	return nil
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// asSchema coerces a value that may be a Schema or a plain map into a Schema.
func asSchema(v any) (Schema, bool) {
	switch t := v.(type) {
	case Schema:
		return t, true
	case map[string]any:
		return Schema(t), true
	default:
		return nil, false
	}
}

// jsonEqual deep-compares two decoded JSON values.
func jsonEqual(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

// ErrorList is a collection of validation errors.
type ErrorList []*ValidationError

// Error implements error.
func (l ErrorList) Error() string {
	msgs := make([]string, len(l))
	for i, e := range l {
		msgs[i] = e.Error()
	}
	return strings.Join(msgs, "; ")
}
