package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FromFile loads a config file (TOML subset or JSON) into a flat Source.
// Supported extensions: .toml, .json, .jsonc. Unknown file types are an error.
func FromFile(path string) (Source, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".toml":
		return parseTOML(string(data))
	case ".json", ".jsonc":
		return parseJSON(data)
	default:
		return nil, fmt.Errorf("unsupported config file type %q (want .toml, .json or .jsonc)", ext)
	}
}

// parseJSON decodes a JSON/JSONC object and flattens nested objects into
// dotted keys. Trailing commas and // comments are tolerated (JSONC).
func parseJSON(data []byte) (Source, error) {
	cleaned, err := stripJSONC(data)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := json.Unmarshal(cleaned, &root); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	src := Source{}
	flattenMap(root, "", src)
	return src, nil
}

func flattenMap(m map[string]any, prefix string, out Source) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// deterministic order not required; iterate as-is
	for k, v := range m {
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		if nested, ok := v.(map[string]any); ok {
			flattenMap(nested, full, out)
			continue
		}
		out[full] = v
	}
}

// stripJSONC removes // and /* */ comments and trailing commas, keeping string
// literals intact. A small state machine sufficient for config files.
func stripJSONC(data []byte) ([]byte, error) {
	var out strings.Builder
	inStr, esc := false, false
	lineComment, blockComment := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case blockComment:
			if c == '*' && i+1 < len(data) && data[i+1] == '/' {
				blockComment = false
				i++
			}
			continue
		case lineComment:
			if c == '\n' {
				lineComment = false
				out.WriteByte('\n')
			}
			continue
		case inStr:
			out.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		case c == '"':
			inStr = true
			out.WriteByte(c)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			lineComment = true
			i++
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			blockComment = true
			i++
		case c == ',':
			// drop trailing commas: a comma followed by whitespace then } or ]
			j := i + 1
			for j < len(data) && (data[j] == ' ' || data[j] == '\t' || data[j] == '\n' || data[j] == '\r') {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue // skip comma
			}
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	if inStr {
		return nil, fmt.Errorf("unterminated string in JSON config")
	}
	return []byte(out.String()), nil
}

// parseTOML parses a minimal TOML subset sufficient for fuji's settings:
// sections ([model], [timeouts], [tools], [retry], [compaction], [paths]),
// key = value pairs with string (basic and literal), integer, float, boolean,
// and array values, # comments. No dotted keys, no inline tables, no
// multi-line strings. Anything else is an error — we fail loud rather than
// silently mis-read a fleet config.
func parseTOML(text string) (Source, error) {
	src := Source{}
	section := ""
	sc := bufio.NewScanner(strings.NewReader(text))
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip trailing comments that are not inside quotes.
		line = stripTOMLComment(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			switch section {
			case "model", "timeouts", "tools", "retry", "compaction", "paths":
			default:
				return nil, fmt.Errorf("line %d: unknown section [%s]", lineNo, section)
			}
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected key = value", lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		rawVal := strings.TrimSpace(line[eq+1:])
		if key == "" || rawVal == "" {
			return nil, fmt.Errorf("line %d: empty key or value", lineNo)
		}
		val, err := parseTOMLValue(rawVal)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", lineNo, err)
		}
		full := key
		if section != "" {
			full = section + "." + key
		}
		src[full] = val
	}
	return src, sc.Err()
}

// stripTOMLComment removes a trailing # comment outside of quotes.
func stripTOMLComment(line string) string {
	inStr, esc := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = true
		case '#':
			return strings.TrimSpace(line[:i])
		}
	}
	return strings.TrimSpace(line)
}

func parseTOMLValue(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty value")
	}
	// Array.
	if strings.HasPrefix(raw, "[") {
		if !strings.HasSuffix(raw, "]") {
			return nil, fmt.Errorf("unterminated array: %s", raw)
		}
		inner := strings.TrimSpace(raw[1 : len(raw)-1])
		if inner == "" {
			return []any{}, nil
		}
		parts := splitTOMLArray(inner)
		arr := make([]any, 0, len(parts))
		for _, p := range parts {
			v, err := parseTOMLValue(strings.TrimSpace(p))
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	}
	// Quoted strings (basic or literal).
	if (raw[0] == '"' && raw[len(raw)-1] == '"') || (raw[0] == '\'' && raw[len(raw)-1] == '\'') {
		if raw[0] == '\'' {
			return raw[1 : len(raw)-1], nil
		}
		return strconv.Unquote(raw)
	}
	// Booleans.
	if raw == "true" {
		return true, nil
	}
	if raw == "false" {
		return false, nil
	}
	// Integers.
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n, nil
	}
	// Floats.
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f, nil
	}
	return nil, fmt.Errorf("cannot parse value %q", raw)
}

// splitTOMLArray splits an array on top-level commas (no nesting support).
func splitTOMLArray(inner string) []string {
	var parts []string
	start := 0
	inStr, esc := false, false
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' || c == '\'' {
				inStr = false
			}
			continue
		}
		if c == '"' || c == '\'' {
			inStr = true
			continue
		}
		if c == ',' {
			parts = append(parts, inner[start:i])
			start = i + 1
		}
	}
	parts = append(parts, inner[start:])
	return parts
}
