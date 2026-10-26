package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// `orch spawn <id> --auto` replaces the orchestrator's choice of harness,
// model and effort with a lookup: triage rates the finished brief, the first
// dispatch rule with that tier supplies the profiles, and the first profile
// that is healthy, allowed and not a degraded stand-in wins. Without --apply
// it only says what it would do, until the triage report earns more trust.
var tierEffort = map[string]string{"settled": "low", "standard": "high", "planning": "high"}

type autoChoice struct {
	tier, reason, rule string
	profile            *profile
	effort             string
	skipped            []string
	parkedWhy          string
	degradedOption     *profile
}

// autoPick resolves a task made with --harness auto. The task keeps a hand
// chosen profile otherwise: auto applies only when it was asked for. It
// records unavailable triage attempts; cached says a successful record is an
// earlier one that needs no write.
func (a *app) autoPick(s site, id string, allowDegraded bool, tierOverride ...string) (c *autoChoice, rec *triageRecord, cached bool, err error) {
	dir, err := taskDir(s.root, id)
	if err != nil {
		return nil, nil, false, err
	}
	fm, _, err := readTask(filepath.Join(dir, "task.md"))
	if err != nil {
		return nil, nil, false, fail("no task %s", id)
	}
	if fm["harness"] != "auto" {
		return nil, nil, false, fail("%s has a hand-chosen profile (%s %s). --auto applies only to tasks made with --harness auto --model auto --effort auto", id, fm["harness"], fm["model"])
	}
	if len(tierOverride) > 0 && tierOverride[0] != "" {
		_, hash, e := triageState(dir, s.cfg)
		if e != nil {
			return nil, nil, false, e
		}
		rec = &triageRecord{ID: id, TaskHash: hash, Tier: tierOverride[0], Reason: "user-selected tier", Mode: "manual"}
	} else {
		if reason := s.deciderOff(a.now()); reason != "" {
			a.recordTriageSkip(s, id, reason)
			return nil, nil, false, &triageOff{reason: reason, id: id}
		}
		rec, cached, err = a.triageScore(s, id, false)
		if err != nil {
			if off, ok := err.(*triageOff); ok {
				a.recordTriageSkip(s, id, off.reason)
				return nil, nil, false, off
			}
			return nil, nil, false, fail("cannot triage %s: %v", id, err)
		}
	}
	var rule *dispatchRule
	for i := range s.cfg.Dispatch {
		if s.cfg.Dispatch[i].Tier == rec.Tier {
			rule = &s.cfg.Dispatch[i]
			break
		}
	}
	if rule == nil {
		return nil, nil, false, fail("triage rated %s as %s, but no dispatch rule has tier = %q (add it in %s)", id, rec.Tier, rec.Tier, userConfigPath())
	}
	c = a.chooseProfile(s, rec.Tier, rec.Reason, rule, fm["private"] == "true", allowDegraded)
	return c, rec, cached, nil
}

func (a *app) spawnChosen(id, harness, model, effort string) error {
	s, err := a.site()
	if err != nil {
		return err
	}
	dir, err := taskDir(s.root, id)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "task.md")
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fm, _, err := readTask(path)
	if err != nil {
		return err
	}
	if effort == "" {
		effort = fm["effort"]
		if effort == "" || effort == "auto" {
			effort = "high"
		}
	}
	if _, _, _, _, err = a.preflight(id, &spawnOverride{harness: harness, model: model, effort: effort}); err != nil {
		return err
	}
	for key, value := range map[string]string{"harness": harness, "model": model, "effort": effort} {
		if err := setFrontMatter(path, key, value); err != nil {
			_ = os.WriteFile(path, original, 0o644)
			return err
		}
	}
	old := a.skipTriage
	a.skipTriage = true
	defer func() { a.skipTriage = old }()
	if err := a.spawn(id, ""); err != nil {
		if l, _ := loadLead(dir); l == nil || l.ClosedAt != 0 {
			_ = os.WriteFile(path, original, 0o644)
		}
		return err
	}
	return nil
}

