package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Shared by gate compare and bench. Current scores count keyed behaviors;
// the explicitly legacy scorer below retains deterministic location matching.
type locationScore struct {
	Found       int      `json:"found"`
	Total       int      `json:"total"`
	Match       int      `json:"match"`
	Precision   *float64 `json:"precision"`
	Recall      *float64 `json:"recall"`
	F1          *float64 `json:"f1"`
	Denominator string   `json:"denominator,omitempty"`
	Excluded    int      `json:"excluded,omitempty"`
	Unavailable string   `json:"unavailable,omitempty"`
}

// scoreLocations is now behavior-based. Evidence ranges are acceptable targets,
// never identities. Precision conditions on supported answers; recall retains
// every keyed behavior, including missing and unsupported answers.
func scoreLocations(a *armData, ref map[string]refEntry, judge, kind string) locationScore {
	s := locationScore{Denominator: "precision: supported predicted behaviors in mapped key; recall: all keyed behaviors of class; non-key predictions excluded"}
	issues := referenceIssues(ref)
	if len(issues) > 0 {
		s.Unavailable = strings.Join(issues, "; ")
	}
	for id, r := range ref {
		if normVerdict(r.Verdict) == kind {
			s.Total++
		}
		answer := a.answer(judge, id)
		if !answer.Present || !answer.Valid || !answer.Supported {
			s.Excluded++
			continue
		}
		if answer.Verdict != kind {
			continue
		}
		s.Found++
		if normVerdict(r.Verdict) != kind {
			continue
		}
		if kind == verdictNew || kind == verdictUnclr {
			s.Match++
			continue
		}
		for _, target := range r.targets() {
			if sameLocation(answer.Candidate, target) {
				s.Match++
				break
			}
		}
	}
	if s.Unavailable != "" {
		return s
	}
	if s.Found > 0 {
		v := float64(s.Match) / float64(s.Found)
		s.Precision = &v
	}
	if s.Total > 0 {
		v := float64(s.Match) / float64(s.Total)
		s.Recall = &v
	}
	if s.Found+s.Total > 0 {
		v := 2 * float64(s.Match) / float64(s.Found+s.Total)
		s.F1 = &v
	}
	return s
}

// Historical range-overlap heuristics are retained only as explicitly legacy
// output. They neither establish behavior accuracy nor participate in rankings.
func scoreLegacyLocations(a *armData, ref map[string]refEntry, judge, kind string) locationScore {
	pos := a.locations[judge][kind]
	targets := map[string]bool{}
	for _, r := range ref {
		if normVerdict(r.Verdict) == kind && r.Candidate != "" {
			targets[r.Candidate] = true
		}
	}
	ps, ts := benchSortedKeys(pos), benchSortedKeys(targets)
	paired := map[string]string{}
	var match func(string, map[string]bool) bool
	match = func(p string, seen map[string]bool) bool {
		for _, t := range ts {
			if seen[t] || !sameLocation(p, t) {
				continue
			}
			seen[t] = true
			if prev, ok := paired[t]; !ok || match(prev, seen) {
				paired[t] = p
				return true
			}
		}
		return false
	}
	s := locationScore{Found: len(ps), Total: len(ts), Denominator: "legacy unique locations, not behavior identities"}
	for _, p := range ps {
		if match(p, map[string]bool{}) {
			s.Match++
		}
	}
	if s.Found > 0 {
		v := float64(s.Match) / float64(s.Found)
		s.Precision = &v
	}
	if s.Total > 0 {
		v := float64(s.Match) / float64(s.Total)
		s.Recall = &v
	}
	if s.Found+s.Total > 0 {
		v := 2 * float64(s.Match) / float64(s.Found+s.Total)
		s.F1 = &v
	}
	return s
}
func benchSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func benchExists(p string) bool { _, e := os.Stat(p); return e == nil }
func benchCommand(args []string) string {
	parts := []string{"orch", "gate"}
	for _, v := range args {
		parts = append(parts, "'"+strings.ReplaceAll(v, "'", "'\"'\"'")+"'")
	}
	return strings.Join(parts, " ")
}
func benchRunCost(r *benchRun) benchCost {
	c := benchCost{Basis: "none"}
	sum := 0.0
	known := 0
	for _, g := range r.Groups {
		if g.Cost.USD == nil {
			c.Missing++
			continue
		}
		known++
		sum += *g.Cost.USD
		c.Parts = c.Parts.add(g.Cost.Parts)
		c.Missing += g.Cost.Missing
	}
	if known > 0 {
		c.USD = &sum
		c.Basis = "reported"
	}
	return c
}
func benchCostText(c benchCost) string {
	if c.USD == nil {
		return "not recorded"
	}
	s := fmt.Sprintf("$%.2f", *c.USD)
	if c.Missing > 0 {
		s += " (incomplete)"
	}
	return s
}
func benchNum(v float64) string {
	if v >= 1e6 {
		return fmt.Sprintf("%.1fM", v/1e6)
	}
	if v >= 1000 {
		return fmt.Sprintf("%.0fk", v/1000)
	}
	return fmt.Sprintf("%.0f", v)
}
func benchGroupValue(g benchGroup, metric string) *float64 {
	var p *int
	switch metric {
	case "wall":
		p = g.Seconds
	case "requests":
		p = g.Requests
	case "turns":
		p = g.Turns
	case "max_context":
		p = g.MaxContext
	case "cache_read":
		p = g.Tokens["cache_read"]
	}
	if p == nil {
		return nil
	}
	v := float64(*p)
	return &v
}
func benchSelectedGroups(r *benchRun, groups []string) []string {
	if len(groups) > 0 {
		return groups
	}
	return benchSortedKeys(r.Groups)
}
func benchGroupSum(r *benchRun, metric string, groups []string) *float64 {
	sum := 0.0
	names := benchSelectedGroups(r, groups)
	if len(names) == 0 {
		return nil
	}
	for _, n := range names {
		v := benchGroupValue(r.Groups[n], metric)
		if v == nil {
			return nil
		}
		if metric == "max_context" {
			sum = math.Max(sum, *v)
		} else {
			sum += *v
		}
	}
	return &sum
}
func benchGroupText(r *benchRun, metric string, groups []string, total bool) string {
	format := func(v *float64) string {
		if v == nil {
			return "not recorded"
		}
		switch metric {
		case "wall":
			return fmt.Sprintf("%.0f s", *v)
		case "max_context", "cache_read":
			return benchNum(*v)
		}
		return fmt.Sprintf("%.0f", *v)
	}
	if total {
		return format(benchGroupSum(r, metric, groups))
	}
	var cells []string
	for _, n := range benchSelectedGroups(r, groups) {
		cells = append(cells, format(benchGroupValue(r.Groups[n], metric)))
	}
	if len(cells) == 0 {
		return "not recorded"
	}
	return strings.Join(cells, " and ")
}
func benchRatio(base, current *float64, metric string) string {
	if base == nil || current == nil || *base == 0 {
		return "not recorded"
	}
	if *current == 0 {
		return "-100%"
	}
	ratio := *base / *current
	if ratio >= 1.25 || ratio <= 0.8 {
		word := "fewer"
		switch metric {
		case "wall":
			word = "faster"
		case "cache_read", "max_context", "cost":
			word = "less"
		}
		if ratio < 1 {
			ratio = 1 / ratio
			word = "more"
			if metric == "wall" {
				word = "slower"
			}
		}
		return fmt.Sprintf("about %.1fx %s", ratio, word)
	}
	return fmt.Sprintf("%+.1f%%", (*current / *base - 1)*100)
}

