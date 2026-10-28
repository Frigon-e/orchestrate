package main

import (
	"bytes"
	"encoding/json"
	"time"
)

// claudeLimit is a subscription rejection found in a Claude transcript.
type claudeLimit struct {
	ResetsAt    int64
	Type, Stamp string
}

// claudeLimitSince returns the newest quota rejection in a Claude session
// transcript stamped strictly after `after` (RFC3339; empty means no bound).
// Other API errors (overload, auth) carry no quotaLimits and are ignored.
func claudeLimitSince(session, after string) (claudeLimit, bool) {
	var lower time.Time
	if after != "" {
		lower, _ = time.Parse(time.RFC3339, after)
	}
	var best claudeLimit
	var bestAt time.Time
	for _, line := range bytes.Split(readTail(sessionPath("claude", session), 1<<20), []byte("\n")) {
		if !bytes.Contains(line, []byte(`"isApiErrorMessage"`)) || !bytes.Contains(line, []byte(`"quotaLimits"`)) {
			continue
		}
		var e struct {
			Type      string `json:"type"`
			IsAPIErr  bool   `json:"isApiErrorMessage"`
			Error     string `json:"error"`
			Timestamp string `json:"timestamp"`
			Quota     struct {
				Status   string `json:"status"`
				ResetsAt int64  `json:"resetsAt"`
				Type     string `json:"rateLimitType"`
			} `json:"quotaLimits"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || !e.IsAPIErr || e.Error != "rate_limit" ||
			e.Quota.Status != "rejected" || e.Quota.ResetsAt <= 0 {
			continue
		}
		at, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil || !at.After(lower) || (!bestAt.IsZero() && !at.After(bestAt)) {
			continue
		}
		best, bestAt = claudeLimit{ResetsAt: e.Quota.ResetsAt, Type: e.Quota.Type, Stamp: e.Timestamp}, at
	}
	return best, !bestAt.IsZero()
}
