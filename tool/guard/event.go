package guard

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// Event is one hook event: the JSON object Claude Code writes on stdin.
type Event struct {
	fields map[string]json.RawMessage
}

// ParseEvent reads a hook event. It reports false for input that is not valid
// UTF-8 JSON or not a JSON object, which every hook allows.
func ParseEvent(data []byte) (*Event, bool) {
	if !utf8.Valid(data) {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return nil, false
	}
	return &Event{fields}, true
}

func decode(raw json.RawMessage) any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

// Value is a top-level field, with numbers kept as json.Number. A JSON null is present with a nil value.
func (e *Event) Value(key string) (any, bool) {
	raw, ok := e.fields[key]
	if !ok {
		return nil, false
	}
	return decode(raw), true
}

// Str is a string field, or "" when it is absent or not a string.
func (e *Event) Str(key string) string {
	s, _ := decodeString(e.fields[key])
	return s
}

// fieldString is a string field and whether it is one.
func (e *Event) fieldString(key string) (string, bool) { return decodeString(e.fields[key]) }

func decodeString(raw json.RawMessage) (string, bool) {
	var s string
	if raw == nil || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Object is a top-level field that is a JSON object.
func (e *Event) Object(key string) (map[string]any, bool) {
	m, ok := e.Value(key)
	obj, isObj := m.(map[string]any)
	return obj, ok && isObj
}

// pyStr is Python's str() of a decoded JSON value.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		if f, err := t.Float64(); err == nil && bytes.ContainsAny([]byte(t), ".eE") {
			return pyFloat(f, t.String())
		}
		return t.String()
	}
	return pyDumpValue(v)
}

// truthy is Python's truth value of a decoded JSON value.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := strconv.ParseFloat(t.String(), 64)
		return err != nil || f != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}