type benchTableRow struct {
	Metric  string   `json:"metric"`
	Values  []string `json:"values"`
	Changes []string `json:"changes"`
}
type benchTable struct {
	Suite    string          `json:"suite"`
	Key      string          `json:"key"`
	Runs     []string        `json:"runs"`
	Baseline string          `json:"baseline"`
	Groups   []string        `json:"groups"`
	Rows     []benchTableRow `json:"rows"`
	Notes    []string        `json:"notes"`
}

func benchComparison(suite string, s benchSuite, runs []*benchRun, base *benchRun, key *benchKey, total bool) (benchTable, error) {
	groups, notes, e := benchComparisonScope(s, runs, base, key, total)
	if e != nil {
		return benchTable{}, e
	}
	s.Groups = groups
	t := benchTable{Suite: suite, Key: "not recorded", Baseline: base.ID, Groups: groups, Notes: notes}
	if key != nil {
		t.Key = key.ID
	}
	for _, r := range runs {
		t.Runs = append(t.Runs, r.Label)
		if reason := benchRankingCompatibility(base, r); reason != "" {
			t.Notes = append(t.Notes, r.Label+": no comparative ranking ("+reason+")")
		}
	}
	t.Notes = append(t.Notes, "Performance denominators: printed groups/passes only; wall clock is full-run elapsed time. Quality denominators: keyed behaviors in those packages, not distinct ranges. Missing/invalid/unsupported answers remain in coverage; conditional accuracy excludes them. Legacy location heuristics are labeled and excluded from rankings.")
	add := func(name, metric string, values []string, nums []*float64, baseline *float64) {
		any := false
		for _, v := range values {
			if v != "not recorded" {
				any = true
			}
		}
		if !any {
			return
		}
		row := benchTableRow{Metric: name, Values: values}
		for i, r := range runs {
			if r.ID != base.ID {
				var v *float64
				if i < len(nums) {
					v = nums[i]
				}
				change := benchRatio(baseline, v, metric)
				if reason := benchRankingCompatibility(base, r); reason != "" {
					change = "not comparable: " + reason
				}
				row.Changes = append(row.Changes, r.Label+": "+change)
			}
		}
		t.Rows = append(t.Rows, row)
	}
	for _, m := range []struct{ key, name string }{{"wall", "Wall time"}, {"requests", "Requests"}, {"turns", "Turns"}, {"max_context", "Max context"}, {"cache_read", "Cache read"}} {
		var vs []string
		var ns []*float64
		for _, r := range runs {
			vs = append(vs, benchGroupText(r, m.key, s.Groups, total))
			ns = append(ns, benchGroupSum(r, m.key, s.Groups))
		}
		add(m.name, m.key, vs, ns, benchGroupSum(base, m.key, s.Groups))
	}
	var wallValues []string
	var wallNums []*float64
	for _, r := range runs {
		value := "not recorded"
		if r.WallSeconds.Value != nil {
			value = fmt.Sprintf("%.0f s (%s)", *r.WallSeconds.Value, r.WallSeconds.Basis)
		}
		wallValues = append(wallValues, value)
		wallNums = append(wallNums, r.WallSeconds.Value)
	}
	add("Wall clock", "wall", wallValues, wallNums, base.WallSeconds.Value)
	var cs []string
	var cn []*float64
	for _, r := range runs {
		c := benchRunCostScoped(r, s.Groups)
		cs = append(cs, benchCostText(c))
		if c.Missing == 0 {
			cn = append(cn, c.USD)
		} else {
			cn = append(cn, nil)
		}
	}
	bc := benchRunCostScoped(base, s.Groups)
	bp := bc.USD
	if bc.Missing > 0 {
		bp = nil
	}
	add("Cost", "cost", cs, cn, bp)
	for _, m := range benchToolRows {
		var vs []string
		var ns []*float64
		for _, r := range runs {
			v := benchToolSum(r, s.Groups, m.count)
			ns = append(ns, v)
			if v == nil {
				vs = append(vs, "not recorded")
			} else {
				vs = append(vs, fmt.Sprintf("%.0f", *v))
			}
		}
		add(m.name, "calls", vs, ns, benchToolSum(base, s.Groups, m.count))
	}
	pkgs := map[string]bool{}
	for _, r := range runs {
		for _, g := range benchSelectedGroups(r, s.Groups) {
			for _, p := range r.Groups[g].Packages {
				pkgs[p] = true
			}
		}
	}
	reuse := func(r *benchRun) float64 {
		if r.Findings == nil {
			return 0
		}
		if len(s.Groups) == 0 {
			return float64(r.Findings.Reuse)
		}
		var rec gateRecord
		if benchReadJSON(filepath.Join(r.dir, "snapshot", "gate.json"), &rec) != nil {
			return float64(r.Findings.Reuse)
		}
		n := 0
		for _, f := range rec.Findings {
			if f.Source != "reuse" {
				continue
			}
			p := strings.TrimPrefix(f.Where, "package ")
			if pkgs[p] {
				n++
			}
		}
		return float64(n)
	}
	var vs []string
	var ns []*float64
	for _, r := range runs {
		if r.Kind == "report-only" {
			vs = append(vs, "not recorded")
			ns = append(ns, nil)
			continue
		}
		v := reuse(r)
		vs = append(vs, fmt.Sprintf("%.0f", v))
		ns = append(ns, &v)
	}
	b := reuse(base)
	add(fmt.Sprintf("Reuse findings (%d packages)", len(pkgs)), "reuse", vs, ns, &b)
	for _, field := range []string{"ctx_tool", "runner", "items with queries", "queries run", "candidate locations", "bodies shown"} {
		var vs []string
		for _, r := range runs {
			v := "not recorded"
			switch field {
			case "ctx_tool":
				v = orDefault(r.Config.CtxTool, v)
			case "runner":
				var rs []string
				for _, g := range benchSelectedGroups(r, s.Groups) {
					rs = append(rs, orDefault(r.Groups[g].Runner, "not recorded"))
				}
				v = strings.Join(rs, " and ")
			case "items with queries", "queries run", "candidate locations", "bodies shown":
				if r.Items != nil {
					n := r.Items.WithQueries
					switch field {
					case "queries run":
						n = r.Items.QueriesRun
					case "candidate locations":
						n = r.Items.CandidateLocations
					case "bodies shown":
						n = r.Items.BodiesShown
					}
					v = fmt.Sprint(n)
				}
			}
			vs = append(vs, v)
		}
		name := field
		if field != "ctx_tool" && field != "runner" {
			name += " (whole recorded run)"
		}
		add(name, field, vs, nil, nil)
	}
	if key != nil {
		t.Notes = append(t.Notes, "New scores: "+benchScorerSchema+" / "+benchScorerProfile+" recomputed read-only; original recorded scores below remain legacy evidence, not current approval.")
		for _, class := range []string{"duplicate", "variation_site", "none", "unknown"} {
			var vs []string
			for _, r := range runs {
				vs = append(vs, benchOriginalScoreText(r, key.ID, class))
			}
			add("old recorded "+class+" score (legacy, unversioned)", class, vs, nil, nil)
		}
		scores := make([]benchScore, len(runs))
		for i, r := range runs {
			scoped := *r
			if len(groups) > 0 {
				scoped.Groups = map[string]benchGroup{}
				for _, g := range groups {
					scoped.Groups[g] = r.Groups[g]
				}
			}
			v, e := benchRescore(&scoped, key)
			if e != nil {
				return t, e
			}
			scores[i] = v
			t.Notes = append(t.Notes, r.Label+": "+v.Coverage)
			if v.ItemNote != "" {
				t.Notes = append(t.Notes, r.Label+": "+v.ItemNote)
			}
		}
		for _, class := range []string{"duplicate", "variation_site", "none", "unknown"} {
			for _, metric := range []string{"supported found / key behaviors", "precision", "recall", "F1"} {
				var vs []string
				for _, score := range scores {
					c, known := score.Classes[class]
					if !known || c.Unavailable != "" {
						v := "not recorded"
						if score.Unavailable != "" {
							v = "unavailable (see notes)"
						}
						vs = append(vs, v)
						continue
					}
					v := "not recorded"
					switch metric {
					case "supported found / key behaviors":
						v = fmt.Sprintf("%d / %d (%d matched; %d missing/invalid/unsupported excluded from precision)", c.Found, c.Total, c.Match, c.Excluded)
					case "precision":
						v = fmt.Sprintf("%s (%d/%d supported predictions; non-key/missing/invalid/unsupported excluded)", benchPercent(c.Precision), c.Match, c.Found)
					case "recall":
						v = fmt.Sprintf("%s (%d/%d all keyed behaviors of class)", benchPercent(c.Recall), c.Match, c.Total)
					case "F1":
						v = fmt.Sprintf("%s (2*%d/(%d+%d) supported predictions + keyed behaviors)", benchPercent(c.F1), c.Match, c.Found, c.Total)
					}
					vs = append(vs, v)
				}
				add(class+" "+metric, class, vs, nil, nil)
			}
		}
		for _, class := range []string{"duplicate", "variation_site"} {
			var vs []string
			for _, score := range scores {
				c, ok := score.Seen[class]
				if !ok || c.Total == 0 {
					vs = append(vs, "not recorded")
					continue
				}
				v := fmt.Sprintf("rendered %d/%d; retrieved %d/%d; author hint %d/%d; unknown visibility %d/%d; unknown retrieval %d/%d", c.Seen, c.Total, c.Retrieved, c.Total, c.Hint, c.Total, c.Unknown, c.Total, c.RetrievalUnknown, c.Total)
				vs = append(vs, v)
			}
			add(class+" key target visibility (all keyed targets)", class, vs, nil, nil)
		}
		for _, j := range []string{"group", "decider", "flash"} {
			var vs []string
			for _, score := range scores {
				k, ok := score.KeyAgree[j]
				if !ok {
					vs = append(vs, "not recorded")
					continue
				}
				vs = append(vs, keyAgreeText(k))
			}
			add("Agreement with the key ("+j+")", j, vs, nil, nil)
		}
		for _, class := range []string{"duplicate", "variation_site"} {
			var vs []string
			for _, score := range scores {
				c, known := score.LegacyClasses[class]
				if !known {
					vs = append(vs, "not recorded")
					continue
				}
				vs = append(vs, fmt.Sprintf("%d/%d matched unique locations (legacy precision %s, recall %s; not behavior quality)", c.Match, c.Total, benchPercent(c.Precision), benchPercent(c.Recall)))
			}
			add("legacy "+class+" location heuristic", class, vs, nil, nil)
		}
		judges := map[string]bool{}
		for _, score := range scores {
			for j := range score.Judges {
				judges[j] = true
			}
		}
		for _, j := range benchSortedKeys(judges) {
			var vs []string
			for _, score := range scores {
				v := "not recorded"
				if x, ok := score.Judges[j]; ok && x.Both > 0 {
					v = fmt.Sprintf("%d/%d (%s)", x.Agree, x.Both, pct(x.Agree, x.Both))
				}
				vs = append(vs, v)
			}
			add("Shadow "+j+" agreement", j, vs, nil, nil)
		}
	}
	// Free metrics allow retrieval benchmarks to use this container too.
	metrics := map[string]bool{}
	for _, r := range runs {
		for m := range r.Metrics {
			metrics[m] = true
		}
	}
	for _, m := range benchSortedKeys(metrics) {
		var vs []string
		var nums []*float64
		for _, r := range runs {
			v, ok := r.Metrics[m]
			if !ok {
				vs = append(vs, "not recorded")
				nums = append(nums, nil)
			} else {
				vs = append(vs, fmt.Sprintf("%g %s", v.Value, v.Unit))
				n := v.Value
				nums = append(nums, &n)
			}
		}
		var bv *float64
		if v, ok := base.Metrics[m]; ok {
			bv = &v.Value
		}
		add(m, m, vs, nums, bv)
	}
	return t, nil
}
func benchRunCostScoped(r *benchRun, groups []string) benchCost {
	if len(groups) == 0 {
		return benchRunCost(r)
	}
	cp := *r
	cp.Groups = map[string]benchGroup{}
	for _, g := range groups {
		cp.Groups[g] = r.Groups[g]
	}
	return benchRunCost(&cp)
}
func benchPercent(p *float64) string {
	if p == nil {
		return "not recorded"
	}
	return fmt.Sprintf("%.1f%%", *p*100)
}
func benchMD(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ")
}
func benchPrintTable(w io.Writer, t benchTable) {
	fmt.Fprintf(w, "Key: %s. Baseline: %s. Groups: %s.\n\n", t.Key, t.Baseline, strings.Join(t.Groups, ", "))
	for _, note := range t.Notes {
		fmt.Fprintln(w, note)
	}
	if len(t.Notes) > 0 {
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, "| Metric |")
	for _, r := range t.Runs {
		fmt.Fprintf(w, " %s |", benchMD(r))
	}
	fmt.Fprintln(w, " Change |")
	fmt.Fprint(w, "| --- |")
	for range t.Runs {
		fmt.Fprint(w, " --- |")
	}
	fmt.Fprintln(w, " --- |")
	for _, row := range t.Rows {
		fmt.Fprintf(w, "| %s |", benchMD(row.Metric))
		for _, v := range row.Values {
			fmt.Fprintf(w, " %s |", benchMD(v))
		}
		fmt.Fprintf(w, " %s |\n", benchMD(strings.Join(row.Changes, "; ")))
	}
}

