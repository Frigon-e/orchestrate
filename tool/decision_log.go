package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// The decision model: every use asks the Decider through askDecision, which
// records the question, the raw answers, cost and latency. Outcomes are written
// later as separate lines. See references/decision-model.md.

const (
	// decisionStateFieldCap is the most characters of one state field sent.
	decisionStateFieldCap = 2000
	// minOutcomesForVerdict is how many outcomes a use needs before its metrics
	// are worth reading, and before the user may promote it.
	minOutcomesForVerdict = 100
)

var errDecisionDisabled = errors.New("decision role is disabled")

// decisionCall is the decider call; tests replace it.
var decisionCall = postDecisionsRawWithContext

type decisionRequest struct {
	Use       string
	Role      string
	Subject   string // task id or lead id
	State     map[string]any
	Questions map[string]any
	Keys      []string            // answer keys that must be present
	Fields    map[string][]string // per answer key, JSON fields that must be present
	Meta      map[string]any      // stored in the record, never sent
	Action    func(decisionResult) string
}

type decisionResult struct {
	ID      string
	Raw     map[string]json.RawMessage
	Answers map[string]decAnswer
	Model   string
	Usage   shadowUsage
	Latency time.Duration
	Action  string
}

type decisionRecord struct {
	ID        string                     `json:"id"`
	Time      time.Time                  `json:"time"`
	Use       string                     `json:"use"`
	Role      string                     `json:"role"`
	Subject   string                     `json:"subject,omitempty"`
	State     string                     `json:"state_digest"`
	Meta      map[string]any             `json:"meta,omitempty"`
	Questions map[string]any             `json:"questions"`
	Answers   map[string]json.RawMessage `json:"answers,omitempty"`
	Model     string                     `json:"model,omitempty"`
	Cost      float64                    `json:"cost"`
	Tokens    int                        `json:"tokens"`
	LatencyMS int64                      `json:"latency_ms"`
	Reason    string                     `json:"reason,omitempty"` // failure class
	Error     string                     `json:"error,omitempty"`
	Action    string                     `json:"action"`
}

type decisionOutcome struct {
	ID      string         `json:"id"`
	Time    time.Time      `json:"time"`
	Outcome map[string]any `json:"outcome"`
}

func decisionsDir(root string) string { return filepath.Join(root, "decisions") }

func decisionFile(root, use string) string {
	return filepath.Join(decisionsDir(root), use+".jsonl")
}

func decisionOutcomeFile(root, use string) string {
	return filepath.Join(decisionsDir(root), use+".outcomes.jsonl")
}

func roleEnabled(r modelRole) bool { return r.Enabled == nil || *r.Enabled }

func newDecisionID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// capDecisionState cuts each top-level string in the state to the field cap.
func capDecisionState(state map[string]any) map[string]any {
	out := make(map[string]any, len(state))
	for k, v := range state {
		if s, ok := v.(string); ok {
			if r := []rune(s); len(r) > decisionStateFieldCap {
				v = string(r[:decisionStateFieldCap])
			}
		}
		out[k] = v
	}
	return out
}

