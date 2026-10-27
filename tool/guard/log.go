package guard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// LookupLine is one row of lookups.jsonl: a lookup a harness made. The Claude
// coach and the pi extension write it, and `orch lookups report` reads it.
type LookupLine struct {
	TS         string   `json:"ts"`
	Harness    string   `json:"harness"`
	Session    string   `json:"session"`
	Repo       string   `json:"repo"`
	Turn       any      `json:"turn"`
	Tool       string   `json:"tool"`
	Query      string   `json:"query"`
	OK         bool     `json:"ok"`
	Empty      bool     `json:"empty"`
	Bytes      int      `json:"bytes"`
	DurationMS *float64 `json:"duration_ms"`
	Reminder   *string  `json:"reminder"`
	Followed   *bool    `json:"followed"`
}

// StateDir holds the coach state, the lookup log and the guard rebuild log.
func StateDir() string {
	if dir := os.Getenv("ORCH_TOOL_GUARD_STATE_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "orchestrate")
}

// LogPath is the lookup log.
func LogPath() string { return filepath.Join(StateDir(), "lookups.jsonl") }

// appendLine writes one log row with a single O_APPEND write, so parallel hooks never interleave.
func appendLine(path string, line LookupLine) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(line); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(buf.Bytes())
	return err
}