type benchConfigDiff struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Changes    []string `json:"changes"`
	Confounded bool     `json:"confounded"`
}
type benchLane struct {
	Runner          string  `json:"runner"`
	PassClass       string  `json:"pass_class"`
	USD             float64 `json:"usd"`
	Seconds         int     `json:"seconds"`
	Reported        int     `json:"reported"`
	None            int     `json:"none"`
	MissingRequests int     `json:"missing_requests"`
}
type benchQuality struct {
	Run              string    `json:"run"`
	Cost             benchCost `json:"cost"`
	DuplicateMatches int       `json:"duplicate_matches"`
	VariationMatches int       `json:"variation_matches"`
	CostPerDuplicate *float64  `json:"cost_per_duplicate"`
	CostPerVariation *float64  `json:"cost_per_variation"`
	Pareto           bool      `json:"pareto"`
	Eligible         bool      `json:"eligible"`
	Exclusion        string    `json:"exclusion,omitempty"`
	Population       []string  `json:"population"`
	Coverage         keyAgree  `json:"coverage"`
}
type benchHistory struct {
	RecordedScoreBasis string                `json:"recorded_score_basis"`
	Suite              string                `json:"suite"`
	Key                *benchKey             `json:"key"`
	Headline           benchTable            `json:"headline"`
	Changes            []string              `json:"headline_changes"`
	Runs               []*benchRun           `json:"runs"`
	Scores             map[string]benchScore `json:"scores"`
	Diffs              []benchConfigDiff     `json:"config_diffs"`
	Lanes              []benchLane           `json:"cost_time"`
	Quality            []benchQuality        `json:"quality"`
	Caveats            []string              `json:"caveats"`
	Subjects           []benchSubjectSummary `json:"subjects"`
}

