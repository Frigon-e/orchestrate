package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Circuit breakers. A task that keeps escalating, respawning or repeating
// itself is not converging, so orch stops automatic recovery and leaves the
// decision to the user. Everything is computed from status.log, so there is
// no counter to drift out of sync.
const (
	maxEscalations = 2 // the third escalate freezes the task
	maxRespawns    = 2 // the third respawn freezes the task
	loopRepeat     = 3 // this many identical reports in a row freeze it
	maxFrozenOpen  = 2 // this many frozen tasks hold all new spawns
)

const unfreezeNote = "unfrozen by the user"

// frozenReason returns why a task is frozen, or "" when it is not. Only
// entries after the last unfreeze count.
func frozenReason(entries []entry) string {
	for i, entrie := range slices.Backward(entries) {
		if entrie.By == "orch" && entrie.Text == unfreezeNote {
			entries = entries[i+1:]
			break
		}
	}
	escalations, respawns, stalls, verifyFails := 0, 0, 0, 0
	for _, e := range entries {
		switch {
		case e.By != "orch" && e.State == "escalate":
			escalations++
		case e.By == "orch" && strings.HasPrefix(e.Text, "closed for respawn"):
			respawns++
		case e.By == "orch" && (strings.HasPrefix(e.Text, "stalled:") || strings.HasPrefix(e.Text, "looping:")):
			stalls++
		case e.By == "orch" && strings.HasPrefix(e.Text, "verify fail"):
			verifyFails++
		}
	}
	// A quick job gets one attempt: a failed verification or a stall ends it,
	// and the orchestrator takes it over instead of sending Flash round again.
	if isQuick(entries) {
		if verifyFails > 0 {
			return "quick job failed verification: it had one attempt. Take it over or write a spec"
		}
		if stalls > 0 {
			return "quick job stalled: it had one attempt. Take it over or write a spec"
		}
	}
	if escalations > maxEscalations {
		return fmt.Sprintf("%d escalations: the task is on the wrong model or the plan is wrong", escalations)
	}
	if respawns > maxRespawns {
		return fmt.Sprintf("respawned %d times: the work is not converging", respawns)
	}
	if stalls > maxStalls {
		return fmt.Sprintf("stalled or looping %d times: it keeps getting stuck without reporting", stalls)
	}
	lead := leadEntries(entries)
	if n := len(lead); n >= loopRepeat {
		last := lead[n-1]
		same := 0
		for i := n - 1; i >= 0 && lead[i].State == last.State && lead[i].Text == last.Text; i-- {
			same++
		}
		if same >= loopRepeat {
			return fmt.Sprintf("the same %s report %d times in a row", last.State, same)
		}
	}
	return ""
}

func frozenTasks(root string) map[string]string {
	out := map[string]string{}
	for _, dir := range allTasks(root) {
		if l, _ := loadLead(dir); l != nil && l.ClosedAt != 0 {
			continue
		}
		if r := frozenReason(readStatus(dir)); r != "" {
			out[filepath.Base(dir)] = r
		}
	}
	return out
}

func holdPath(root string) string { return filepath.Join(root, "hold") }

// spawnBlocked says why no new lead may start, or "". A hold is the safe
// stop: running leads finish, nothing new starts, and the user decides.
func spawnBlocked(root string) string {
	if b, err := os.ReadFile(holdPath(root)); err == nil {
		why := strings.TrimSpace(string(b))
		if why == "" {
			why = "held by the user"
		}
		return "spawning is on hold: " + why + ". Lift it with: orch hold off"
	}
	if f := frozenTasks(root); len(f) >= maxFrozenOpen {
		return fmt.Sprintf("%d tasks are frozen, so no new lead starts until the user decides (orch doctor shows them; orch unfreeze <id> clears one)", len(f))
	}
	return ""
}