// chooseProfile walks one dispatch rule and takes the first profile that is
// healthy, allowed and not a degraded stand-in the user has not approved.
func (a *app) chooseProfile(s site, tier, reason string, rule *dispatchRule, private, allowDegraded bool) *autoChoice {
	c := &autoChoice{tier: tier, reason: reason, rule: rule.When}
	limited := loadHealth(a.now())
	for i := range rule.Use {
		p := &rule.Use[i]
		parent, _, down := limitedBy(limited, p.lane())
		switch {
		case private && p.Harness != "claude":
			c.skipped = append(c.skipped, p.String()+": the task is private, so Claude only")
		case down:
			c.skipped = append(c.skipped, p.String()+": "+parent+" is limited")
		case s.cfg.modelAllowed(p.Harness, p.Model, a.now()) != nil:
			c.skipped = append(c.skipped, p.String()+": not allowed right now ("+p.Model+" is an emergency stand-in)")
		case p.Degraded && !allowDegraded:
			c.skipped = append(c.skipped, p.String()+": degraded fallback, needs the user's approval (--degraded)")
			if c.degradedOption == nil {
				c.degradedOption = p
			}
		case c.profile == nil:
			c.profile = p
		}
	}
	if c.profile == nil {
		c.parkedWhy = "no usable profile in the rule"
		return c
	}
	c.effort = c.profile.Effort
	if c.effort == "" {
		c.effort = tierEffort[c.tier]
	}
	return c
}

func (a *app) spawnAuto(id string, apply, allowDegraded bool, tierOverride ...string) error {
	s, err := a.site()
	if err != nil {
		return err
	}
	if len(tierOverride) > 0 && tierOverride[0] != "" {
		old := a.skipTriage
		a.skipTriage = true
		defer func() { a.skipTriage = old }()
	}
	c, rec, cached, err := a.autoPick(s, id, allowDegraded, tierOverride...)
	if err != nil {
		if off, ok := err.(*triageOff); ok {
			if apply {
				a.recordTriageSkip(s, id, off.reason)
			}
			fmt.Fprintln(a.out, off)
			return nil
		}
		return err
	}
	fmt.Fprintf(a.out, "auto: %s is %s work (%s)\n  rule: %s\n", id, c.tier, rec.Reason, c.rule)
	for _, sk := range c.skipped {
		fmt.Fprintf(a.out, "  skipped %s\n", sk)
	}
	if c.profile == nil {
		fmt.Fprintf(a.out, "  parked: %s\n", c.parkedWhy)
		if c.degradedOption != nil {
			fmt.Fprintf(a.out, "  degraded option: %s; ask the user, then rerun with --degraded\n", c.degradedOption)
		}
		return &exitError{code: 1}
	}
	pick := fmt.Sprintf("%s %s (effort %s)", c.profile.Harness, c.profile.Model, c.effort)
	if !apply {
		fmt.Fprintf(a.out, "  would use: %s\ndry run: nothing changed. Add --apply to set the profile and spawn\n", pick)
		return nil
	}
	// Refuse on everything but a free slot and the triage gate before the task
	// is touched, so a refusal leaves it as an auto task that can be retried.
	if _, _, _, _, err := a.preflight(id, &spawnOverride{harness: c.profile.Harness, model: c.profile.Model, effort: c.effort, degraded: c.profile.Degraded}); err != nil {
		return err
	}
	dir, _ := taskDir(s.root, id)
	path := filepath.Join(dir, "task.md")
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	undo := func() { _ = os.WriteFile(path, orig, 0o644) }
	for k, v := range map[string]string{"harness": c.profile.Harness, "model": c.profile.Model, "effort": c.effort} {
		if err := setFrontMatter(path, k, v); err != nil {
			undo()
			return err
		}
	}
	if c.profile.Degraded {
		if err := setFrontMatter(path, "degraded", "true"); err != nil {
			undo()
			return err
		}
	}
	_ = a.appendStatus(dir, "note", fmt.Sprintf("auto: %s -> %s (%s)", c.tier, strings.TrimSpace(pick), rec.Reason), "", "orch")
	// The record is written once, with the profile it really runs on, so the
	// triage report compares like with like; it also lets the spawn below
	// find the rating instead of asking the model again.
	moved := *rec
	moved.At, moved.Actual = a.now().Unix(), c.profile.Harness+" "+c.profile.Model
	if cached {
		_ = appendTriage(s.root, moved)
	} else if err := a.persistTriage(s, id, &moved); err != nil {
		undo()
		return err
	}
	fmt.Fprintf(a.out, "  using: %s\n", pick)
	if err := a.spawn(id, ""); err != nil {
		// A spawn that left a lead behind runs on the new profile; otherwise
		// put the task back to auto so the next --auto --apply can run.
		if l, _ := loadLead(dir); l == nil || l.ClosedAt != 0 {
			undo()
			if rec.Tier != "" {
				_ = setFrontMatter(path, "tier", rec.Tier)
			}
		}
		return err
	}
	return nil
}