func benchDiff(a, b *benchRun) benchConfigDiff {
	d := benchConfigDiff{From: a.ID, To: b.ID}
	if a.Subject != b.Subject {
		d.Changes = append(d.Changes, fmt.Sprintf("subject: %s -> %s", a.Subject, b.Subject))
	}
	if a.Itemset != b.Itemset {
		d.Changes = append(d.Changes, fmt.Sprintf("itemset: %s -> %s", a.Itemset, b.Itemset))
	}
	fields := func(r *benchRun) map[string]any {
		return map[string]any{"flags": r.Config.Flags, "roles": r.Config.Roles, "runner": r.Config.Runners, "dossier": r.Config.Dossier, "cache": r.Config.Cache, "ctx_tool": r.Config.CtxTool, "context_files": r.Config.ContextFiles, "verdicts": r.Config.Verdicts, "shadow": r.Config.Shadow, "budgets": r.Config.Budgets, "spec_variant": r.Inputs.SpecVariant, "spec_hash": r.Inputs.SpecHash, "orch_build": r.Build}
	}
	x, y := fields(a), fields(b)
	for _, k := range benchSortedKeys(x) {
		v, _ := json.Marshal(x[k])
		w, _ := json.Marshal(y[k])
		if string(v) != string(w) {
			d.Changes = append(d.Changes, fmt.Sprintf("%s: %s -> %s", k, v, w))
		}
	}
	d.Confounded = len(d.Changes) > 1
	return d
}
func benchHistoryBuild(suite string, s benchSuite, runs []*benchRun, base *benchRun, key *benchKey) (benchHistory, error) {
	h := benchHistory{Suite: suite, Key: key, Runs: runs, Scores: map[string]benchScore{}, Subjects: benchHistorySubjects(runs, key), RecordedScoreBasis: "Old runs[].scores are preserved historical originals; unversioned heuristic scores are legacy, not current behavior quality. New scores use " + benchScorerSchema + " / " + benchScorerProfile + "; read-only in-memory computations are in scores."}
	headlineRuns := []*benchRun{base}
	if last := runs[len(runs)-1]; last.ID != base.ID {
		headlineRuns = append(headlineRuns, last)
	}
	var e error
	h.Headline, e = benchComparison(suite, s, headlineRuns, base, key, false)
	if e != nil {
		return h, e
	}
	for _, row := range h.Headline.Rows {
		for _, change := range row.Changes {
			if strings.Contains(change, "about ") {
				h.Changes = append(h.Changes, row.Metric+": "+change+".")
			}
		}
	}
	lanes := map[string]*benchLane{}
	for i, r := range runs {
		if i > 0 {
			_, notes, refuse := compat(runs[i-1], r, key)
			if refuse && !s.crossSubject {
				return h, fail("incomparable consecutive subjects: %s", strings.Join(notes, "; "))
			}
			h.Caveats = append(h.Caveats, notes...)
			d := benchDiff(runs[i-1], r)
			h.Diffs = append(h.Diffs, d)
			if d.Confounded {
				h.Caveats = append(h.Caveats, r.Label+": confounded (multiple config changes)")
			}
		}
		h.Caveats = append(h.Caveats, r.Warnings...)
		if r.Subject != "" {
			if subj, e := benchSubjectLoad(r.Subject); e == nil {
				if subj.CommitBasis != "git" {
					h.Caveats = append(h.Caveats, r.Label+": commit basis "+subj.CommitBasis+"; "+subj.CommitEvidence)
				}
				if !objectHas("spec", subj.Spec.Hash) {
					h.Caveats = append(h.Caveats, r.Label+": spec text unknown")
				}
			}
		}
		if key != nil && key.Subject != r.Subject {
			h.Caveats = append(h.Caveats, r.Label+": key subject differs: "+key.Subject)
		}
		cost := benchRunCost(r)
		if cost.USD == nil || cost.Missing > 0 {
			h.Caveats = append(h.Caveats, r.Label+": missing cost; totals are incomplete")
		}
		if r.Inputs.Commit == "" || r.Config.Command == "" {
			h.Caveats = append(h.Caveats, r.Label+": original commit or exact command not recorded")
		}
		for name, g := range r.Groups {
			class := "other"
			if strings.HasPrefix(name, "group:") {
				class = "group passes"
			} else if strings.HasPrefix(name, "adversary") {
				class = "adversaries"
			} else if strings.Contains(name, "shadow") {
				class = "shadows"
			}
			runner := orDefault(g.Runner, "unknown")
			id := runner + "/" + class
			l := lanes[id]
			if l == nil {
				l = &benchLane{Runner: runner, PassClass: class}
				lanes[id] = l
			}
			if g.Seconds != nil {
				l.Seconds += *g.Seconds
			}
			if g.Cost.USD == nil {
				l.None++
			} else {
				l.Reported++
				l.USD += *g.Cost.USD
			}
			l.MissingRequests += g.Cost.Missing
		}
		if key != nil {
			score, e := benchRescore(r, key)
			if e != nil {
				return h, e
			}
			h.Scores[r.ID] = score
			if score.ItemNote != "" {
				h.Caveats = append(h.Caveats, r.Label+": "+score.ItemNote)
			}
			q := benchQuality{Run: r.ID, Cost: cost, DuplicateMatches: score.Classes["duplicate"].Match, VariationMatches: score.Classes["variation_site"].Match, Population: score.Population, Coverage: score.KeyAgree["group"]}
			q.Exclusion = benchRankingCompatibility(base, r)
			if score.Unavailable != "" {
				q.Exclusion = score.Unavailable
			}
			if q.Coverage.Items == 0 || q.Coverage.Supported != q.Coverage.Items {
				q.Exclusion = "incomplete supported-answer coverage"
			}
			if cost.USD == nil || cost.Missing != 0 {
				q.Exclusion = "unknown or incomplete cost coverage"
			}
			q.Eligible = q.Exclusion == ""
			if cost.USD != nil && cost.Missing == 0 && score.Unavailable == "" {
				if q.DuplicateMatches > 0 {
					v := *cost.USD / float64(q.DuplicateMatches)
					q.CostPerDuplicate = &v
				}
				if q.VariationMatches > 0 {
					v := *cost.USD / float64(q.VariationMatches)
					q.CostPerVariation = &v
				}
				q.Pareto = q.Eligible
			}
			h.Quality = append(h.Quality, q)
			for old := range r.Scores {
				if old != key.ID {
					h.Caveats = append(h.Caveats, r.Label+": recorded against older/different "+old+", report uses "+key.ID)
				}
			}
		}
	}
	for _, id := range benchSortedKeys(lanes) {
		h.Lanes = append(h.Lanes, *lanes[id])
	}
	for i, q := range h.Quality {
		if !q.Pareto {
			continue
		}
		peers := 0
		for j, p := range h.Quality {
			if i == j || !p.Eligible || strings.Join(p.Population, "\x00") != strings.Join(q.Population, "\x00") {
				continue
			}
			peers++
			if *p.Cost.USD <= *q.Cost.USD && p.DuplicateMatches >= q.DuplicateMatches && p.VariationMatches >= q.VariationMatches && (*p.Cost.USD < *q.Cost.USD || p.DuplicateMatches > q.DuplicateMatches || p.VariationMatches > q.VariationMatches) {
				h.Quality[i].Pareto = false
			}
		}
		if peers == 0 {
			h.Quality[i].Pareto = false
			h.Quality[i].Eligible = false
			h.Quality[i].Exclusion = "no compatible peer with complete cost and supported-answer coverage"
		}
	}
	if key != nil {
		for _, class := range []string{"duplicate", "variation_site", "none"} {
			if key.Counts[class] < 20 {
				h.Caveats = append(h.Caveats, fmt.Sprintf("%s: only %d key items (fewer than 20); small-sample warning", class, key.Counts[class]))
			}
		}
	} else {
		h.Caveats = append(h.Caveats, "No answer key recorded; quality is unknown")
	}
	return h, nil
}
func benchPrintHistory(w io.Writer, h benchHistory) {
	fmt.Fprintf(w, "# Benchmark history: %s\n\n%s\n\n", h.Suite, h.RecordedScoreBasis)
	if len(h.Subjects) > 0 {
		fmt.Fprintln(w, "## 0. Subjects")
		for _, s := range h.Subjects {
			fmt.Fprintf(w, "\n### Subject %s (runs: %s; keys: %s)\n\n%s\n\nGroups tested: %s. Item sets: %s. Spec text stored: %t.\n", s.ID, strings.Join(s.Runs, ", "), strings.Join(s.Keys, ", "), s.Description, strings.Join(s.Groups, ", "), strings.Join(s.Itemsets, ", "), s.TextStored)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, "## 1. Headline\n\n")
	benchPrintTable(w, h.Headline)
	for _, s := range h.Changes {
		fmt.Fprintln(w, "\n"+s)
	}
	fmt.Fprintln(w, "\n## 2. All runs, oldest first\n\n| ID | Label | Arm | Date | Build | Cost (basis) | Wall | Requests | Turns | Ctx tool | Duplicates found / precision / recall | Variation sites found / recall | Shadow agreement |\n| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, r := range h.Runs {
		cost := benchRunCost(r)
		dup, variation, shadow := "not recorded", "not recorded", "not recorded"
		if s, ok := h.Scores[r.ID]; ok {
			d, v := s.Classes["duplicate"], s.Classes["variation_site"]
			if s.Unavailable == "" {
				dup = fmt.Sprintf("%d/%d supported matches; precision %s; recall %s", d.Match, d.Total, benchPercent(d.Precision), benchPercent(d.Recall))
				variation = fmt.Sprintf("%d/%d supported matches; recall %s", v.Match, v.Total, benchPercent(v.Recall))
			} else {
				dup = "unavailable (see caveats)"
				variation = dup
			}
			var js []string
			for _, j := range benchSortedKeys(s.Judges) {
				x := s.Judges[j]
				js = append(js, j+" "+pct(x.Agree, x.Both))
			}
			if len(js) > 0 {
				shadow = strings.Join(js, ", ")
			}
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s (%s) | %s | %s | %s | %s | %s | %s | %s |\n", r.ID, r.Label, r.Arm, r.CreatedAt.Format(time.RFC3339), short(r.Build.Version, 12), benchCostText(cost), cost.Basis, benchGroupText(r, "wall", nil, true), benchGroupText(r, "requests", nil, true), benchGroupText(r, "turns", nil, true), orDefault(r.Config.CtxTool, "not recorded"), dup, variation, shadow)
	}
	for _, r := range h.Runs {
		fmt.Fprintln(w, "\n"+r.Label+": "+benchTestedOn(r))
		if h.Key != nil {
			fmt.Fprintf(w, "\nNew scores: %s / %s with %s (%d items; key subject %s); %s.\n", benchScorerSchema, benchScorerProfile, h.Key.ID, len(h.Key.Items), h.Key.Subject, h.Scores[r.ID].Coverage)
			for _, class := range []string{"duplicate", "variation_site", "none", "unknown"} {
				fmt.Fprintf(w, "Old recorded %s score (legacy, unversioned): %s.\n", class, benchOriginalScoreText(r, h.Key.ID, class))
			}
			score := h.Scores[r.ID]
			fmt.Fprintf(w, "\n%s. %s.\n", score.ItemNote, keyAgreeText(score.KeyAgree["group"]))
			for _, class := range []string{"duplicate", "variation_site"} {
				c, known := score.LegacyClasses[class]
				if !known {
					continue
				}
				fmt.Fprintf(w, "legacy %s location heuristic: %d/%d unique locations matched, precision %s, recall %s. Not behavior quality or ranking input.\n", class, c.Match, c.Total, benchPercent(c.Precision), benchPercent(c.Recall))
			}
		}
		if r.WallSeconds.Value != nil {
			fmt.Fprintf(w, "\nWall clock: %.0f s (basis: %s). Pass seconds, summed: %s.\n", *r.WallSeconds.Value, r.WallSeconds.Basis, benchGroupText(r, "wall", nil, true))
		}
	}
	if len(h.Diffs) > 0 {
		fmt.Fprint(w, "\n## 3. What changed\n\n")
		for _, d := range h.Diffs {
			fmt.Fprintf(w, "### %s -> %s\n\n", d.From, d.To)
			if d.Confounded {
				fmt.Fprint(w, "**Confounded**: more than one config field changed.\n\n")
			}
			if len(d.Changes) == 0 {
				fmt.Fprint(w, "No recorded config change.\n\n")
			}
			for _, c := range d.Changes {
				fmt.Fprintln(w, "- "+benchMD(c))
			}
		}
	}
	if len(h.Lanes) > 0 {
		fmt.Fprintln(w, "\n## 4. Cost and time by runner/lane/model and pass class\n\nRunner is the recorded harness/provider/model/effort string. Unknown lanes are not inferred.\n\n| Runner | Pass class | Known USD | Seconds | reported | none | Missing requests |\n| --- | --- | --- | --- | --- | --- | --- |")
		usd, seconds, reported, none, missing := 0.0, 0, 0, 0, 0
		for _, l := range h.Lanes {
			fmt.Fprintf(w, "| %s | %s | $%.4f | %d | %d | %d | %d |\n", benchMD(l.Runner), l.PassClass, l.USD, l.Seconds, l.Reported, l.None, l.MissingRequests)
			usd += l.USD
			seconds += l.Seconds
			reported += l.Reported
			none += l.None
			missing += l.MissingRequests
		}
		fmt.Fprintf(w, "| Total (known costs only) | all | $%.4f | %d | %d | %d | %d |\n", usd, seconds, reported, none, missing)
	}
	if len(h.Quality) > 0 {
		fmt.Fprintln(w, "\n## 5. Quality against cost\n\nPareto is restricted to identical subjects, item populations and pass/package coverage, complete supported-answer coverage, and known complete costs. No compatible peer means no ranking. Cost denominators are supported correct key behaviors, not unique locations.\n\n| Run | USD / true duplicate | USD / variation site | Pareto |\n| --- | --- | --- | --- |")
		for _, q := range h.Quality {
			money := func(p *float64) string {
				if p == nil {
					return "not recorded"
				}
				return fmt.Sprintf("$%.4f", *p)
			}
			ranking := fmt.Sprintf("%t", q.Pareto)
			if !q.Eligible {
				ranking = "not ranked: " + q.Exclusion
			}
			fmt.Fprintf(w, "| %s | %s | %s | %s |\n", q.Run, money(q.CostPerDuplicate), money(q.CostPerVariation), benchMD(ranking))
		}
	}
	shadowHeader := false
	for _, r := range h.Runs {
		score := h.Scores[r.ID]
		for _, name := range benchSortedKeys(score.Judges) {
			j := score.Judges[name]
			if !shadowHeader {
				fmt.Fprint(w, "\n## 6. Shadow judges\n\nConfusion: actual class -> predicted class -> count. Costs are known subtotals only.\n\n")
				shadowHeader = true
			}
			conf, _ := json.Marshal(j.Confusion)
			per := "not recorded"
			secs := "not recorded"
			if j.Items > 0 {
				secs = fmt.Sprintf("%.2f s/item", j.Seconds/float64(j.Items))
				if j.Cost.USD != nil && j.Cost.Missing == 0 {
					per = fmt.Sprintf("$%.6f/item", *j.Cost.USD/float64(j.Items))
				}
			}
			fmt.Fprintf(w, "- %s / %s: %s; agreement %s; %s; %s; %s\n", r.Label, name, conf, pct(j.Agree, j.Both), benchCostText(j.Cost), per, secs)
		}
	}
	fmt.Fprint(w, "\n## 7. Reading guide and caveats\n\n")
	if h.Key != nil {
		fmt.Fprintf(w, "Key: %s, %d items; duplicate %d, variation_site %d, none %d, unknown %d.\n\n", h.Key.ID, len(h.Key.Items), h.Key.Counts["duplicate"], h.Key.Counts["variation_site"], h.Key.Counts["none"], h.Key.Counts["unknown"])
	}
	fmt.Fprintln(w, "Pass wall times are not end-to-end elapsed time. Total wall is the sum of selected pass seconds; total context is the maximum, not a sum. Quality scores count keyed behaviors; multiple behaviors may share evidence. Precision conditions on supported predicted behaviors; recall includes all key behaviors of the class. Legacy scores alone count unique locations. Conditional accuracy excludes missing, invalid and unsupported answers, which remain in answer-coverage denominators. Group comparison scope is printed above; all-run history and lane totals include every recorded pass. Unknown values are never treated as zero.")
	for _, c := range h.Caveats {
		fmt.Fprintln(w, "- "+c)
	}
	fmt.Fprint(w, "\n## 8. Reproduce\n\n")
	for _, r := range h.Runs {
		fmt.Fprintln(w, benchReproduce(r)+"\n")
		arm, _ := json.MarshalIndent(r.ArmDefinition, "", "  ")
		fmt.Fprintf(w, "### %s (%s)\n\nArm `%s`:\n\n```json\n%s\n```\n\nCommand: `%s`\n\nRepo: `%s`, commit: `%s`, spec: `%s`, hash: `%s`, variant: `%s`.\n\n", r.ID, r.Label, r.Arm, arm, orDefault(r.Config.Command, "not recorded"), r.Inputs.Repo, orDefault(r.Inputs.Commit, "not recorded"), r.Inputs.SpecPath, r.Inputs.SpecHash, orDefault(r.Inputs.SpecVariant, "not recorded"))
	}
}
func cmdBenchReport(a *app, command string, args []string) error {
	fs := a.flags("bench " + command)
	baseline := fs.String("baseline", "", "baseline run id or label")
	keyN := fs.String("key", "", "key number (default newest)")
	md := fs.Bool("md", false, "paste-ready Markdown (default)")
	js := fs.Bool("json", false, "JSON output")
	total := &benchTotalFlag{}
	fs.Var(total, "total", "sum common groups; --total=all explicitly includes differing group sets")
	cross := fs.Bool("cross-subject", false, "allow different repo or spec paths with warnings")
	runList := fs.String("runs", "", "report run ids or labels, comma-separated")
	since := fs.String("since", "", "include runs since YYYY-MM-DD")
	save := fs.Bool("save", false, "save Markdown history and append reports/index.md")
	pos, e := parseArgs(fs, args)
	if e != nil {
		return e
	}
	if command == "compare" {
		if e := benchOnlyFlags(fs, "baseline", "key", "md", "json", "total", "cross-subject"); e != nil {
			return e
		}
	} else {
		if e := benchOnlyFlags(fs, "baseline", "key", "md", "json", "runs", "since", "save", "cross-subject"); e != nil {
			return e
		}
	}
	if *md && *js {
		return fail("choose --md or --json, not both")
	}
	if len(pos) == 0 {
		return fail("usage: bench %s <suite> [runs...]", command)
	}
	if command == "compare" && len(pos) < 3 {
		return fail("compare requires a suite and at least two runs")
	}
	if command == "report" && len(pos) != 1 {
		return fail("report takes one suite; use --runs a,b,c")
	}
	if command == "compare" && (*save || *since != "" || *runList != "") {
		return fail("--save, --since and --runs belong to bench report")
	}
	suite := pos[0]
	s, e := benchSuiteLoad(suite)
	if e != nil {
		return e
	}
	s.crossSubject, s.totalAll = *cross, total.all
	all, e := benchRuns(suite)
	if e != nil {
		return e
	}
	if len(all) == 0 {
		return fail("suite %s has no runs", suite)
	}
	base := all[0]
	if *baseline != "" {
		base, e = benchRunLoad(suite, *baseline)
		if e != nil {
			return e
		}
	}
	runs := all
	if command == "compare" {
		runs = nil
		for _, name := range pos[1:] {
			r, e := benchRunLoad(suite, name)
			if e != nil {
				return e
			}
			runs = append(runs, r)
		}
	} else {
		if *runList != "" {
			runs = nil
			for _, name := range strings.Split(*runList, ",") {
				r, e := benchRunLoad(suite, name)
				if e != nil {
					return e
				}
				runs = append(runs, r)
			}
			sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.Before(runs[j].CreatedAt) })
		}
		if *since != "" {
			date, e := time.Parse("2006-01-02", *since)
			if e != nil {
				return fail("--since requires YYYY-MM-DD")
			}
			filtered := []*benchRun{}
			for _, r := range runs {
				if !r.CreatedAt.Before(date) {
					filtered = append(filtered, r)
				}
			}
			runs = filtered
		}
	}
	if len(runs) == 0 {
		return fail("no runs match report filters")
	}
	key, e := benchKeyLoad(suite, *keyN)
	if e != nil {
		keys, keyErr := benchKeys(suite)
		if keyErr != nil {
			return keyErr
		}
		if *keyN != "" || len(keys) > 0 {
			return e
		}
		key = nil
	}
	if command == "compare" {
		t, e := benchComparison(suite, s, runs, base, key, total.total)
		if e != nil {
			return e
		}
		if *js {
			return json.NewEncoder(a.out).Encode(t)
		}
		benchPrintTable(a.out, t)
		return nil
	}
	h, e := benchHistoryBuild(suite, s, runs, base, key)
	if e != nil {
		return e
	}
	if *save {
		if e := benchSaveReport(suite, h); e != nil {
			return e
		}
	}
	if *js {
		return json.NewEncoder(a.out).Encode(h)
	}
	benchPrintHistory(a.out, h)
	return nil
}

