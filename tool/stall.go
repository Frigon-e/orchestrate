package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// A stalled lead is busy but not getting anywhere. `quiet` catches an idle
// lead and the circuit breakers count what a lead reports, so a lead that
// retries one failing command inside a single turn trips neither. These
// signals need no model: the worktree has not changed, or the transcript
// shows the same tool call again and again.
const (
	stallSampleSecs = 60  // fingerprint a worktree at most this often
	loopStillSecs   = 120 // a repeating call counts only if the worktree was still this long
	loopWindow      = 12  // look at this many latest tool calls
	loopRepeats     = 8   // the same call this many times in the window
	loopRun         = 5   // or this many times in a row
	maxStalls       = 2   // the third stalled or looping alert freezes the task
)

// worktreeFingerprint hashes everything about a worktree that work can
// change: HEAD, tracked edits and the names and sizes of untracked files.
// It is "" when the worktree does not exist.
func worktreeFingerprint(wt string) string {
	if st, err := os.Stat(wt); err != nil || !st.IsDir() {
		return ""
	}
	h := sha256.New()
	head, _ := git(wt, "rev-parse", "HEAD")
	diff, _ := git(wt, "diff", "HEAD")
	untracked, _ := git(wt, "ls-files", "--others", "--exclude-standard")
	fmt.Fprintf(h, "%s\x00%s\x00", head, diff)
	for _, f := range strings.Split(untracked, "\n") {
		if f == "" {
			continue
		}
		if st, err := os.Stat(wt + "/" + f); err == nil {
			fmt.Fprintf(h, "%s:%d:%d\x00", f, st.Size(), st.ModTime().UnixNano())
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:8])
}