func cmdHold(a *app, args []string) error {
	pos, err := parseArgs(a.flags("hold"), args)
	if err != nil {
		return err
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 1 && pos[0] == "off":
		if err := os.Remove(holdPath(s.root)); err != nil && !os.IsNotExist(err) {
			return err
		}
		logEvent(s.root, auditEvent{At: a.now().Unix(), System: "hold", Result: "info", Text: "hold lifted"})
		fmt.Fprintln(a.out, "hold: off")
	case len(pos) >= 1 && pos[0] == "on":
		why := strings.TrimSpace(strings.Join(pos[1:], " "))
		if why == "" {
			return &exitError{code: 2, msg: "usage: orch hold on <reason> | orch hold off"}
		}
		if err := os.MkdirAll(s.root, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(holdPath(s.root), []byte(why+"\n"), 0o644); err != nil {
			return err
		}
		logEvent(s.root, auditEvent{At: a.now().Unix(), System: "hold", Result: "problem", Text: "hold on: " + why})
		fmt.Fprintf(a.out, "hold: on (%s). Running leads continue; new spawns are refused\n", why)
	case len(pos) == 0:
		if b, err := os.ReadFile(holdPath(s.root)); err == nil {
			fmt.Fprintf(a.out, "hold: on (%s)\n", strings.TrimSpace(string(b)))
		} else {
			fmt.Fprintln(a.out, "hold: off")
		}
	default:
		return &exitError{code: 2, msg: "usage: orch hold [on <reason> | off]"}
	}
	return nil
}

func cmdUnfreeze(a *app, args []string) error {
	if err := exactArgs(args, 1, "unfreeze <id>"); err != nil {
		return err
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	dir, err := taskDir(s.root, args[0])
	if err != nil {
		return err
	}
	reason := frozenReason(readStatus(dir))
	if reason == "" {
		fmt.Fprintf(a.out, "%s is not frozen\n", args[0])
		return nil
	}
	if err := a.appendStatus(dir, "note", unfreezeNote, "", "orch"); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "unfroze %s (was: %s). Its counters start again from zero\n", args[0], reason)
	return nil
}

type finding struct{ severity, where, msg, fix string }

// cmdDoctor lists what is inconsistent or stuck, each with the one command
// that fixes it. It changes nothing.
func cmdDoctor(a *app, args []string) error {
	if _, err := parseArgs(a.flags("doctor"), args); err != nil {
		return err
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	me := orchID()
	var fs []finding
	add := func(sev, where, msg, fix string) { fs = append(fs, finding{sev, where, msg, fix}) }
	if homeDir, err := os.UserHomeDir(); err == nil {
		for _, f := range setupFindings(homeDir, s.repo, s.root, exec.LookPath) {
			add(f.severity, f.where, f.msg, f.fix)
		}
	}
	if bin := runnerBinary("claude"); installed("claude") {
		for _, f := range claudeAuthFindings(bin, runClaudeAuth) {
			add(f.severity, f.where, f.msg, f.fix)
		}
	}
	if s.cfg.benchSuiteIgnored {
		add("warn", "config", "repo bench_suite ignored", "use --bench-suite or bench_suite in the user config")
	}

	if items, err := pruneScan(s, staleRule{}); err == nil {
		n := 0
		for _, it := range items {
			if it.keep == "" {
				n++
			}
		}
		if n > 0 {
			add("warn", "worktrees", fmt.Sprintf("%d merged, clean orch worktrees can be removed", n), "run: orch prune")
		}
	}

	leads := myLeads(openLeads(s.root), me)
	if len(leads) > 0 && watcherAlive(s.root, me) == 0 {
		add("error", "watcher", fmt.Sprintf("%d lead(s) in flight and no watcher is running", len(leads)), "start `orch watch` with the Monitor tool")
	}
	agents, herdrErr := agentMap()
	if herdrErr != nil {
		add("warn", "herdr", "cannot list agents: "+herdrErr.Error(), "check that Herdr is running")
	}
	for _, ol := range leads {
		id := filepath.Base(ol.dir)
		fm, _, _ := readTask(filepath.Join(ol.dir, "task.md"))
		if herdrErr == nil {
			if _, ok := agents[ol.lead.PaneID]; !ok {
				add("error", id, "pane "+ol.lead.PaneID+" is gone but the lead is open", "orch close "+id+" --force, then respawn it (work is on its branch)")
			}
		}
		if fm["harness"] != "" && ol.lead.Harness != fm["harness"] {
			add("warn", id, fmt.Sprintf("running on %s but task.md says %s", ol.lead.Harness, fm["harness"]), "orch respawn "+id+" --harness "+fm["harness"]+" --model <m>")
		}
		if ol.lead.Owner != "" && me != "" && ol.lead.Owner != me {
			continue
		}
		if len(openDecisions(readStatus(ol.dir))) > 0 && lastState(leadEntries(readStatus(ol.dir))) == "done" {
			add("warn", id, "reported done with a decision still open", "orch status, then resolve it with orch send --resolve <key>")
		}
		if fm["kind"] == "ship" && lastState(leadEntries(readStatus(ol.dir))) == "done" {
			if out, err := git(worktreePath(ol.dir, id), "status", "--porcelain"); err == nil && out != "" {
				add("warn", id, "done, but its worktree has uncommitted changes", "orch send "+id+" \"commit or discard your changes\"")
			}
		}
	}
	for id, why := range frozenTasks(s.root) {
		add("error", id, "frozen: "+why, "decide with the user, then orch unfreeze "+id)
	}
	if why := spawnBlocked(s.root); why != "" {
		add("warn", "spawn", why, "orch hold off, or clear the frozen tasks")
	}
	limited := loadHealth(a.now())
	for h, e := range limited {
		n := 0
		for _, ol := range leads {
			if laneWithin(lane(ol.lead.Harness, ol.lead.Model), h) {
				n++
			}
		}
		if n > 0 {
			add("error", h, fmt.Sprintf("%s, and %d lead(s) still run on it", e, n), "orch respawn <id> --harness <other> --model <m> for each, or wait")
		}
	}
	defer func() {
		fmt.Fprintln(a.out, retrievalReadiness(newCodeEnv(s.repo, codeOpts{Cfg: s.cfg})))
		printRoleReadiness(a.out, s)
		fmt.Fprintln(a.out, benchDoctorLine())
	}()
	if len(fs) == 0 {
		fmt.Fprintln(a.out, "doctor: ok")
		return nil
	}
	errs := 0
	fmt.Fprintf(a.out, "doctor: %d finding(s)\nfindings[%d]{severity,where,message,fix}:\n", len(fs), len(fs))
	for _, f := range fs {
		if f.severity == "error" {
			errs++
		}
		fmt.Fprintf(a.out, "  %s,%s,%s,%s\n", f.severity, agentScalar(f.where), agentScalar(f.msg), agentScalar(f.fix))
	}
	if errs > 0 {
		return &exitError{code: 1, msg: ""}
	}
	return nil
}