func stateDigest(state map[string]any) string {
	b, _ := json.Marshal(state) // map keys marshal sorted
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func failureReason(err error) string {
	var de *decisionError
	switch {
	case errors.As(err, &de):
		return de.Reason
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "error"
}

// validateAnswers checks the raw JSON of every required key and field. A
// missing or null value is invalid; it never reads as zero.
func validateAnswers(raw map[string]json.RawMessage, keys []string, fields map[string][]string) error {
	need := append([]string(nil), keys...)
	for k := range fields {
		need = append(need, k)
	}
	seen := map[string]bool{}
	for _, k := range need {
		if seen[k] {
			continue
		}
		seen[k] = true
		a, ok := raw[k]
		if !ok || strings.TrimSpace(string(a)) == "null" {
			return fmt.Errorf("answer %q is missing", k)
		}
		if len(fields[k]) == 0 {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(a, &obj); err != nil {
			return fmt.Errorf("answer %q is not an object", k)
		}
		for _, f := range fields[k] {
			if v, ok := obj[f]; !ok || strings.TrimSpace(string(v)) == "null" {
				return fmt.Errorf("answer %q has no field %q", k, f)
			}
		}
	}
	return nil
}

// askDecision makes one decision call and records it. The deadline is the
// caller's context. A failure is recorded with its reason and returned; the
// caller then keeps today's behaviour. A disabled role writes nothing.
func askDecision(ctx context.Context, s site, req decisionRequest) (decisionResult, error) {
	role := s.role(req.Role)
	if !roleEnabled(role) {
		return decisionResult{}, errDecisionDisabled
	}
	state := capDecisionState(req.State)
	rec := decisionRecord{
		ID: newDecisionID(), Time: time.Now().UTC(), Use: req.Use, Role: req.Role,
		Subject: req.Subject, State: stateDigest(state), Meta: req.Meta,
		Questions: req.Questions, Action: "shadow",
	}
	res := decisionResult{ID: rec.ID, Action: "shadow"}

	var p *profile
	for i := range role.Use {
		if role.Use[i].Harness == "api" {
			p = &role.Use[i]
			break
		}
	}
	var callErr error
	if p == nil {
		callErr = &decisionError{"no_connector", fmt.Errorf("role %s has no api connector", req.Role)}
	} else {
		start := time.Now()
		raw, model, u, err := decisionCall(ctx, *p, state, req.Questions)
		res.Latency = time.Since(start)
		res.Raw, res.Model, res.Usage = raw, model, u
		rec.Answers, rec.Model, rec.Cost, rec.Tokens = raw, model, u.Cost, u.Tokens
		rec.LatencyMS = res.Latency.Milliseconds()
		callErr = err
		if callErr == nil {
			if verr := validateAnswers(raw, req.Keys, req.Fields); verr != nil {
				callErr = &decisionError{"invalid_answer", verr}
			}
		}
		if callErr == nil {
			res.Answers = make(map[string]decAnswer, len(raw))
			for k, r := range raw {
				var a decAnswer
				if err := json.Unmarshal(r, &a); err != nil {
					callErr = &decisionError{"invalid_answer", fmt.Errorf("answer %q: %v", k, err)}
					break
				}
				res.Answers[k] = a
			}
		}
	}
	if callErr != nil {
		rec.Reason, rec.Error = failureReason(callErr), callErr.Error()
	} else if req.Action != nil {
		res.Action = req.Action(res)
		rec.Action = res.Action
	}
	if err := appendDecisionLine(decisionFile(s.root, req.Use), rec); err != nil {
		fmt.Fprintf(os.Stderr, "orch: decision record for %s not written: %v\n", req.Use, err)
	}
	if callErr != nil {
		return res, callErr
	}
	return res, nil
}

// appendDecisionLine adds one JSON line under a file lock; decisions/ is created
// first so the lock file can live there.
func appendDecisionLine(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return withFlock(path, func() error {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(append(b, '\n'))
		return err
	})
}

func readJSONLines[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		var v T
		if json.Unmarshal(sc.Bytes(), &v) == nil {
			out = append(out, v)
		}
	}
	return out, sc.Err()
}

// recordDecisionOutcome appends the outcome of decision id. A second outcome
// for the same ID is ignored: the first one written stands.
func recordDecisionOutcome(root, use, id string, outcome map[string]any) error {
	path := decisionOutcomeFile(root, use)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(decisionOutcome{ID: id, Time: time.Now().UTC(), Outcome: outcome})
	if err != nil {
		return err
	}
	return withFlock(path, func() error {
		have, err := readJSONLines[decisionOutcome](path)
		if err != nil {
			return err
		}
		for _, o := range have {
			if o.ID == id {
				return nil
			}
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(append(b, '\n'))
		return err
	})
}

var (
	resolverMu sync.Mutex
	resolvers  = map[string]func(s site) error{}
	metricsReg = map[string][]decisionMetric{}
)

// registerOutcomeResolver sets the code that writes a use's outcomes from
// durable data once enough time has passed. It must be safe to run repeatedly.
func registerOutcomeResolver(use string, fn func(s site) error) {
	resolverMu.Lock()
	defer resolverMu.Unlock()
	resolvers[use] = fn
}

// resolveOutcomes runs every registered resolver; errors are logged, not returned.
func resolveOutcomes(s site) {
	resolverMu.Lock()
	uses := make([]string, 0, len(resolvers))
	for u := range resolvers {
		uses = append(uses, u)
	}
	sort.Strings(uses)
	fns := make([]func(site) error, len(uses))
	for i, u := range uses {
		fns[i] = resolvers[u]
	}
	resolverMu.Unlock()
	for i, fn := range fns {
		if err := fn(s); err != nil {
			fmt.Fprintf(os.Stderr, "orch: outcome resolver for %s: %v\n", uses[i], err)
		}
	}
}

// decisionMetric declares one score a use wants judged: the numeric Field
// ("noul" or "score") of answer Key, against the boolean Outcome field.
type decisionMetric struct {
	Name    string
	Key     string
	Field   string
	Outcome string
}

func registerDecisionMetric(use string, m decisionMetric) {
	resolverMu.Lock()
	defer resolverMu.Unlock()
	metricsReg[use] = append(metricsReg[use], m)
}

type calBand struct {
	Lo, Hi    float64
	N, Pos    int
	MeanScore float64
}

type scored struct {
	score float64
	pos   bool
}

// aucOf is the chance that a random positive outranks a random negative, ties
// counting half. ok is false when one class is empty.
func aucOf(xs []scored) (auc float64, ok bool) {
	sort.Slice(xs, func(i, j int) bool { return xs[i].score < xs[j].score })
	var pos, neg int
	for _, x := range xs {
		if x.pos {
			pos++
		} else {
			neg++
		}
	}
	if pos == 0 || neg == 0 {
		return 0, false
	}
	var rankSum float64
	for i := 0; i < len(xs); {
		j := i
		for j < len(xs) && xs[j].score == xs[i].score {
			j++
		}
		avg := float64(i+j+1) / 2 // mean of 1-based ranks i+1..j
		for k := i; k < j; k++ {
			if xs[k].pos {
				rankSum += avg
			}
		}
		i = j
	}
	return (rankSum - float64(pos)*float64(pos+1)/2) / (float64(pos) * float64(neg)), true
}

// calibration groups scores into ten probability bands over [0,1].
func calibration(xs []scored) []calBand {
	bands := make([]calBand, 10)
	for i := range bands {
		bands[i].Lo, bands[i].Hi = float64(i)/10, float64(i+1)/10
	}
	for _, x := range xs {
		i := int(math.Floor(x.score * 10))
		i = max(0, min(9, i))
		bands[i].N++
		bands[i].MeanScore += x.score
		if x.pos {
			bands[i].Pos++
		}
	}
	for i := range bands {
		if bands[i].N > 0 {
			bands[i].MeanScore /= float64(bands[i].N)
		}
	}
	return bands
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(len(sorted)-1, i))]
}

const decisionsUsage = "usage: orch decisions report [--use U] [--since 7d]"

func cmdDecisions(a *app, args []string) error {
	if len(args) == 0 || args[0] != "report" {
		return &exitError{code: 2, msg: decisionsUsage}
	}
	fs := a.flags("decisions report")
	useFlag := fs.String("use", "", "only this use")
	since := fs.String("since", "", "only decisions this recent, such as 90m, 36h or 7d")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return &exitError{code: 2, msg: decisionsUsage}
	}
	var cutoff time.Time
	if *since != "" {
		d, err := parseSince(*since)
		if err != nil {
			return &exitError{code: 2, msg: err.Error()}
		}
		cutoff = a.now().Add(-d)
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	return reportDecisions(a, s, *useFlag, cutoff)
}

// reportDecisions resolves outcomes first, then prints one section per use.
func reportDecisions(a *app, s site, only string, cutoff time.Time) error {
	resolveOutcomes(s)
	uses, err := decisionUses(s.root)
	if err != nil {
		return err
	}
	if only != "" {
		uses = []string{only}
	} else {
		// A use with only old records still has a report.
		for u := range legacyReaders {
			if !slices.Contains(uses, u) && len(legacyReaders[u](s)) > 0 {
				uses = append(uses, u)
			}
		}
		sort.Strings(uses)
	}
	if len(uses) == 0 {
		fmt.Fprintln(a.out, "no decisions recorded")
		return nil
	}
	for _, u := range uses {
		if err := reportDecisionUse(a, s, u, cutoff); err != nil {
			return err
		}
		reportLegacyGroup(a, s, u, cutoff)
	}
	return nil
}

func decisionUses(root string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(decisionsDir(root), "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var uses []string
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		if !strings.HasSuffix(name, ".outcomes") {
			uses = append(uses, name)
		}
	}
	sort.Strings(uses)
	return uses, nil
}

func numField(raw json.RawMessage, field string) (float64, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return 0, false
	}
	var f float64
	if v, ok := obj[field]; !ok || json.Unmarshal(v, &f) != nil {
		return 0, false
	}
	return f, true
}

