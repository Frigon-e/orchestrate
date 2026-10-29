package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Triage asks a decision model (Perplexity Decider on OpenRouter) typed
// questions about a task and turns the probabilities into a tier with
// thresholds in code:
//
//	settled   exact instructions, easy, a check that can fail: a cheap model may do it
//	standard  a mid-tier model
//	planning  open-ended, hard or unknown cause: the strongest model plans it
//
// In shadow mode (the default) it only records its answer. `orch triage
// report` joins those records with what actually happened to each task, so
// thresholds are tuned on our own outcomes before they route anything.
const (
	triageURLDefault   = "https://openrouter.ai/api/alpha/decisions"
	triageModelDefault = "perplexity/pplx-decider-v1.2-20261016"
	triageDeadline     = 90 * time.Second // per call, passed to askDecision as a context
	triageMinOutcomes  = 20               // outcomes needed before the old report trusts a rate

	// The tier follows the raw answers: no confidence bump, no open-ended rule.
	hardPlanning      = 0.3 // P(difficulty=3) at or above this means planning
	causeUnknown      = 0.2 // P(cause known) below this means planning
	settledDifficulty = 1.5 // expected difficulty below this for settled
	settledVerifiable = 0.8 // P(verifiable) at or above this for settled
)

type triageAnswers struct {
	Clarity      string             `json:"clarity"`
	ClarityP     map[string]float64 `json:"clarity_p"`
	ClarityConf  float64            `json:"clarity_confidence"`
	Difficulty   float64            `json:"difficulty"`
	DifficultyP  map[string]float64 `json:"difficulty_p"`
	DiffConf     float64            `json:"difficulty_confidence"`
	Verifiable   float64            `json:"verifiable"`
	CrossCutting float64            `json:"cross_cutting"`
	CauseKnown   float64            `json:"cause_known"`
}

type triageRecord struct {
	ID       string        `json:"id"`
	At       int64         `json:"at"`
	Mode     string        `json:"mode"`
	Model    string        `json:"model"`
	Actual   string        `json:"actual"` // harness and model the task was set to run on
	TaskHash string        `json:"task_hash"`
	Answers  triageAnswers `json:"answers"`
	Tier     string        `json:"tier"`
	Decision string        `json:"decision_id,omitempty"` // the decisions/triage.jsonl record this tier came from
	Reason   string        `json:"reason"`
	Skipped  string        `json:"skipped,omitempty"`
}

type triageOff struct{ reason, id string }

func (e *triageOff) Error() string {
	return fmt.Sprintf("triage is off (%s): choose a profile by hand (orch spawn %s --harness H --model M) or pass --tier settled|standard|planning to skip triage", e.reason, e.id)
}

func (s site) deciderOff(now time.Time) string {
	if s.cfg.TriageMode == "off" {
		return "triage_mode off, pi:openrouter"
	}
	r, _ := s.newRoleRun("decider", modelOptions{})
	r.now = func() time.Time { return now }
	if choices, _ := r.available(); len(choices) > 0 {
		return ""
	}
	reason := r.unavailable().Error()
	if !strings.Contains(reason, "pi:") {
		connector := "pi:openrouter"
		if len(r.choices) > 0 {
			connector = r.choices[0].lane()
		}
		reason += ", " + connector
	}
	return reason
}

func (a *app) recordTriageSkip(s site, id, reason string) {
	dir, err := taskDir(s.root, id)
	if err != nil {
		return
	}
	_, hash, _ := triageState(dir, s.cfg)
	fm, _, _ := readTask(filepath.Join(dir, "task.md"))
	_ = appendTriage(s.root, triageRecord{ID: id, At: a.now().Unix(), Mode: s.cfg.TriageMode, TaskHash: hash, Actual: fm["harness"] + " " + fm["model"], Reason: reason, Skipped: "decider off"})
}

