package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// A pane pass runs pi as an interactive agent; its reply is read from the
// session file pi saves, whose lines are events of this shape.

type piEvent struct {
	Type    string `json:"type"`
	Message struct {
		Role     string          `json:"role"`
		ToolName string          `json:"toolName"`
		Content  json.RawMessage `json:"content"`
		Usage    struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			CacheRead int `json:"cacheRead"`
		} `json:"usage"`
	} `json:"message"`
}

// text joins the text parts of an assistant message; other roles carry a
// string or other shapes and give "".
func (e *piEvent) text() string {
	if e.Message.Role != "assistant" {
		return ""
	}
	var parts []struct{ Type, Text string }
	if json.Unmarshal(e.Message.Content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// eachPiEvent calls fn for every well-formed JSONL line of r; other lines are
// skipped. Lines can be hundreds of KB, so it does not use a bounded scanner.
func eachPiEvent(r io.Reader, fn func(*piEvent)) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var e piEvent
			if json.Unmarshal(line, &e) == nil && e.Type != "" {
				fn(&e)
			}
		}
		if err != nil {
			return
		}
	}
}

// lastReply is the text of the last assistant message in a pi session; a pass whose last assistant message has no text has no reply.
func lastReply(r io.Reader) (string, error) {
	text, seen := "", false
	eachPiEvent(r, func(e *piEvent) {
		if (e.Type == "message" || e.Type == "message_end") && e.Message.Role == "assistant" {
			text, seen = e.text(), true
		}
	})
	if !seen || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("pi gave no reply")
	}
	return text, nil
}

// checkReplyObject rejects absent/null required fields and null array elements.
// A suffix ? marks an optional field; optional does not mean untyped.
func checkReplyObject(raw json.RawMessage, fields map[string]string) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return fmt.Errorf("expected an object")
	}
	for key, kind := range fields {
		name := strings.TrimSuffix(key, "?")
		b, ok := obj[name]
		if !ok && name != key {
			continue
		}
		if !ok || string(b) == "null" {
			return fmt.Errorf("%s must be a %s", name, kind)
		}
		var value any
		switch kind {
		case "string":
			value = new(string)
		case "boolean":
			value = new(bool)
		}
		if err := json.Unmarshal(b, value); err != nil {
			return fmt.Errorf("%s must be a %s", name, kind)
		}
	}
	return nil
}

func (e *groupEntry) UnmarshalJSON(b []byte) error {
	type plain groupEntry
	if err := json.Unmarshal(b, (*plain)(e)); err != nil {
		return err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	for key, fields := range map[string]map[string]string{
		"claims":    {"claim": "string", "evidence": "string"},
		"questions": {"question": "string", "changes_result": "boolean", "package?": "string", "answered_by?": "string"},
		"reuse":     {"builds": "string", "existing": "string", "evidence": "string", "why?": "string"},
	} {
		var entries []json.RawMessage
		if raw, ok := obj[key]; ok {
			if err := json.Unmarshal(raw, &entries); err != nil {
				return err
			}
			for _, entry := range entries {
				if err := checkReplyObject(entry, fields); err != nil {
					return fmt.Errorf("package %s %s: %w", e.ID, key, err)
				}
			}
		}
	}
	var paths []json.RawMessage
	if raw, ok := obj["will_change"]; ok {
		if err := json.Unmarshal(raw, &paths); err != nil {
			return err
		}
		for _, p := range paths {
			var path string
			if string(p) == "null" || json.Unmarshal(p, &path) != nil {
				return fmt.Errorf("will_change entries must be strings")
			}
		}
	}
	return nil
}

// validateEntries checks the entire reply before any of it can establish
// coverage. Non-nil slices distinguish explicit [] from absent/null arrays.
func validateEntries(entries []groupEntry, ids []string) error {
	if entries == nil {
		return fmt.Errorf("packages must be an explicit array")
	}
	expected, seen := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		expected[id] = true
	}
	for _, e := range entries {
		if !expected[e.ID] {
			return fmt.Errorf("unknown package id %q", e.ID)
		}
		if seen[e.ID] {
			return fmt.Errorf("duplicate package id %q", e.ID)
		}
		seen[e.ID] = true
		if e.WillChange == nil || e.Claims == nil || e.Questions == nil {
			return fmt.Errorf("package %s: will_change, claims and questions must be explicit arrays", e.ID)
		}
		if e.Reuse == nil {
			return fmt.Errorf("package %s: reuse must be an explicit array", e.ID)
		}
		for _, q := range e.Questions {
			if q.Package != "" && q.Package != e.ID {
				return fmt.Errorf("package %s: question has unknown package id %q", e.ID, q.Package)
			}
		}
	}
	for _, id := range ids {
		if !seen[id] {
			return fmt.Errorf("no entry for %s", id)
		}
	}
	return nil
}
