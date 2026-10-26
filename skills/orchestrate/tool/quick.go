package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// `orch quick` is the short path for a small, clear change: no spec file, no
// gate, no plan. It is allowed only where a failing test can catch a wrong
// answer, so the repo check carries the safety that the gate gives a spec.
// The structural limits below always apply. The triage verdict is the one
// part that follows quick_mode: shadow records it and warns, enforce refuses.
const (
	quickMaxOwns    = 3
	quickMinRequest = 20
	quickMaxRequest = 800

	quickClear      = 0.9 // P(clear) at least
	quickDifficulty = 0.6 // expected difficulty at most
	quickVerifiable = 0.8
	quickCross      = 0.3 // P(cross-cutting) below
	quickCauseKnown = 0.5
)

// quickFit says whether the decision model's answers describe a job small
// enough for the short path. Unlike tierFor it does not move the tier up for
// low confidence: the structural limits already bound the job.
func quickFit(a triageAnswers) (bool, string) {
	var misses []string
	if v := a.ClarityP["clear"]; v < quickClear {
		misses = append(misses, fmt.Sprintf("clear %.2f < %.2f", v, quickClear))
	}
	if a.Difficulty > quickDifficulty {
		misses = append(misses, fmt.Sprintf("difficulty %.2f > %.2f", a.Difficulty, quickDifficulty))
	}
	if a.Verifiable < quickVerifiable {
		misses = append(misses, fmt.Sprintf("verifiable %.2f < %.2f", a.Verifiable, quickVerifiable))
	}
	if a.CrossCutting >= quickCross {
		misses = append(misses, fmt.Sprintf("cross-cutting %.2f >= %.2f", a.CrossCutting, quickCross))
	}
	if a.CauseKnown < quickCauseKnown {
		misses = append(misses, fmt.Sprintf("cause known %.2f < %.2f", a.CauseKnown, quickCauseKnown))
	}
	if len(misses) > 0 {
		return false, strings.Join(misses, ", ")
	}
	return true, fmt.Sprintf("clear %.2f, difficulty %.2f, verifiable %.2f", a.ClarityP["clear"], a.Difficulty, a.Verifiable)
}

// quickLimits checks what code can check before any model is asked.
func quickLimits(cfg config, request string, owns []string) error {
	switch {
	case cfg.Check == "":
		return fail("quick needs a `check` command in the repo config: without a test that can fail, nothing catches a wrong answer. Use the full path (orch new, then spawn)")
	case len(owns) == 0 || len(owns) > quickMaxOwns:
		return fail("--owns must name 1 to %d files; a bigger change needs the full path", quickMaxOwns)
	case len([]rune(request)) < quickMinRequest:
		return fail("the request is under %d characters: say what to change and what must be true afterwards", quickMinRequest)
	case len([]rune(request)) > quickMaxRequest:
		return fail("the request is over %d characters, which is a spec, not a quick job. Write a spec and use orch plan", quickMaxRequest)
	}
	for _, o := range owns {
		if filepath.IsAbs(o) || strings.Contains(o, "..") || strings.ContainsAny(o, "*?[") || strings.HasSuffix(o, "/") {
			return fail("--owns %q must be a plain relative file path, not a directory, glob or parent path", o)
		}
	}
	return nil
}