// callSignatures returns the tool calls in the tail of a session transcript,
// oldest first, each as "name\x00canonical-arguments". Claude writes
// tool_use blocks and pi writes toolCall blocks; both sit in message.content.
func callSignatures(harness, session string) []string {
	path := sessionPath(harness, session)
	if path == "" {
		return nil
	}
	var out []string
	for _, line := range bytes.Split(readTail(path, 1<<20), []byte("\n")) {
		if !bytes.Contains(line, []byte(`"tool_use"`)) && !bytes.Contains(line, []byte(`"toolCall"`)) {
			continue
		}
		var e struct {
			IsSidechain bool `json:"isSidechain"`
			Message     struct {
				Content []struct {
					Type      string          `json:"type"`
					Name      string          `json:"name"`
					Input     json.RawMessage `json:"input"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.IsSidechain {
			continue
		}
		for _, b := range e.Message.Content {
			if b.Type != "tool_use" && b.Type != "toolCall" {
				continue
			}
			raw := b.Input
			if len(raw) == 0 {
				raw = b.Arguments
			}
			var v any
			if json.Unmarshal(raw, &v) == nil {
				raw, _ = json.Marshal(v) // sorted keys: equal calls compare equal
			}
			out = append(out, b.Name+"\x00"+string(raw))
		}
	}
	return out
}

// loopingCall reports a call that repeats in the latest tool calls: the same
// one loopRepeats times in the window, or loopRun times in a row at the end.
func loopingCall(sigs []string) (name string, n int, ok bool) {
	if len(sigs) == 0 {
		return "", 0, false
	}
	if len(sigs) > loopWindow {
		sigs = sigs[len(sigs)-loopWindow:]
	}
	counts := map[string]int{}
	for _, s := range sigs {
		counts[s]++
	}
	for s, c := range counts {
		if c >= loopRepeats {
			return strings.SplitN(s, "\x00", 2)[0], c, true
		}
	}
	run := 0
	for i := len(sigs) - 1; i >= 0 && sigs[i] == sigs[len(sigs)-1]; i-- {
		run++
	}
	if run >= loopRun {
		return strings.SplitN(sigs[len(sigs)-1], "\x00", 2)[0], run, true
	}
	return "", 0, false
}

// inFlightSince returns the start time (unix seconds) of the newest tool call
// in the transcript tail that has no result yet. Claude pairs tool_use blocks
// in assistant lines with tool_result blocks in user lines; pi pairs toolCall
// blocks in assistant messages with toolResult messages.
func inFlightSince(harness, session string) (start int64, ok bool) {
	if harness != "claude" && harness != "pi" {
		return 0, false
	}
	path := sessionPath(harness, session)
	if path == "" {
		return 0, false
	}
	open := map[string]int64{}
	for _, line := range bytes.Split(readTail(path, 1<<20), []byte("\n")) {
		if !bytes.Contains(line, []byte(`"tool_use"`)) && !bytes.Contains(line, []byte(`"toolCall"`)) &&
			!bytes.Contains(line, []byte(`"tool_result"`)) && !bytes.Contains(line, []byte(`"toolResult"`)) {
			continue
		}
		var e struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Timestamp   string `json:"timestamp"`
			Message     struct {
				Role       string          `json:"role"`
				ToolCallID string          `json:"toolCallId"`
				Content    json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.IsSidechain {
			continue
		}
		var blocks []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ToolUseID string `json:"tool_use_id"`
		}
		_ = json.Unmarshal(e.Message.Content, &blocks) // content may be a plain string
		switch {
		case harness == "claude" && e.Type == "assistant":
			ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
			if err != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type == "tool_use" {
					open[b.ID] = ts.Unix()
				}
			}
		case harness == "claude" && e.Type == "user":
			for _, b := range blocks {
				if b.Type == "tool_result" {
					delete(open, b.ToolUseID)
				}
			}
		case harness == "pi" && e.Type == "message" && e.Message.Role == "assistant":
			ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
			if err != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type == "toolCall" {
					open[b.ID] = ts.Unix()
				}
			}
		case harness == "pi" && e.Type == "message" && e.Message.Role == "toolResult":
			delete(open, e.Message.ToolCallID)
		}
	}
	for _, s := range open {
		if s > start {
			start = s
		}
		ok = true
	}
	return start, ok
}

// toolInFlight reports a tool call that started less than 3x stall_minutes
// ago and has no result yet: the lead is busy, not stalled.
func toolInFlight(ol openLead, ag herdrAgent, now, mins int64) bool {
	start, ok := inFlightSince(ol.lead.Harness, sessionOf(ag, ol.lead))
	return ok && now-start < 3*mins*60
}

// stallCheck runs for a lead whose agent is working. It emits `stalled` when
// nothing has changed or been reported for stall_minutes, and `looping` when
// the transcript repeats one tool call while the worktree stands still. Each
// alert is also written to the status log so the circuit breakers count it.
func (a *app) stallCheck(ol openLead, st *watchSeen, ag herdrAgent, cfg config, now int64, emit func(string)) {
	id := ol.lead.ID
	if now-st.LastSample >= stallSampleSecs || st.FPSince == 0 {
		st.LastSample = now
		if fp := worktreeFingerprint(worktreePath(ol.dir, id)); fp != st.FP || st.FPSince == 0 {
			st.FP, st.FPSince = fp, now
			st.StallAlerted, st.LoopAlerted = false, ""
		}
	}
	progressAt := max(st.FPSince, st.LastReport, ol.lead.SpawnedAt)
	mins := int64(cfg.StallMinutes)
	if !st.StallAlerted && now-progressAt >= mins*60 && !toolInFlight(ol, ag, now, mins) {
		st.StallAlerted = true
		msg := fmt.Sprintf("no report and no change to the worktree for %dm", (now-progressAt)/60)
		_ = a.appendStatus(ol.dir, "note", "stalled: "+msg, "", "orch")
		emit("stalled: " + msg + "; run orch peek " + id)
	}
	if now-st.FPSince >= loopStillSecs {
		if name, n, ok := loopingCall(callSignatures(ol.lead.Harness, sessionOf(ag, ol.lead))); ok && st.LoopAlerted != name {
			st.LoopAlerted = name
			msg := fmt.Sprintf("it called %s with the same arguments %d times in its last %d tool calls while the worktree did not change", name, n, loopWindow)
			_ = a.appendStatus(ol.dir, "note", "looping: "+msg, "", "orch")
			emit("looping: " + msg + "; run orch peek " + id)
		}
	}
}
