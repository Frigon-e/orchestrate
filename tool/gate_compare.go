package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// orch gate compare <spec> <experiment-a> <experiment-b> sets two experiment
// runs (orch gate <spec> --only ... --experiment <name>) side by side: what
// they cost and how much they found, and how far their findings and
// questions agree.

func cmdGateCompare(a *app, args []string) error {
	fs := a.flags("gate")
	refPath := fs.String("reference", "", "JSON reference [{id,revision,package,item,verdict,candidate,candidates}]; cross-version identities require adjacent mapping.json")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 3, "gate compare <spec> <experiment-a> <experiment-b> [--reference <file.json>]"); err != nil {
		return err
	}
	gs, err := loadGateSpec(a, pos[0])
	if err != nil {
		return err
	}
	var recs [2]*gateRecord
	var dirs [2]string
	for i, name := range pos[1:] {
		dirs[i] = filepath.Join(gateDir(gs.s.root, gs.hash), "experiments", name)
		b, err := os.ReadFile(filepath.Join(dirs[i], "gate.json"))
		if err != nil {
			return fail("experiment %s: no record at %s (run `orch gate <spec> --only <group-key> --experiment %s` first)", name, dirs[i], name)
		}
		recs[i] = &gateRecord{}
		if err := json.Unmarshal(b, recs[i]); err != nil {
			return fail("experiment %s: %v", name, err)
		}
	}
	for i, name := range pos[1:] {
		describeExperiment(a.out, string(rune('A'+i)), name, recs[i], dirs[i])
	}
	fa, fb := claims(recs[0].Findings), claims(recs[1].Findings)
	printOverlap(a.out, "findings", pos[1], pos[2], fa, fb)
	var qa, qb []string
	for _, q := range recs[0].Questions {
		qa = append(qa, q.Question)
	}
	for _, q := range recs[1].Questions {
		qb = append(qb, q.Question)
	}
	printOverlap(a.out, "questions", pos[1], pos[2], qa, qb)
	gdir := gateDir(gs.s.root, gs.hash)
	arms := []*armData{loadArm(gdir, pos[1], dirs[0], recs[0]), loadArm(gdir, pos[2], dirs[1], recs[1])}
	for i, r := range recs {
		if cl := r.coverageLine(); cl != "" {
			fmt.Fprintf(a.out, "coverage %s: %s\n", string(rune('A'+i)), cl)
		}
	}
	var ref map[string]refEntry
	if *refPath != "" {
		entries, err := loadReference(*refPath)
		if err != nil {
			return fail("%v", err)
		}
		data, err := os.ReadFile(expandHome(*refPath))
		if err != nil {
			return err
		}
		for i, arm := range arms {
			set := benchMakeItemset("gate-compare", "", arm.items)
			mapping, mappingErr := readItemMapping(filepath.Join(filepath.Dir(expandHome(*refPath)), "mapping.json"), filepath.Base(*refPath), benchHash(data), set.ID)
			arm.reference = resolveMappedReference(entries, arm.items, benchHash(data), set.ID, mapping)
			if mappingErr != nil {
				for id, r := range arm.reference {
					r.Issue = mappingErr.Error()
					arm.reference[id] = r
				}
			}
			issues := referenceIssues(arm.reference)
			fmt.Fprintf(a.out, "reference %s: %d entries, %d mapped to items; %d unmapped/ambiguous\n", string(rune('A'+i)), len(entries), len(arm.reference)-len(issues), len(issues))
		}
		ref = arms[0].reference
	}
	printItemComparison(a.out, arms, ref, *refPath != "")
	return nil
}

func claims(fs []gateFinding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Claim)
	}
	return out
}

// legacyCtxProfile names the ctx profile an old record ran with. The switch is
// gone, so a new record has none and shows nothing.
func legacyCtxProfile(rec *gateRecord) string {
	if rec.CtxProfile == "" || rec.CtxProfile == "default" {
		return ""
	}
	return "; ctx_profile: " + rec.CtxProfile
}

// legacyContextFiles names the context-files setting an old record ran with.
// The switch is gone, so a new record has none and shows nothing.
func legacyContextFiles(rec *gateRecord) string {
	if rec.ContextFiles == "" {
		return ""
	}
	return "; context_files: " + rec.ContextFiles
}

// legacyCache names the cache retention an old record asked for.
func legacyCache(rec *gateRecord) string {
	if rec.Cache == "" {
		return ""
	}
	return "cache " + rec.Cache + ", "
}