func reportDecisionUse(a *app, s site, use string, cutoff time.Time) error {
	recs, err := readJSONLines[decisionRecord](decisionFile(s.root, use))
	if err != nil {
		return err
	}
	outs, err := readJSONLines[decisionOutcome](decisionOutcomeFile(s.root, use))
	if err != nil {
		return err
	}
	outcome := map[string]map[string]any{}
	for _, o := range outs {
		if _, dup := outcome[o.ID]; !dup {
			outcome[o.ID] = o.Outcome
		}
	}
	var kept []decisionRecord
	for _, r := range recs {
		if cutoff.IsZero() || !r.Time.Before(cutoff) {
			kept = append(kept, r)
		}
	}
	w := a.out
	fmt.Fprintf(w, "== %s ==\n", use)
	if len(kept) == 0 {
		fmt.Fprintln(w, "no decisions in range")
		return nil
	}
	role := kept[len(kept)-1].Role
	state := "enabled"
	if role != "" && !roleEnabled(s.role(role)) {
		state = "disabled"
	}
	fails := map[string]int{}
	var cost float64
	var lat []int64
	var okN, withOutcome int
	choices := map[string]map[string]int{}
	for _, r := range kept {
		cost += r.Cost
		if r.LatencyMS > 0 {
			lat = append(lat, r.LatencyMS)
		}
		if r.Error != "" {
			fails[orDefault(r.Reason, "error")]++
			continue
		}
		okN++
		if _, ok := outcome[r.ID]; ok {
			withOutcome++
		}
		for k, raw := range r.Answers {
			var c struct {
				Choice string `json:"choice"`
			}
			if json.Unmarshal(raw, &c) == nil && c.Choice != "" {
				if choices[k] == nil {
					choices[k] = map[string]int{}
				}
				choices[k][c.Choice]++
			}
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	fmt.Fprintf(w, "role %s: %s\n", orDefault(role, "?"), state)
	fmt.Fprintf(w, "calls %d, failures %d%s\n", len(kept), len(kept)-okN, failureList(fails))
	fmt.Fprintf(w, "cost $%.4f, latency p50 %dms p90 %dms\n", cost, percentile(lat, 0.5), percentile(lat, 0.9))
	cov := 0.0
	if okN > 0 {
		cov = 100 * float64(withOutcome) / float64(okN)
	}
	fmt.Fprintf(w, "outcome coverage %d/%d (%.0f%%)\n", withOutcome, okN, cov)
	for _, k := range sortedKeys(choices) {
		fmt.Fprintf(w, "answers %s:", k)
		for _, c := range sortedKeys(choices[k]) {
			fmt.Fprintf(w, " %s=%d", c, choices[k][c])
		}
		fmt.Fprintln(w)
	}
	resolverMu.Lock()
	metrics := append([]decisionMetric(nil), metricsReg[use]...)
	resolverMu.Unlock()
	for _, m := range metrics {
		var xs []scored
		for _, r := range kept {
			o, ok := outcome[r.ID]
			if !ok || r.Error != "" {
				continue
			}
			pos, isBool := o[m.Outcome].(bool)
			sc, haveScore := numField(r.Answers[m.Key], m.Field)
			if isBool && haveScore {
				xs = append(xs, scored{sc, pos})
			}
		}
		printScoreTable(w, fmt.Sprintf("metric %s (%s.%s vs %s)", m.Name, m.Key, m.Field, m.Outcome), xs)
	}
	return nil
}

// printScoreTable prints the outcome count, the AUC and the calibration bands
// of one score against a boolean outcome.
func printScoreTable(w io.Writer, title string, xs []scored) {
	fmt.Fprintf(w, "%s: %d outcomes", title, len(xs))
	if len(xs) < minOutcomesForVerdict {
		fmt.Fprintf(w, ", fewer than %d outcomes\n", minOutcomesForVerdict)
	} else {
		fmt.Fprintln(w)
	}
	if auc, ok := aucOf(append([]scored(nil), xs...)); ok {
		fmt.Fprintf(w, "  AUC %.3f\n", auc)
	} else {
		fmt.Fprintln(w, "  AUC n/a (one outcome class only)")
	}
	fmt.Fprintln(w, "  band        n  mean  positive")
	for _, b := range calibration(xs) {
		if b.N == 0 {
			continue
		}
		fmt.Fprintf(w, "  %.1f-%.1f %4d  %.2f  %.2f\n", b.Lo, b.Hi, b.N, b.MeanScore, float64(b.Pos)/float64(b.N))
	}
}

// legacyDecision is an old record that predates the helper, in the scored form
// the report reads. Outcome is "" while the task has no result or no status log.
type legacyDecision struct {
	ID      string
	At      time.Time
	Scores  map[string]float64 // raw answers by report name, each scaled into [0,1]
	Outcome string             // "hard", "clean" or ""
	RunHard bool               // a run signal fired, not only a lead one
}

// legacyReaders turn a use's old records into scored form at report time.
// Nothing is written to decisions/.
var legacyReaders = map[string]func(s site) []legacyDecision{}

// legacyScoreOrder is the order the raw answers print in.
var legacyScoreOrder = []string{"expected difficulty / 3", "P(difficulty=3)", "P(verifiable)", "P(cause known)"}

// reportLegacyGroup prints the legacy group of one use after its helper group.
func reportLegacyGroup(a *app, s site, use string, cutoff time.Time) {
	read := legacyReaders[use]
	if read == nil {
		return
	}
	var kept []legacyDecision
	for _, d := range read(s) {
		if cutoff.IsZero() || !d.At.Before(cutoff) {
			kept = append(kept, d)
		}
	}
	if len(kept) == 0 {
		return
	}
	w := a.out
	withOutcome := 0
	for _, d := range kept {
		if d.Outcome != "" {
			withOutcome++
		}
	}
	fmt.Fprintf(w, "-- %s: legacy (triage/triage.jsonl, outcomes from status logs; read-only) --\n", use)
	fmt.Fprintf(w, "tasks %d, outcome coverage %d/%d (%.0f%%), no outcome %d\n",
		len(kept), withOutcome, len(kept), 100*float64(withOutcome)/float64(len(kept)), len(kept)-withOutcome)
	for _, view := range []struct {
		name string
		pos  func(legacyDecision) bool
	}{
		{"hard overall", func(d legacyDecision) bool { return d.Outcome == "hard" }},
		{"hard from run_signals only", func(d legacyDecision) bool { return d.RunHard }},
	} {
		for _, name := range legacyScoreOrder {
			var xs []scored
			for _, d := range kept {
				if d.Outcome != "" {
					xs = append(xs, scored{d.Scores[name], view.pos(d)})
				}
			}
			printScoreTable(w, fmt.Sprintf("legacy %s vs %s", name, view.name), xs)
		}
	}
}

func failureList(f map[string]int) string {
	if len(f) == 0 {
		return ""
	}
	var parts []string
	for _, k := range sortedKeys(f) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, f[k]))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}
