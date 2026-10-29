package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The wake use judges watcher alerts (stalled, looping, quiet) in shadow: it
// records what the decider thinks and what really happened, and changes
// nothing. See docs/specs/decision-model.md#wake-shadow.

const (
	wakeUse = "wake"
	// wakeScreenLines and wakeStatusLines are how much of the lead goes to the decider.
	wakeScreenLines = 40
	wakeStatusLines = 5
)

// wakeDeadline is how long one decider call may take; the watcher never waits
// for it except to flush records on exit.
var wakeDeadline = 10 * time.Second

// wakeClock is the time the outcome resolver measures windows against.
var wakeClock = time.Now

// wakeActionPrefixes start the status.log notes that `orch` writes when it
// acts on a lead. Watcher alerts ("stalled:", "looping:") are not actions.
var wakeActionPrefixes = []string{
	"closed by orchestrator", "closed for respawn", "respawned", unfreezeNote,
	"sent message", "compacted", "focused",
}

var wakeInflight sync.WaitGroup

// waitWake waits for in-flight wake calls, at most one deadline, so their
// records are written before the watcher exits.
func waitWake() {
	done := make(chan struct{})
	go func() { wakeInflight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wakeDeadline + time.Second):
	}
}

// wakeSink starts shadow decisions for one watcher.
type wakeSink struct {
	s         site
	flushFile string // last-flush-<watch-name>: when the watcher last printed to the orchestrator
}

func newWakeSink(s site, stateFile string) *wakeSink {
	name := strings.TrimSuffix(filepath.Base(stateFile), ".json")
	return &wakeSink{s: s, flushFile: filepath.Join(s.root, "last-flush-"+name)}
}

// noteFlush records that the watcher printed to the orchestrator just now.
func (w *wakeSink) noteFlush(now time.Time) {
	if w == nil {
		return
	}
	_ = writeAtomic(w.flushFile, []byte(strconv.FormatInt(now.Unix(), 10)+"\n"))
}

// idleSeconds is the time since the last flush, or nil before any flush.
func (w *wakeSink) idleSeconds(now time.Time) any {
	b, err := os.ReadFile(w.flushFile)
	if err != nil {
		return nil
	}
	at, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return nil
	}
	return max(0, now.Unix()-at)
}

func wakeQuestions(window int) map[string]any {
	return map[string]any{
		"recovers": map[string]any{"type": "noul",
			"instructions": fmt.Sprintf("Will the lead make progress within %d minutes without help?", window),
			"criteria":     map[string]any{"true": "it is working or waiting on something that will finish", "false": "it is stuck, looping or waiting for input"}},
		"needs_orchestrator": map[string]any{"type": "noul",
			"instructions": "Does this need the orchestrator to act?",
			"criteria":     map[string]any{"true": "the lead cannot go on without a message, restart or decision", "false": "the orchestrator can leave it alone"}},
	}
}

// alert starts a shadow decision for one alert and returns at once. fp is the
// worktree fingerprint at the alert; mins is the minutes since progress.
func (w *wakeSink) alert(a *app, ol openLead, ag herdrAgent, kind string, mins int64, window int, fp string) {
	if w == nil || !roleEnabled(w.s.role("wake")) {
		return
	}
	now := a.now()
	meta := map[string]any{
		"kind": kind, "alert_at": now.Unix(), "window_minutes": window,
		"idle_seconds": w.idleSeconds(now), "fingerprint": fp,
	}
	wakeInflight.Add(1)
	go func() {
		defer wakeInflight.Done()
		state := map[string]any{
			"alert":                  kind,
			"minutes_since_progress": mins,
			"status_lines":           wakeStatusTail(ol.dir),
			"tool_in_flight":         toolInFlight(ol, ag, now.Unix(), int64(w.s.cfg.StallMinutes)),
		}
		if lines, err := readScreen(ol.lead.PaneID, wakeScreenLines); err == nil {
			state["screen"] = strings.Join(lines, "\n")
		}
		ctx, cancel := context.WithTimeout(context.Background(), wakeDeadline)
		defer cancel()
		_, _ = askDecision(ctx, w.s, decisionRequest{
			Use: wakeUse, Role: "wake", Subject: ol.lead.ID,
			State: state, Questions: wakeQuestions(window),
			Keys:   []string{"recovers", "needs_orchestrator"},
			Fields: map[string][]string{"recovers": {"noul"}, "needs_orchestrator": {"noul"}},
			Meta:   meta,
		})
	}()
}

// wakeStatusTail is the lead's last few status lines, one string each.
func wakeStatusTail(dir string) string {
	es := leadEntries(readStatus(dir))
	es = es[max(0, len(es)-wakeStatusLines):]
	lines := make([]string, len(es))
	for i, e := range es {
		lines[i] = e.State + ": " + e.Text
	}
	return strings.Join(lines, "\n")
}

func isActionNote(e entry) bool {
	if e.By != "orch" {
		return false
	}
	for _, p := range wakeActionPrefixes {
		if strings.HasPrefix(e.Text, p) {
			return true
		}
	}
	return false
}

func metaInt(m map[string]any, k string) int64 {
	f, _ := m[k].(float64)
	return int64(f)
}

// resolveWakeOutcomes writes the outcome of every alert whose window has
// ended and has none yet. It runs from the watcher and from
// `orch decisions report`, and is safe to repeat.
func resolveWakeOutcomes(s site) error {
	recs, err := readJSONLines[decisionRecord](decisionFile(s.root, wakeUse))
	if err != nil || len(recs) == 0 {
		return err
	}
	outs, err := readJSONLines[decisionOutcome](decisionOutcomeFile(s.root, wakeUse))
	if err != nil {
		return err
	}
	done := map[string]bool{}
	for _, o := range outs {
		done[o.ID] = true
	}
	now := wakeClock().Unix()
	var firstErr error
	for _, r := range recs {
		if done[r.ID] {
			continue
		}
		alertAt, window := metaInt(r.Meta, "alert_at"), metaInt(r.Meta, "window_minutes")
		end := alertAt + window*60
		if alertAt == 0 || window == 0 || now < end {
			continue
		}
		dir, err := taskDir(s.root, r.Subject)
		if err != nil {
			continue
		}
		var acted []string
		wrote := false
		entries := readStatus(dir)
		for _, e := range entries {
			if e.At < alertAt || e.At > end {
				continue
			}
			if isActionNote(e) {
				acted = append(acted, e.Text)
			} else if e.By != "orch" {
				wrote = true
			}
		}
		fp, _ := r.Meta["fingerprint"].(string)
		changed := false
		if cur := worktreeFingerprint(worktreePath(dir, r.Subject)); cur != "" && cur != fp {
			changed = true
		}
		out := map[string]any{
			"orchestrator_acted": len(acted) > 0,
			"progressed_alone":   len(acted) == 0 && (wrote || changed),
			"actions":            append([]string{}, acted...),
		}
		if err := recordDecisionOutcome(s.root, wakeUse, r.ID, out); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func init() {
	registerOutcomeResolver(wakeUse, resolveWakeOutcomes)
	registerDecisionMetric(wakeUse, decisionMetric{Name: "recovers", Key: "recovers", Field: "noul", Outcome: "progressed_alone"})
	registerDecisionMetric(wakeUse, decisionMetric{Name: "needs_orchestrator", Key: "needs_orchestrator", Field: "noul", Outcome: "orchestrator_acted"})
}