func triageQuestions() map[string]any {
	return map[string]any{
		"clarity": map[string]any{"type": "choice", "instructions": "How well specified is this task?",
			"criteria": map[string]any{
				"clear":      "it names exactly what to change and a check that can fail",
				"guided":     "the goal and constraints are clear but the approach is open",
				"open_ended": "the goal itself needs discovery or judgment",
			}},
		"difficulty": map[string]any{"type": "score", "instructions": "How hard is this task for a coding agent?",
			"criteria": []string{
				"trivial: one obvious edit",
				"easy: a few files, clear steps",
				"medium: needs investigation or several coordinated changes",
				"hard: unclear cause, cross-cutting, or risky",
			}},
		"verifiable": map[string]any{"type": "noul", "instructions": "Is there a command or test named in the task that fails if the work is wrong?",
			"criteria": map[string]any{"true": "a named command or test would fail on wrong work", "false": "no check, or one that cannot fail"}},
		"cross_cutting": map[string]any{"type": "noul", "instructions": "Does the work touch more than one subsystem, or change an interface other code depends on?",
			"criteria": map[string]any{"true": "several subsystems or a shared interface", "false": "one contained area"}},
		"cause_known": map[string]any{"type": "noul", "instructions": "If the task is a bug, is its cause already known? Answer true if the task is not a bug.",
			"criteria": map[string]any{"true": "not a bug, or the cause is stated", "false": "a bug whose cause is unknown and must be found"}},
	}
}

// tierFor turns answers into a tier and the reason, using fixed thresholds.
func tierFor(a triageAnswers) (tier, reason string) {
	switch {
	case a.DifficultyP["3"] >= hardPlanning:
		return "planning", fmt.Sprintf("hard (P=%.2f)", a.DifficultyP["3"])
	case a.CauseKnown < causeUnknown:
		return "planning", fmt.Sprintf("unknown cause (P(known)=%.2f)", a.CauseKnown)
	case a.Difficulty < settledDifficulty && a.Verifiable >= settledVerifiable:
		return "settled", fmt.Sprintf("difficulty %.2f, verifiable %.2f", a.Difficulty, a.Verifiable)
	}
	return "standard", "not easy and verifiable enough for settled"
}

type decAnswer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// triageKey finds an OpenRouter credential: an explicit variable, then the
// token pi already holds for the provider.
func triageKey() (string, error) { return providerKey("openrouter") }

// triageAnswersOf reads the typed answers the helper has already validated.
func triageAnswersOf(r map[string]decAnswer) triageAnswers {
	c, d := r["clarity"], r["difficulty"]
	return triageAnswers{Clarity: c.Choice, ClarityP: c.Probabilities, ClarityConf: c.Confidence,
		Difficulty: d.Score, DifficultyP: d.Probabilities, DiffConf: d.Confidence,
		Verifiable: r["verifiable"].Noul, CrossCutting: r["cross_cutting"].Noul, CauseKnown: r["cause_known"].Noul}
}

// triageFields are the JSON fields each answer must carry; a missing one is an
// invalid answer, never a zero.
var triageFields = map[string][]string{
	"clarity":       {"choice", "probabilities", "confidence"},
	"difficulty":    {"score", "probabilities", "confidence"},
	"verifiable":    {"noul"},
	"cross_cutting": {"noul"},
	"cause_known":   {"noul"},
}

// triageState is what the model reads: the task's own words and nothing from
// the code. Each part is capped so a long brief stays cheap.
func triageState(dir string, cfg config) (map[string]any, string, error) {
	fm, body, err := readTask(filepath.Join(dir, "task.md"))
	if err != nil {
		return nil, "", err
	}
	secs := sections(body)
	limit := func(s string) string { return short(strings.TrimSpace(s), 6000) }
	state := map[string]any{
		"kind": fm["kind"], "owns": fm["owns"],
		"spec": limit(secs["Spec"]), "scope": limit(secs["Scope"]), "acceptance": limit(secs["Acceptance"]), "authority": limit(secs["Authority"]),
		"repo_check_command": cfg.Check,
	}
	b, _ := json.Marshal(state)
	sum := sha256.Sum256(b)
	return state, fmt.Sprintf("%x", sum[:6]), nil
}

