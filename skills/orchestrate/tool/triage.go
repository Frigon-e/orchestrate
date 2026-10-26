package main

import (
	"crypto/sha256"
	"encoding/json"
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
	triageTimeout      = 30 * time.Second
	triageMinOutcomes  = 20 // outcomes needed before the report trusts a rate

	settledClear      = 0.8 // P(clear) needed for settled
	settledDifficulty = 1.0 // expected difficulty at most this for settled
	settledVerifiable = 0.8
	crossCuttingMax   = 0.5
	hardPlanning      = 0.3 // P(hard) at or above this means planning
	openEndedPlanning = 0.5
	causeUnknown      = 0.2 // P(cause known) below this means planning
	causeKnownMin     = 0.5
	lowConfidence     = 0.6 // below this the tier moves up one step
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
	case a.ClarityP["open_ended"] >= openEndedPlanning:
		tier, reason = "planning", fmt.Sprintf("open-ended (%.2f)", a.ClarityP["open_ended"])
	case a.DifficultyP["3"] >= hardPlanning:
		tier, reason = "planning", fmt.Sprintf("hard (P=%.2f)", a.DifficultyP["3"])
	case a.CauseKnown < causeUnknown:
		tier, reason = "planning", fmt.Sprintf("unknown cause (P(known)=%.2f)", a.CauseKnown)
	case a.ClarityP["clear"] >= settledClear && a.Difficulty <= settledDifficulty &&
		a.Verifiable >= settledVerifiable && a.CrossCutting < crossCuttingMax && a.CauseKnown >= causeKnownMin:
		tier, reason = "settled", fmt.Sprintf("clear %.2f, difficulty %.2f, verifiable %.2f", a.ClarityP["clear"], a.Difficulty, a.Verifiable)
	default:
		tier, reason = "standard", "not clear and easy and verifiable enough for settled"
	}
	if c := min(a.ClarityConf, a.DiffConf); c < lowConfidence && tier != "planning" {
		up := map[string]string{"settled": "standard", "standard": "planning"}[tier]
		reason += fmt.Sprintf("; confidence %.2f is low, moved up from %s", c, tier)
		tier = up
	}
	return tier, reason
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

// callDecider posts one decisions request and returns the answers and the
// model version that served it.
func callDecider(model string, state map[string]any) (triageAnswers, string, error) {
	return callDeciderWith(profile{Harness: "api", Provider: "openrouter", Model: model}, state)
}

func callDeciderWith(p profile, state map[string]any) (triageAnswers, string, error) {
	var out triageAnswers
	answers, served, _, err := postDecisionsWith(p, state, triageQuestions())
	if err != nil {
		return out, "", err
	}
	r := struct {
		Model   string
		Answers map[string]decAnswer
	}{served, answers}
	for _, k := range []string{"clarity", "difficulty", "verifiable", "cross_cutting", "cause_known"} {
		if _, ok := r.Answers[k]; !ok {
			return out, "", fmt.Errorf("decisions API reply lacks the %q answer", k)
		}
	}
	c, d := r.Answers["clarity"], r.Answers["difficulty"]
	out = triageAnswers{Clarity: c.Choice, ClarityP: c.Probabilities, ClarityConf: c.Confidence,
		Difficulty: d.Score, DifficultyP: d.Probabilities, DiffConf: d.Confidence,
		Verifiable: r.Answers["verifiable"].Noul, CrossCutting: r.Answers["cross_cutting"].Noul, CauseKnown: r.Answers["cause_known"].Noul}
	return out, r.Model, nil
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
	runner, _ := s.newRoleRun("decider", modelOptions{})
	var ans triageAnswers
	var served string
	_, _, used, runErr := runner.run(func(p profile) (string, string, error) {
		var e error
		ans, served, e = callDeciderWith(p, state)
		return "", "", e
	})
	if runErr != nil {
		// Runtime failures never supply a tier. Persist only a reason class and
		// lane, not provider response bodies or potentially secret transport URLs.
		return nil, false, &triageOff{reason: "dependency_unavailable, " + used.lane(), id: id}
	}
	tier, reason := tierFor(ans)
	fm, _, _ := readTask(filepath.Join(dir, "task.md"))
	return &triageRecord{ID: id, At: a.now().Unix(), Mode: s.cfg.TriageMode, Model: served, TaskHash: hash,
		Actual: fm["harness"] + " " + fm["model"], Answers: ans, Tier: tier, Reason: reason}, false, nil
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

// triageOutcome says what happened to a task, from its status log alone:
// "hard" when any of the signals that mean the plan or the model was not
// enough fired, "clean" when it reported done without them, "" while open.
func triageOutcome(entries []entry) (outcome, why string) {
	var hits []string
	add := func(s string) {
		if slices.Contains(hits, s) {
			return
		}
		hits = append(hits, s)
	}
	for _, e := range entries {
		switch {
		case e.By != "orch" && e.State == "escalate":
			add("escalated")
		case e.By != "orch" && e.State == "failed":
			add("failed")
		case e.By == "orch" && (strings.HasPrefix(e.Text, "stalled:") || strings.HasPrefix(e.Text, "looping:")):
			add("stalled")
		case e.By == "orch" && strings.HasPrefix(e.Text, "verify fail"):
			add("verify failed")
		case e.By == "orch" && strings.HasPrefix(e.Text, "closed for respawn"):
			add("respawned")
		}
	}
	if frozenReason(entries) != "" {
		add("frozen")
	}
	if len(hits) > 0 {
		return "hard", strings.Join(hits, ", ")
	}
	if lastState(leadEntries(entries)) == "done" {
		return "clean", ""
	}
	return "", ""
}

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
		fmt.Fprintln(a.out, "triage: no records yet (they are written when a task is spawned or triaged)")
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
	return nil
}