func describeExperiment(w io.Writer, label, name string, rec *gateRecord, dir string) {
	fmt.Fprintf(w, "experiment %s: %s (%sgroups %s)\n", label, name, legacyCache(rec), strings.Join(rec.Only, ", "))
	fmt.Fprintf(w, "  ctx_tool: %s; ctx_shell: %s%s%s\n", orDefault(rec.CtxTool, "off (legacy)"), orDefault(rec.CtxShell, "open"), legacyContextFiles(rec), legacyCtxProfile(rec))
	if warning := treeWarning(rec); warning != "" {
		fmt.Fprintf(w, "  %s\n", warning)
	}
	t, widest, hits := rec.totals()
	share := 0
	if all := t.Input + t.CacheRead; all > 0 {
		share = t.CacheRead * 100 / all
	}
	fmt.Fprintf(w, "  requests %d; tokens: input %d, cache read %d (%d%% of input), cache write %d, output %d; max context %d in %s\n",
		t.Requests, t.Input, t.CacheRead, share, t.CacheWrite, t.Output, t.MaxContext, orDefault(widest, "-"))
	fmt.Fprintf(w, "  %s; parts: input $%.4f, output $%.4f, cache read $%.4f, cache write $%.4f\n", costNote(t.Cost, t.NoCost),
		t.CostParts.Input, t.CostParts.Output, t.CostParts.CacheRead, t.CostParts.CacheWrite)
	secs := 0
	var names []string
	for n, s := range rec.PassSeconds {
		secs += s
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "  seconds: %d over %d pass(es); turns %d; budget hits: soft %d, hard %d\n", secs, len(names), t.Turns, hits["soft"], hits["hard"])
	bySource := map[string]int{}
	verified := 0
	for _, f := range rec.Findings {
		bySource[f.Source]++
		if f.Verified {
			verified++
		}
	}
	var srcs []string
	for s, n := range bySource {
		srcs = append(srcs, fmt.Sprintf("%s %d", orDefault(s, "?"), n))
	}
	sort.Strings(srcs)
	blocking := 0
	for _, q := range rec.Questions {
		if q.blocks() {
			blocking++
		}
	}
	fmt.Fprintf(w, "  findings: %d (%d verified; by source: %s); questions: %d (%d need a decision)\n", len(rec.Findings), verified, orNone(srcs), len(rec.Questions), blocking)
	if rec.Checklist == "plain" {
		fmt.Fprintln(w, "  reuse checklist: plain (spec text only; no lookups, no candidates)")
	}
	for _, n := range names {
		p := rec.PassTokens[n]
		fmt.Fprintf(w, "  pass %s: runner %s; %ds; input %d, cache read %d, cache write %d, output %d; %d requests, %d turns, max context %d; %s", n, orDefault(rec.PassRunners[n], "unknown (legacy)"), rec.PassSeconds[n],
			p.Input, p.CacheRead, p.CacheWrite, p.Output, p.Requests, p.Turns, p.MaxContext, costNote(p.Cost, p.NoCost))
		if p.BudgetHit != "" {
			fmt.Fprintf(w, "; budget hit: %s", p.BudgetHit)
		}
		fmt.Fprintln(w)
		if prof, ok := profileSession(filepath.Join(dir, "sessions", passFile(n)), rec.SpecRel); ok {
			fmt.Fprintf(w, "    exploration: %s\n", prof)
		}
	}
	for _, n := range rec.Notes {
		fmt.Fprintf(w, "  note: %s\n", agentScalar(n))
	}
	for n, e := range rec.PassErrors {
		fmt.Fprintf(w, "  pass_failed: %s - %s\n", n, agentScalar(e))
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// matchItems pairs each item of a with the most alike unpaired item of b; two
// items are alike when at least 60% of their words agree (Jaccard on the
// words of the normalised text). It returns the pairs' count and the
// leftovers of each side.
func matchItems(a, b []string) (matched int, onlyA, onlyB []string) {
	ta := make([]map[string]bool, len(a))
	for i, s := range a {
		ta[i] = questionTokens(s)
	}
	tb := make([]map[string]bool, len(b))
	for i, s := range b {
		tb[i] = questionTokens(s)
	}
	used := make([]bool, len(b))
	for i := range a {
		best, bestJ := -1, 0.6
		for j := range b {
			if used[j] {
				continue
			}
			sim := jaccard(ta[i], tb[j])
			if sim == 0 && strings.EqualFold(flat(a[i]), flat(b[j])) {
				sim = 1
			}
			if sim >= bestJ {
				best, bestJ = j, sim
			}
		}
		if best < 0 {
			onlyA = append(onlyA, a[i])
			continue
		}
		used[best] = true
		matched++
	}
	for j := range b {
		if !used[j] {
			onlyB = append(onlyB, b[j])
		}
	}
	return matched, onlyA, onlyB
}

func printOverlap(w io.Writer, what, nameA, nameB string, a, b []string) {
	matched, onlyA, onlyB := matchItems(a, b)
	union := len(a) + len(b) - matched
	jac := 1.0
	if union > 0 {
		jac = float64(matched) / float64(union)
	}
	fmt.Fprintf(w, "legacy descriptive overlap of %s: %d matched, %d in %s, %d in %s, Jaccard %.2f (items match when 60%% of their words agree)\n", what, matched, len(a), nameA, len(b), nameB, jac)
	for _, s := range onlyA {
		fmt.Fprintf(w, "  only in %s: %s\n", nameA, short(flat(s), 80))
	}
	for _, s := range onlyB {
		fmt.Fprintf(w, "  only in %s: %s\n", nameB, short(flat(s), 80))
	}
}