func triageLogPath(root string) string { return filepath.Join(root, "triage", "triage.jsonl") }

func readTriage(root string) []triageRecord {
	b, err := os.ReadFile(triageLogPath(root))
	if err != nil {
		return nil
	}
	var out []triageRecord
	for _, l := range strings.Split(string(b), "\n") {
		var r triageRecord
		if l != "" && json.Unmarshal([]byte(l), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func appendTriage(root string, r triageRecord) error {
	path := triageLogPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// triageTask scores one task, records the answer and sets `tier:` in its
// front matter. A task whose text has not changed since its last record is
// not asked again.
func (a *app) triageTask(s site, id string, force bool) (*triageRecord, error) {
	rec, cached, err := a.triageScore(s, id, force)
	if err != nil {
		return nil, err
	}
	if !cached {
		if err := a.persistTriage(s, id, rec); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

// persistTriage writes a fresh record and the task's `tier:`.
func (a *app) persistTriage(s site, id string, rec *triageRecord) error {
	dir, err := taskDir(s.root, id)
	if err != nil {
		return err
	}
	if err := appendTriage(s.root, *rec); err != nil {
		return err
	}
	_ = setFrontMatter(filepath.Join(dir, "task.md"), "tier", rec.Tier)
	return nil
}

// triageScore rates one task without writing anything. cached reports that
// the record is an earlier one for the same text, which needs no new write.
func (a *app) triageScore(s site, id string, force bool) (rec *triageRecord, cached bool, err error) {
	dir, err := taskDir(s.root, id)
	if err != nil {
		return nil, false, err
	}
	// Triage sends the task's own words to a third-party model. A private
	// task is kept on Claude so that its text stays off other providers.
	if fm, _, err := readTask(filepath.Join(dir, "task.md")); err == nil && fm["private"] == "true" {
		return nil, false, fail("%s is private: its text is not sent to the triage model. Choose its profile by hand", id)
	}
	state, hash, err := triageState(dir, s.cfg)
	if err != nil {
		return nil, false, fail("no task %s", id)
	}
	if !force {
		recs := readTriage(s.root)
		for i := len(recs) - 1; i >= 0; i-- {
			if recs[i].ID == id && recs[i].TaskHash == hash && recs[i].Skipped == "" {
				return &recs[i], true, nil
			}
		}
	}
	fm, _, _ := readTask(filepath.Join(dir, "task.md"))
	actual := fm["harness"] + " " + fm["model"]
	var ans triageAnswers
	var tier, reason string
	ctx, cancel := context.WithTimeout(context.Background(), triageDeadline)
	defer cancel()
	res, runErr := askDecision(ctx, s, decisionRequest{
		Use: "triage", Role: "decider", Subject: id, State: state, Questions: triageQuestions(),
		Keys: []string{"clarity", "difficulty", "verifiable", "cross_cutting", "cause_known"}, Fields: triageFields,
		Meta: map[string]any{"task_hash": hash, "actual": actual, "mode": s.cfg.TriageMode},
		Action: func(r decisionResult) string {
			ans = triageAnswersOf(r.Answers)
			tier, reason = tierFor(ans)
			return tier
		},
	})
	if errors.Is(runErr, errDecisionDisabled) {
		return nil, false, &triageOff{reason: "decider disabled", id: id}
	}
	if runErr != nil {
		// Runtime failures never supply a tier. Persist only a reason class and
		// lane, not provider response bodies or potentially secret transport URLs.
		lane := "pi:openrouter"
		for _, p := range s.role("decider").Use {
			if p.Harness == "api" {
				lane = p.lane()
				break
			}
		}
		return nil, false, &triageOff{reason: "dependency_unavailable, " + lane, id: id}
	}
	return &triageRecord{ID: id, At: a.now().Unix(), Mode: s.cfg.TriageMode, Model: res.Model, TaskHash: hash,
		Actual: actual, Answers: ans, Tier: tier, Reason: reason, Decision: res.ID}, false, nil
}

// triageGate runs at spawn. Shadow mode never blocks. Enforce mode refuses a
// model listed in weak_models for any tier but settled, and refuses to guess
// when the decision model is unreachable.
func (a *app) triageGate(s site, id string, fm map[string]string) error {
	if a.skipTriage {
		return nil
	}
	if reason := s.deciderOff(a.now()); reason != "" {
		a.recordTriageSkip(s, id, reason)
		if s.cfg.TriageMode == "enforce" && !a.overrideTier {
			return &triageOff{reason: reason, id: id}
		}
		fmt.Fprintf(a.out, "triage skipped: decider off (%s)\n", reason)
		return nil
	}
	rec, err := a.triageTask(s, id, false)
	enforce := s.cfg.TriageMode == "enforce" && !a.overrideTier
	if err != nil {
		a.recordTriageSkip(s, id, err.Error())
		if enforce {
			return fail("triage is unavailable (%v). Retry, or spawn with --override-tier after checking the task by hand", err)
		}
		fmt.Fprintf(a.out, "warning: triage unavailable: %v\n", err)
		return nil
	}
	weak := false
	for _, m := range s.cfg.WeakModels {
		if m == fm["model"] {
			weak = true
		}
	}
	if enforce && weak && rec.Tier != "settled" {
		return fail("triage says %s is %s work (%s), and %s is for settled work only. Move it to a stronger profile (orch dispatch), or spawn with --override-tier", id, rec.Tier, rec.Reason, fm["model"])
	}
	if rec.Tier != "settled" && weak {
		fmt.Fprintf(a.out, "warning: triage rates %s as %s work (%s) but it runs on %s (shadow mode: not blocked)\n", id, rec.Tier, rec.Reason, fm["model"])
	}
	return nil
}

func cmdTriage(a *app, args []string) error {
	fs := a.flags("triage")
	force := fs.Bool("force", false, "ask again even if the task text has not changed")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	if len(pos) == 1 && pos[0] == "report" {
		return a.triageReport(s)
	}
	if len(pos) == 0 {
		return &exitError{code: 2, msg: "usage: orch triage <id>... [--force] | orch triage report"}
	}
	if reason := s.deciderOff(a.now()); reason != "" {
		for _, id := range pos {
			a.recordTriageSkip(s, id, reason)
		}
		fmt.Fprintf(a.out, "triage skipped: decider off (%s)\n", reason)
		return nil
	}
	var failed int
	for _, id := range pos {
		rec, err := a.triageTask(s, id, *force)
		if err != nil {
			if off, ok := err.(*triageOff); ok {
				a.recordTriageSkip(s, id, off.reason)
				fmt.Fprintf(a.out, "%s: skipped: decider off (%s)\n", id, off.reason)
				continue
			}
			fmt.Fprintf(a.out, "%s: error: %v\n", id, err)
			failed++
			continue
		}
		an := rec.Answers
		fmt.Fprintf(a.out, "%s: %s (%s)\n  clarity %s %.2f, difficulty %.2f (confidence %.2f), verifiable %.2f, cross-cutting %.2f, cause known %.2f\n  model %s, mode %s\n",
			id, rec.Tier, rec.Reason, an.Clarity, an.ClarityP[an.Clarity], an.Difficulty, min(an.ClarityConf, an.DiffConf), an.Verifiable, an.CrossCutting, an.CauseKnown, rec.Model, rec.Mode)
	}
	if failed > 0 {
		return &exitError{code: 1}
	}
	return nil
}

// triageSignals lists the hard signals in a status log. The lead's own
// reports (escalate, failed) are kept apart from what the run did (stalled,
// looping, verify fail, respawn, frozen), so the report can tell a model that
// gave up from a run that went wrong.
func triageSignals(entries []entry) (lead, run []string) {
	add := func(list *[]string, s string) {
		if !slices.Contains(*list, s) {
			*list = append(*list, s)
		}
	}
	for _, e := range entries {
		switch {
		case e.By != "orch" && e.State == "escalate":
			add(&lead, "escalated")
		case e.By != "orch" && e.State == "failed":
			add(&lead, "failed")
		case e.By == "orch" && (strings.HasPrefix(e.Text, "stalled:") || strings.HasPrefix(e.Text, "looping:")):
			add(&run, "stalled")
		case e.By == "orch" && strings.HasPrefix(e.Text, "verify fail"):
			add(&run, "verify failed")
		case e.By == "orch" && strings.HasPrefix(e.Text, "closed for respawn"):
			add(&run, "respawned")
		}
	}
	if frozenReason(entries) != "" {
		add(&run, "frozen")
	}
	return lead, run
}

// triageOutcome says what happened to a task, from its status log alone:
// "hard" when any of the signals that mean the plan or the model was not
// enough fired, "clean" when it reported done without them, "" while open.
func triageOutcome(entries []entry) (outcome, why string) {
	lead, run := triageSignals(entries)
	if hits := append(lead, run...); len(hits) > 0 {
		return "hard", strings.Join(hits, ", ")
	}
	if lastState(leadEntries(entries)) == "done" {
		return "clean", ""
	}
	return "", ""
}

// latestTriageDecision is the task's newest record that came from a decision.
func latestTriageDecision(root, id string) (triageRecord, bool) {
	recs := readTriage(root)
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].ID == id && recs[i].Decision != "" {
			return recs[i], true
		}
	}
	return triageRecord{}, false
}

// recordTriageOutcome writes what happened to the task's latest triage
// decision. A task with no result yet (still open, or closed without a verdict)
// gets nothing, so a later call can still write one.
func recordTriageOutcome(s site, id string) error {
	rec, ok := latestTriageDecision(s.root, id)
	if !ok {
		return nil
	}
	dir, err := taskDir(s.root, id)
	if err != nil {
		return err
	}
	entries := readStatus(dir)
	outcome, why := triageOutcome(entries)
	if outcome == "" {
		return nil
	}
	lead, run := triageSignals(entries)
	return recordDecisionOutcome(s.root, "triage", rec.Decision, map[string]any{
		"outcome": outcome, "hard": outcome == "hard", "why": why, "tier": rec.Tier,
		"lead_signals": append([]string{}, lead...), "run_signals": append([]string{}, run...),
	})
}

// closedByOrch is true once `orch close` has run on the task. A close for
// respawn writes a different note and does not count: the task goes on.
func closedByOrch(entries []entry) bool {
	for _, e := range entries {
		if e.By == "orch" && strings.HasPrefix(e.Text, "closed by orchestrator") {
			return true
		}
	}
	return false
}

// resolveTriageOutcomes writes the outcome of every closed task whose latest
// triage decision has none yet. It runs from the watcher and from
// `orch decisions report`, and is safe to repeat.
func resolveTriageOutcomes(s site) error {
	done := map[string]bool{}
	outs, err := readJSONLines[decisionOutcome](decisionOutcomeFile(s.root, "triage"))
	if err != nil {
		return err
	}
	for _, o := range outs {
		done[o.ID] = true
	}
	latest := map[string]triageRecord{}
	for _, r := range readTriage(s.root) {
		if r.Decision != "" {
			latest[r.ID] = r
		}
	}
	var errs []error
	for id, r := range latest {
		if done[r.Decision] {
			continue
		}
		dir, err := taskDir(s.root, id)
		if err != nil || !closedByOrch(readStatus(dir)) {
			continue
		}
		if err := recordTriageOutcome(s, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func init() {
	registerOutcomeResolver("triage", resolveTriageOutcomes)
	registerDecisionMetric("triage", decisionMetric{Name: "difficulty", Key: "difficulty", Field: "score", Outcome: "hard"})
}

const triageNewReport = "new records: orch decisions report --use triage"

func (a *app) triageReport(s site) error {
	latest := map[string]triageRecord{}
	var ids []string
	for _, r := range readTriage(s.root) {
		if _, ok := latest[r.ID]; !ok {
			ids = append(ids, r.ID)
		}
		latest[r.ID] = r
	}
	if len(ids) == 0 {
		fmt.Fprintf(a.out, "triage: no records yet (they are written when a task is spawned or triaged); %s\n", triageNewReport)
		return nil
	}
	sort.Strings(ids)
	type agg struct{ n, hard, clean int }
	byTier, byBand := map[string]*agg{}, map[string]*agg{}
	count := func(m map[string]*agg, k, outcome string) {
		if m[k] == nil {
			m[k] = &agg{}
		}
		m[k].n++
		if outcome == "hard" {
			m[k].hard++
		} else {
			m[k].clean++
		}
	}
	band := func(d float64) string {
		switch {
		case d < 0.5:
			return "0.0-0.5 trivial"
		case d < 1.5:
			return "0.5-1.5 easy"
		case d < 2.5:
			return "1.5-2.5 medium"
		}
		return "2.5-3.0 hard"
	}
	var missed, cautious []string
	open, done := 0, 0
	for _, id := range ids {
		r := latest[id]
		dir, err := taskDir(s.root, id)
		if err != nil {
			continue
		}
		outcome, why := triageOutcome(readStatus(dir))
		if outcome == "" {
			open++
			continue
		}
		done++
		count(byTier, r.Tier, outcome)
		count(byBand, band(r.Answers.Difficulty), outcome)
		if (r.Tier == "settled" || r.Tier == "quick") && outcome == "hard" {
			missed = append(missed, fmt.Sprintf("%s (%s; ran on %s)", id, why, r.Actual))
		}
		if r.Tier != "settled" && r.Tier != "quick" && outcome == "clean" {
			cautious = append(cautious, id)
		}
	}
	fmt.Fprintf(a.out, "triage: %d task(s) scored, %d with an outcome, %d still open\n", len(ids), done, open)
	if done < triageMinOutcomes {
		fmt.Fprintf(a.out, "verdict: too few outcomes (%d of %d) to trust any rate; keep shadow mode and let the data grow\n", done, triageMinOutcomes)
	}
	table := func(title string, m map[string]*agg) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(a.out, "%s[%d]{group,tasks,hard,clean,hard_rate}:\n", title, len(keys))
		for _, k := range keys {
			g := m[k]
			fmt.Fprintf(a.out, "  %s,%d,%d,%d,%.0f%%\n", k, g.n, g.hard, g.clean, 100*float64(g.hard)/float64(g.n))
		}
	}
	table("by_tier", byTier)
	table("by_difficulty", byBand)
	fmt.Fprintf(a.out, "settled_but_hard[%d]:\n", len(missed))
	for _, m := range missed {
		fmt.Fprintf(a.out, "  %s\n", agentScalar(m))
	}
	fmt.Fprintf(a.out, "cautious_but_clean: %d task(s) rated above settled finished with no trouble", len(cautious))
	if len(cautious) > 0 {
		fmt.Fprintf(a.out, " (%s)", short(strings.Join(cautious, ", "), 80))
	}
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "read: a good setup has a low hard_rate for settled and a rising rate with difficulty; settled_but_hard is the costly error, cautious_but_clean the cheap one")
	fmt.Fprintln(a.out, triageNewReport)
	return nil
}
