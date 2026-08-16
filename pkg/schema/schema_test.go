package schema

import (
	"encoding/json"
	"testing"
)

func TestValidateObject(t *testing.T) {
	sch := Object([]string{"path"}, map[string]Schema{
		"path":      Str("file path"),
		"offset":    Int("byte offset"),
		"recursive": Bool("recurse"),
		"tags":      Arr("tags", Str("")),
		"mode":      Enum("mode", "a", "b"),
	})

	ok := `{"path":"x","offset":10,"recursive":true,"tags":["t1"],"mode":"a"}`
	if err := ValidateRaw(json.RawMessage(ok), sch); err != nil {
		t.Errorf("expected ok: %v", err)
	}

	if err := ValidateRaw(json.RawMessage(`{"offset":10}`), sch); err == nil {
		t.Error("expected missing-required error")
	}

	if err := ValidateRaw(json.RawMessage(`{"path":"x","unknown":1}`), sch); err == nil {
		t.Error("expected additional-property error")
	}

	if err := ValidateRaw(json.RawMessage(`{"path":5}`), sch); err == nil {
		t.Error("expected type error")
	}

	if err := ValidateRaw(json.RawMessage(`{"path":"x","mode":"c"}`), sch); err == nil {
		t.Error("expected enum error")
	}

	if err := ValidateRaw(json.RawMessage(`{"path":"x","offset":"notanumber"}`), sch); err == nil {
		t.Error("expected nested type error")
	}
}

func TestValidateArrayItems(t *testing.T) {
	sch := Object(nil, map[string]Schema{"list": Arr("", Str(""))})
	if err := ValidateRaw(json.RawMessage(`{"list":["a", 5]}`), sch); err == nil {
		t.Error("expected item type error")
	}
	if err := ValidateRaw(json.RawMessage(`{"list":["a","b"]}`), sch); err != nil {
		t.Errorf("expected ok: %v", err)
	}
}

func TestValidateNullArgs(t *testing.T) {
	sch := Object(nil, map[string]Schema{"optional": Str("")})
	if err := ValidateRaw(json.RawMessage(`null`), sch); err != nil {
		t.Errorf("null args should validate against non-required object: %v", err)
	}
	if err := ValidateRaw(nil, sch); err != nil {
		t.Errorf("empty args should validate: %v", err)
	}
}

func TestSchemaRaw(t *testing.T) {
	sch := Object([]string{"path"}, map[string]Schema{"path": Str("p")})
	raw, err := sch.Raw()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "object" || m["additionalProperties"] != false {
		t.Errorf("schema = %v", m)
	}
}