// A negative internal count means unmeasured, never a negative observation.
func benchShellCount(p sessionProfile, count int) int {
	if p.Shell == nil && (p.Calls == nil || p.Calls["bash"] > 0) {
		return -1
	}
	return count
}

// benchToolRows count what the agents called, from the exploration profile of
// each group: the raw shell against the native read tool.
var benchToolRows = []struct {
	name  string
	count func(sessionProfile) int
}{
	{"Raw shell search (grep, rg, find, ls)", func(p sessionProfile) int { return benchShellCount(p, p.Shell["shell search"]) }},
	{"Raw shell read (sed, cat, head, tail)", func(p sessionProfile) int { return benchShellCount(p, p.Shell["shell read"]) }},
	{"Other shell calls", func(p sessionProfile) int { return benchShellCount(p, p.Shell["shell other"]) }},
	{"Native reads", func(p sessionProfile) int {
		if n, known := p.totalReads(); known {
			return n
		}
		return -1
	}},
	{"Native reads with a range", func(p sessionProfile) int {
		if p.rangedReadsKnown() {
			return p.RangedReads
		}
		return -1
	}},
}

// benchToolSum adds a count over the selected groups; it is nil when a group
// has no exploration profile, or one recorded before shell calls were split.
func benchToolSum(r *benchRun, groups []string, count func(sessionProfile) int) *float64 {
	names := benchSelectedGroups(r, groups)
	if len(names) == 0 {
		return nil
	}
	sum := 0.0
	for _, n := range names {
		e := r.Groups[n].Exploration
		if e == nil {
			return nil
		}
		// Native total reads can predate shell/range instrumentation. Do not
		// discard that actual counter, or infer a missing range breakdown.
		v := count(*e)
		if v < 0 {
			return nil
		}
		sum += float64(v)
	}
	return &sum
}