func cmdQuick(a *app, args []string) error {
	fs := a.flags("quick")
	ownsFlag := fs.String("owns", "", "comma-separated files the lead may change (1 to 3)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return &exitError{code: 2, msg: "usage: orch quick <id> --owns <file>[,<file>] <request...>"}
	}
	id, request := pos[0], strings.TrimSpace(strings.Join(pos[1:], " "))
	owns := splitList(*ownsFlag)
	s, err := a.site()
	if err != nil {
		return err
	}
	if s.cfg.QuickMode == "off" {
		return fail("quick is off (quick_mode in %s)", userConfigPath())
	}
	if why := spawnBlocked(s.root); why != "" {
		return fail("%s", why)
	}
	if err := quickLimits(s.cfg, request, owns); err != nil {
		return err
	}
	// ownedPath lets a directory own everything under it, so a directory would
	// slip past the file limit.
	for _, o := range owns {
		if out, _ := git(s.repo, "ls-tree", s.cfg.BaseBranch, "--", o); strings.Contains(out, " tree ") {
			return fail("--owns %q is a directory; a quick job owns plain files, so name the file", o)
		}
	}
	var rule *dispatchRule
	for i := range s.cfg.Dispatch {
		if s.cfg.Dispatch[i].Tier == "settled" {
			rule = &s.cfg.Dispatch[i]
			break
		}
	}
	if rule == nil {
		return fail("quick runs on the settled tier, and %s has no dispatch rule with tier = \"settled\"", userConfigPath())
	}
	c := a.chooseProfile(s, "settled", "quick", rule, false, false)
	if c.profile == nil {
		for _, sk := range c.skipped {
			fmt.Fprintf(a.out, "skipped %s\n", sk)
		}
		return fail("no usable profile for settled work right now (see orch health, orch dispatch). Park the job or use the full path")
	}

	check := s.cfg.Check
	path, err := newTask(s, taskOpts{noCtx: true, id: id, kind: "ship", harness: c.profile.Harness, model: c.profile.Model, effort: c.effort, owns: owns,
		fill: map[string]string{
			"Intent":     request,
			"Scope":      "In: " + strings.Join(owns, ", ") + "\nOut: every other file. This is a quick job: it owns only the files above.",
			"Pointers":   "Start with the owned files. Nothing else is pre-read.",
			"Spec":       request,
			"Acceptance": fmt.Sprintf("`%s` passes. If the owned files have no test for the change, say so in the done line instead of adding a test file you do not own.", check),
			"Authority": "May decide: names and structure inside the owned files\n" +
				"Must escalate: any doubt about what the request means; a need to change a file you do not own, a function signature, or a test; a first attempt that does not pass the check (this is a one-attempt job: do not iterate, escalate)\n" +
				"Never: push, merge, edit outside the owned files, change the check command",
		}})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := setFrontMatter(path, "quick", "true"); err != nil {
		cleanup()
		return err
	}

	// Triage rates the request the same way it rates a brief. The verdict is
	// logged under the `quick` tier so the report can say whether it was right.
	verdict := "skipped: decider off"
	if reason := s.deciderOff(a.now()); reason != "" {
		a.recordTriageSkip(s, id, reason)
		if s.cfg.QuickMode == "enforce" {
			cleanup()
			return &triageOff{reason: reason, id: id}
		}
	} else {
		rec, terr := a.triageTask(s, id, false)
		if terr != nil {
			a.recordTriageSkip(s, id, terr.Error())
		}
		switch {
		case terr != nil && s.cfg.QuickMode == "enforce":
			cleanup()
			if off, ok := terr.(*triageOff); ok {
				return off
			}
			return fail("triage is unavailable (%v), so quick cannot be checked. Retry, or use the full path", terr)
		case terr != nil:
			verdict = "skipped: decider off"
			fmt.Fprintf(a.out, "warning: triage unavailable: %v\n", terr)
		default:
			ok, why := quickFit(rec.Answers)
			tier := "quick"
			if !ok {
				tier = "not-quick"
			}
			moved := *rec
			moved.At, moved.Tier, moved.Reason, moved.Actual = a.now().Unix(), tier, why, c.profile.Harness+" "+c.profile.Model
			_ = appendTriage(s.root, moved)
			_ = setFrontMatter(path, "tier", tier)
			verdict = tier + " (" + why + ")"
			if !ok {
				if s.cfg.QuickMode == "enforce" {
					cleanup()
					return fail("triage says this is not a quick job (%s). Write a spec and use orch plan, or ask the user", why)
				}
				fmt.Fprintf(a.out, "warning: triage says this is not a quick job (%s); quick_mode is shadow, so it runs anyway\n", why)
			}
		}
	}
	_ = a.appendStatus(dir, "note", fmt.Sprintf("quick: %s; %s %s; triage %s", short(request, 80), c.profile.Harness, c.profile.Model, verdict), "", "orch")
	fmt.Fprintf(a.out, "quick: %s on %s %s (effort %s), owns %s\n", id, c.profile.Harness, c.profile.Model, c.effort, strings.Join(owns, ", "))
	if err := a.spawn(id, ""); err != nil {
		return err
	}
	return nil
}

// isQuick reports whether a task was made by `orch quick`; the first status
// line says so.
func isQuick(entries []entry) bool {
	for _, e := range entries {
		if e.By == "orch" && strings.HasPrefix(e.Text, "quick:") {
			return true
		}
	}
	return false
}

// quickVerify runs when a quick lead reports done. Nobody asked for it: the
// short path has no gate, so verification is the gate.
func (a *app) quickVerify(id string) string {
	va := *a
	var buf bytes.Buffer
	va.out = &buf
	err := cmdVerify(&va, []string{id})
	// Keep the full first line, including a preliminary pass's "(not integrated)".
	first, _, _ := strings.Cut(strings.TrimSpace(buf.String()), "\n")
	if first == "" && err != nil {
		first = "verify: " + err.Error()
	}
	return first
}

func fmQuick(dir string) bool {
	fm, _, err := readTask(filepath.Join(dir, "task.md"))
	return err == nil && fm["quick"] == "true"
}
