package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// `orch audit` answers "when did each system go right or wrong" from records
// orch already keeps: every task's status log, the triage and gate records,
// and a small event log for what has no other record (health limits, holds).
// It only reads. Nothing here changes a record or a threshold.
type auditEvent struct {
	At     int64  `json:"at"`
	System string `json:"system"`
	Task   string `json:"task,omitempty"`
	Result string `json:"result"` // ok, problem or info
	Text   string `json:"text"`
}

func eventLogPath(dir string) string { return filepath.Join(dir, "events.jsonl") }

// logEvent appends one event to the event log in dir. A failure to write is
// never worth failing the command that caused it.
func logEvent(dir string, e auditEvent) {
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	f, err := os.OpenFile(eventLogPath(dir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	b, _ := json.Marshal(e)
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

func readEvents(dir string) []auditEvent {
	b, err := os.ReadFile(eventLogPath(dir))
	if err != nil {
		return nil
	}
	var out []auditEvent
	for _, l := range strings.Split(string(b), "\n") {
		var e auditEvent
		if l != "" && json.Unmarshal([]byte(l), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// statusEvents turns one task's status log into events. Orchestrator notes
// carry the verify, stall, respawn and quick results; lead reports carry the
// outcome. A freeze is not written anywhere, so it is found by replaying the
// log and noting the entry after which the breakers first trip.
func statusEvents(id string, entries []entry) []auditEvent {
	var out []auditEvent
	add := func(e entry, system, result, text string) {
		out = append(out, auditEvent{At: e.At, System: system, Task: id, Result: result, Text: text})
	}
	frozen := ""
	for i, e := range entries {
		switch {
		// Classify preliminary passes too: their suffix is "(not integrated)".
		case e.By == "orch" && strings.HasPrefix(e.Text, "verify pass"):
			add(e, "verify", "ok", e.Text)
		case e.By == "orch" && strings.HasPrefix(e.Text, "verify fail"):
			add(e, "verify", "problem", e.Text)
		case e.By == "orch" && (strings.HasPrefix(e.Text, "stalled:") || strings.HasPrefix(e.Text, "looping:")):
			add(e, "stall", "problem", e.Text)
		case e.By == "orch" && strings.HasPrefix(e.Text, "closed for respawn"):
			add(e, "respawn", "problem", e.Text)
		case e.By == "orch" && strings.HasPrefix(e.Text, "quick:"):
			add(e, "quick", "info", e.Text)
		case e.By == "orch" && strings.HasPrefix(e.Text, "auto:"):
			add(e, "triage", "info", e.Text)
		case e.By == "orch" && e.Text == unfreezeNote:
			add(e, "freeze", "info", e.Text)
		case e.By != "orch" && e.State == "escalate":
			add(e, "escalate", "info", e.Text)
		case e.By != "orch" && e.State == "done":
			add(e, "task", "ok", e.Text)
		case e.By != "orch" && (e.State == "failed" || e.State == "blocked"):
			add(e, "task", "problem", e.State+": "+e.Text)
		}
		if now := frozenReason(entries[:i+1]); now != "" && frozen == "" {
			add(e, "freeze", "problem", now)
			frozen = now
		} else if now == "" {
			frozen = ""
		}
	}
	return out
}

func gateEvents(root string) []auditEvent {
	var out []auditEvent
	ents, _ := os.ReadDir(filepath.Join(root, "gate"))
	for _, d := range ents {
		b, err := os.ReadFile(filepath.Join(root, "gate", d.Name(), "gate.json"))
		if err != nil {
			continue
		}
		var r gateRecord
		if json.Unmarshal(b, &r) != nil {
			continue
		}
		label := storedGateResultLabel(&r)
		res := "problem"
		if label == gateStatePass {
			res = "ok"
		}
		blockers := 0
		for _, f := range r.Findings {
			if f.Severity == "blocker" {
				blockers++
			}
		}
		out = append(out, auditEvent{At: r.At, System: "gate", Result: res,
			Text: fmt.Sprintf("%s %s (spec version %s): %d blocker(s), %d deterministic issue(s)", r.Spec, label, r.Hash, blockers, len(r.Deterministic))})
	}
	return out
}

func triageEvents(root string) []auditEvent {
	var out []auditEvent
	for _, r := range readTriage(root) {
		res := "info"
		if r.Tier == "not-quick" {
			res = "problem"
		}
		out = append(out, auditEvent{At: r.At, System: "triage", Task: r.ID, Result: res,
			Text: fmt.Sprintf("rated %s (%s); runs on %s", r.Tier, r.Reason, strings.TrimSpace(r.Actual))})
	}
	return out
}

func cmdAudit(a *app, args []string) error {
	fs := a.flags("audit")
	system := fs.String("system", "", "only this system: gate, triage, quick, verify, stall, escalate, respawn, freeze, task, health, hold")
	since := fs.String("since", "", "only events this recent, such as 6h or 72h")
	problems := fs.Bool("problems", false, "only events that went wrong")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return &exitError{code: 2, msg: "usage: orch audit [<id>] [--system S] [--since DURATION] [--problems]"}
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	var cutoff int64
	if *since != "" {
		d, err := time.ParseDuration(*since)
		if err != nil || d <= 0 {
			return fail("--since %q is not a duration such as 6h", *since)
		}
		cutoff = a.now().Add(-d).Unix()
	}
	var ev []auditEvent
	for _, dir := range allTasks(s.root) {
		ev = append(ev, statusEvents(filepath.Base(dir), readStatus(dir))...)
	}
	ev = append(ev, gateEvents(s.root)...)
	ev = append(ev, triageEvents(s.root)...)
	ev = append(ev, readEvents(s.root)...)
	ev = append(ev, readEvents(filepath.Dir(healthPath()))...)

	var shown []auditEvent
	for _, e := range ev {
		switch {
		case len(pos) == 1 && e.Task != pos[0]:
		case *system != "" && e.System != *system:
		case *problems && e.Result != "problem":
		case e.At < cutoff:
		default:
			shown = append(shown, e)
		}
	}
	sort.SliceStable(shown, func(i, j int) bool { return shown[i].At < shown[j].At })

	counts := map[string]map[string]int{}
	for _, e := range shown {
		if counts[e.System] == nil {
			counts[e.System] = map[string]int{}
		}
		counts[e.System][e.Result]++
		task := e.Task
		if task == "" {
			task = "-"
		}
		fmt.Fprintf(a.out, "%s  %-8s %-9s %-7s %s\n", time.Unix(e.At, 0).Format("01-02 15:04"), e.System, short(task, 20), e.Result, short(e.Text, 150))
	}
	if len(shown) == 0 {
		fmt.Fprintln(a.out, "no events")
		return nil
	}
	systems := make([]string, 0, len(counts))
	for k := range counts {
		systems = append(systems, k)
	}
	sort.Strings(systems)
	fmt.Fprintf(a.out, "\nsummary[%d]{system,ok,problem,info}:\n", len(systems))
	for _, k := range systems {
		fmt.Fprintf(a.out, "  %s,%d,%d,%d\n", k, counts[k]["ok"], counts[k]["problem"], counts[k]["info"])
	}
	return nil
}
