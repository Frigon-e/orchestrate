package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type specPackage struct {
	id, title                          string
	kind, model, effort, harness, base string
	deps, owns                         []string
	body, acceptance, raw              string
	builds                             []buildItem // the bullets of `#### Builds`
}

type spec struct {
	defaults map[string]string
	title    string
	sections map[string]string
	packages []*specPackage
	problems []string
	lines    map[string][2]int // 1-based line ranges in the spec file: "## Name" sections and "### id" packages
}

func (sp *spec) add(format string, a ...any) {
	sp.problems = append(sp.problems, fmt.Sprintf(format, a...))
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var packageKeys = map[string]bool{"kind": true, "model": true, "effort": true, "harness": true, "depends": true, "owns": true}

// parseSpec reads the spec format of references/spec-format.md. Headings
// inside code fences are ignored. Problems are collected, not returned early.
func parseSpec(src string) *spec {
	sp := &spec{defaults: map[string]string{}, sections: map[string]string{}}
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		end := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				end = i
				break
			}
		}
		if end < 0 {
			sp.add("front matter has no closing ---")
		} else {
			for _, l := range lines[1:end] {
				if k, v, ok := strings.Cut(l, ":"); ok {
					sp.defaults[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
			lines = lines[end+1:]
		}
	}

	// level-2 sections, with the package area kept as raw lines
	var cur string
	secLines := map[string][]string{}
	fenced := false
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
		} else if !fenced {
			if m := headingRe.FindStringSubmatch(l); m != nil && sp.title == "" {
				sp.title = m[1]
				continue
			}
			if strings.HasPrefix(l, "## ") {
				cur = strings.TrimSpace(l[3:])
				secLines[cur] = nil
				continue
			}
		}
		if cur != "" {
			secLines[cur] = append(secLines[cur], l)
		}
	}
	for k, v := range secLines {
		sp.sections[k] = strings.TrimSpace(strings.Join(v, "\n"))
	}
	if sp.title == "" {
		sp.add("spec has no `# Title` heading")
	}
	for _, name := range []string{"Behaviour", "Design rules", "Authority", "Packages", "Open questions"} {
		if _, ok := sp.sections[name]; !ok {
			sp.add("section `## %s` is missing", name)
		}
	}
	for _, name := range []string{"Behaviour", "Design rules"} {
		if t, ok := sp.sections[name]; ok && t == "" {
			sp.add("section `## %s` is empty", name)
		}
	}
	if auth, ok := sp.sections["Authority"]; ok {
		for _, label := range []string{"May decide", "Must escalate", "Never"} {
			if labelled(auth, label) == "" {
				sp.add("Authority needs a non-empty `%s:` line", label)
			}
		}
	}
	if oq, ok := sp.sections["Open questions"]; ok && oq != "" && oq != "None." {
		sp.add("open questions are not empty; ask them first")
	}
	if _, ok := sp.sections["Packages"]; ok {
		sp.parsePackages(secLines["Packages"])
	}
	return sp
}

func (sp *spec) parsePackages(lines []string) {
	var blocks [][]string
	fenced := false
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(l, "### ") {
			blocks = append(blocks, nil)
		}
		if len(blocks) > 0 {
			blocks[len(blocks)-1] = append(blocks[len(blocks)-1], l)
		}
	}
	if len(blocks) == 0 {
		sp.add("`## Packages` has no `### <id>: <title>` block")
	}
	for _, b := range blocks {
		sp.parsePackage(b)
	}
}

func (sp *spec) parsePackage(block []string) {
	head := strings.TrimSpace(block[0][4:])
	id, title, _ := strings.Cut(head, ":")
	p := &specPackage{id: strings.TrimSpace(id), title: strings.TrimSpace(title), raw: strings.TrimSpace(strings.Join(block, "\n"))}
	sp.packages = append(sp.packages, p)
	name := p.id
	if name == "" {
		name = head
	}
	if p.title == "" {
		sp.add("package %s: heading needs `### <id>: <title>`", name)
	}
	keys := map[string]string{}
	i := 1
	for ; i < len(block) && strings.TrimSpace(block[i]) != ""; i++ {
		k, v, ok := strings.Cut(block[i], ":")
		k = strings.TrimSpace(k)
		switch {
		case !ok:
			sp.add("package %s: line %q is not `key: value`", name, block[i])
		case !packageKeys[k]:
			sp.add("package %s: unknown key %q", name, k)
		default:
			keys[k] = strings.TrimSpace(v)
		}
	}
	var body, acc, bld []string
	cur, fenced := "", false
	for _, l := range block[i:] {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(l, "#### ") {
			switch strings.TrimSpace(l[5:]) {
			case "Acceptance":
				cur = "acc"
				continue
			case "Builds":
				cur = "builds"
				continue
			}
			cur = ""
		}
		switch cur {
		case "acc":
			acc = append(acc, l)
		case "builds":
			bld = append(bld, l)
		default:
			body = append(body, l)
		}
	}
	p.body = strings.TrimSpace(strings.Join(body, "\n"))
	p.acceptance = strings.TrimSpace(strings.Join(acc, "\n"))
	var bprob []string
	p.builds, bprob = parseBuilds(strings.Join(bld, "\n"))
	for _, m := range bprob {
		sp.add("package %s: %s", name, m)
	}
	p.kind = keys["kind"]
	pick := func(key string) string {
		if v := keys[key]; v != "" {
			return v
		}
		return sp.defaults[key]
	}
	p.model, p.effort, p.harness = pick("model"), pick("effort"), pick("harness")
	p.deps, p.owns = splitList(keys["depends"]), splitList(keys["owns"])
	if _, ok := sp.sections["Packages"]; !ok {
		return
	}
	if !taskIDRe.MatchString(p.id) {
		sp.add("package %q: id must match %s", p.id, taskIDRe)
	}
	if p.kind != "ship" && p.kind != "scout" {
		sp.add("package %s: kind must be ship or scout", name)
	}
	if p.harness == "auto" && (p.model != "auto" || p.effort != "auto") {
		sp.add("package %s: harness auto needs model auto and effort auto", name)
	} else if p.harness != "claude" && p.harness != "pi" && p.harness != "auto" {
		sp.add("package %s: harness must be claude or pi (set it on the package or in the front matter)", name)
	}
	if p.model == "" {
		sp.add("package %s: no model (set it on the package or in the front matter)", name)
	}
	if p.effort == "" {
		sp.add("package %s: no effort (set it on the package or in the front matter)", name)
	}
	if p.kind == "ship" && len(p.owns) == 0 {
		sp.add("package %s: a ship package needs `owns:`", name)
	}
	if p.body == "" {
		sp.add("package %s: needs body text between its key lines and `#### Acceptance`", name)
	}
	if p.acceptance == "" {
		sp.add("package %s: needs a non-empty `#### Acceptance` subsection", name)
	}
}

// validate checks the cross-package rules.
func (sp *spec) validate() {
	ids := map[string]bool{}
	for _, p := range sp.packages {
		if ids[p.id] {
			sp.add("package id %s is used twice", p.id)
		}
		ids[p.id] = true
	}
	for _, p := range sp.packages {
		for _, d := range p.deps {
			if !ids[d] {
				sp.add("package %s: depends on %q, which is not a package of this spec", p.id, d)
			}
		}
	}
	if _, left := sp.waves(); len(left) > 0 {
		sp.add("dependency cycle among: %s", strings.Join(left, " "))
	}
	for i, p := range sp.packages {
		for _, q := range sp.packages[i+1:] {
			for _, a := range p.owns {
				for _, b := range q.owns {
					if ownsOverlap(a, b) {
						sp.add("ownership overlap: %s owns %q and %s owns %q", p.id, a, q.id, b)
					}
				}
			}
		}
	}
}

// waves returns topological layers (sorted ids) and the ids stuck in a cycle.
func (sp *spec) waves() (waves [][]string, left []string) {
	done := map[string]bool{}
	pending := map[string]*specPackage{}
	for _, p := range sp.packages {
		pending[p.id] = p
	}
	known := map[string]bool{}
	for id := range pending {
		known[id] = true
	}
	for len(pending) > 0 {
		var wave []string
		for id, p := range pending {
			ready := true
			for _, d := range p.deps {
				if known[d] && !done[d] {
					ready = false
				}
			}
			if ready {
				wave = append(wave, id)
			}
		}
		if len(wave) == 0 {
			for id := range pending {
				left = append(left, id)
			}
			sort.Strings(left)
			return waves, left
		}
		sort.Strings(wave)
		for _, id := range wave {
			done[id] = true
			delete(pending, id)
		}
		waves = append(waves, wave)
	}
	return waves, nil
}

func normOwn(p string) (path string, dir bool) {
	p = strings.TrimSpace(p)
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	dir = strings.HasSuffix(p, "/")
	return strings.TrimRight(p, "/"), dir
}

// ownsOverlap: equal, or one is a directory that contains the other.
func ownsOverlap(a, b string) bool {
	pa, da := normOwn(a)
	pb, db := normOwn(b)
	return pa == pb || (da && strings.HasPrefix(pb, pa+"/")) || (db && strings.HasPrefix(pa, pb+"/"))
}

// sameGitCommonDir reports whether two checkouts belong to one repository.
func sameGitCommonDir(a, b string) bool {
	ca, err1 := git(a, "rev-parse", "--path-format=absolute", "--git-common-dir")
	cb, err2 := git(b, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err1 != nil || err2 != nil || ca == "" || cb == "" {
		return false
	}
	ra, err1 := filepath.EvalSymlinks(ca)
	rb, err2 := filepath.EvalSymlinks(cb)
	return err1 == nil && err2 == nil && ra == rb
}

func cmdPlan(a *app, args []string) error {
	fs := a.flags("plan")
	dry := fs.Bool("dry-run", false, "check the spec and show what would be created; write nothing")
	degraded := fs.Bool("degraded", false, "allow degraded fallback profiles (ask the user first)")
	noCtx := fs.Bool("no-ctx", false, "do not add the code-built facts section to the briefs")
	skipGate := fs.Bool("skip-gate", false, "plan without a passed `orch gate` (only with the user's agreement)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "plan <spec> [--dry-run]"); err != nil {
		return err
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	specPath := expandHome(pos[0])
	if !filepath.IsAbs(specPath) {
		specPath = filepath.Join(a.cwd, specPath)
	}
	real, err := filepath.EvalSymlinks(specPath)
	if err != nil {
		return fail("spec %s: %v", pos[0], err)
	}
	// The spec may live in a linked worktree of the repo (its base branch is
	// checked out there), so its path is taken from that worktree's top-level.
	top, err := git(filepath.Dir(real), "rev-parse", "--show-toplevel")
	if err != nil || top == "" || !sameGitCommonDir(top, s.repo) {
		return fail("spec %s is not inside the repo %s", pos[0], s.repo)
	}
	if top, err = filepath.EvalSymlinks(top); err != nil {
		return err
	}
	rel, err := filepath.Rel(top, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fail("spec %s is not inside the repo %s", pos[0], s.repo)
	}
	rel = filepath.ToSlash(rel)
	src, err := os.ReadFile(real)
	if err != nil {
		return err
	}
	sp := parseSpec(string(src))
	sp.validate()
	sp.lines = specRanges(string(src))
	if !*skipGate {
		if passed, reason := gateApproval(s.root, top, src); !passed {
			sp.add("the gate has not passed for this version of the spec and checkout (%s): run `orch gate %s`, fix what it reports, and rerun (or pass --skip-gate with the user's agreement)", reason, pos[0])
		}
	}

	baseBranch := s.cfg.BaseBranch
	if b := sp.defaults["base"]; b != "" {
		baseBranch = b
	}
	if strings.HasPrefix(baseBranch, "-") {
		sp.add("base %q is not a branch or commit", baseBranch)
	} else if sha, err := git(s.repo, "rev-parse", "--verify", "--quiet", "--short=12", baseBranch+"^{commit}"); err != nil || sha == "" {
		sp.add("base %q does not resolve in %s", baseBranch, s.repo)
	} else if _, err := git(s.repo, "cat-file", "-e", sha+":"+rel); err != nil {
		sp.add("%s is not in %s: commit the spec to %s first; leads read it from their worktree", rel, baseBranch, baseBranch)
	} else if _, err := git(top, "diff", "--quiet", sha, "--", rel); err != nil {
		sp.add("spec has changes not in %s: commit them first", baseBranch)
	}

	var intBranch string
	if sp.hasDeps() {
		sl := slug(strings.TrimSuffix(filepath.Base(real), filepath.Ext(real)))
		if intBranch = integrationPrefix + sl; sl == "" || !integrationRe.MatchString(intBranch) {
			sp.add("spec file name %q gives no safe integration branch name", filepath.Base(real))
		} else if out, err := gitOutput(s.repo, "check-ref-format", "--branch", intBranch); err != nil {
			sp.add("integration branch %s is not a valid branch name: %s", intBranch, out)
		} else if owner := integrationOwner(s.root, sl); owner != "" && owner != rel {
			sp.add("integration branch %s already belongs to the spec %s; rename this spec file (%s), or delete %s if that spec is gone", intBranch, owner, rel, integrationSpecFile(s.root, sl))
		}
	}

	status := map[string]string{}
	for _, p := range sp.packages {
		if !taskIDRe.MatchString(p.id) {
			continue
		}
		path := filepath.Join(s.root, p.id, "task.md")
		if _, err := os.Stat(path); err != nil {
			status[p.id] = "created"
			continue
		}
		fm, _, _ := readTask(path)
		switch {
		case fm["spec"] != rel+"#"+p.id:
			sp.add("task id %s is already used by another task (%s)", p.id, path)
		case fm["spec_hash"] != "" && fm["spec_hash"] != specInputsHash(p):
			status[p.id] = "stale"
		default:
			status[p.id] = "exists"
		}
	}

	for _, p := range sp.packages {
		if status[p.id] == "created" {
			if err := harnessUsable(s.cfg, p.harness, p.model, *degraded, a.now()); err != nil {
				sp.add("package %s: %v", p.id, err)
			}
		}
	}

	if n := len(sp.problems); n > 0 {
		fmt.Fprintf(a.out, "problems[%d]:\n", n)
		for _, p := range sp.problems {
			fmt.Fprintf(a.out, "  %s\n", agentScalar(p))
		}
		noun := "problems"
		if n == 1 {
			noun = "problem"
		}
		return fail("spec has %d %s; nothing was created", n, noun)
	}

	intNote := ""
	if intBranch != "" {
		suffix := ""
		if *dry {
			suffix = " (would create)"
		}
		intNote = fmt.Sprintf("integration: %s%s\nmerge_worktree: %s\n", intBranch, suffix, filepath.Join(integrationDir(s.root, strings.TrimPrefix(intBranch, integrationPrefix)), "wt"))
		if !*dry {
			if err := ensureIntegrationBranch(s.repo, intBranch, baseBranch); err != nil {
				return err
			}
			if err := recordIntegrationOwner(s.root, strings.TrimPrefix(intBranch, integrationPrefix), rel); err != nil {
				return err
			}
		}
	}
	for _, p := range sp.packages {
		if status[p.id] != "created" {
			continue
		}
		if *dry {
			status[p.id] = "would-create"
			continue
		}
		o := withDegraded(sp.taskOpts(p, rel, baseBranch), *degraded)
		o.noCtx = *noCtx
		if intBranch != "" {
			o.base = intBranch
			if p.kind == "ship" {
				o.integration = intBranch
			}
		}
		if _, err := newTask(s, o); err != nil {
			return err
		}
	}

	waves, _ := sp.waves()
	var stale []string
	fmt.Fprintf(a.out, "spec: %s\npackages: %d\n%s", agentScalar(rel), len(sp.packages), intNote)
	fmt.Fprintf(a.out, "tasks[%d]{id,kind,model,depends,status}:\n", len(sp.packages))
	for _, p := range sp.packages {
		deps := ""
		if len(p.deps) > 0 {
			deps = agentScalar(strings.Join(p.deps, " "))
		}
		fmt.Fprintf(a.out, "  %s,%s,%s,%s,%s\n", agentScalar(p.id), agentScalar(p.kind), agentScalar(p.model), deps, status[p.id])
		if status[p.id] == "stale" {
			stale = append(stale, p.id)
		}
	}
	if len(stale) > 0 {
		// An existing brief is never rewritten; the old one may be running.
		fmt.Fprintf(a.out, "stale[%d]:\n", len(stale))
		for _, id := range stale {
			fmt.Fprintf(a.out, "  %s\n", agentScalar(id+": the spec changed since its brief was written; close its lead if one is open, remove "+filepath.Join(s.root, id)+" by hand and rerun orch plan"))
		}
		return fail("%d stale brief(s); do not spawn until they are replaced", len(stale))
	}
	fmt.Fprintf(a.out, "waves[%d]:\n", len(waves))
	for _, w := range waves {
		fmt.Fprintf(a.out, "  %s\n", agentScalar(strings.Join(w, " ")))
	}
	fmt.Fprintln(a.out, "help[2]:\n  orch check <id>\n  orch spawn <id>")
	return nil
}

func (sp *spec) hasDeps() bool {
	for _, p := range sp.packages {
		if len(p.deps) > 0 {
			return true
		}
	}
	return false
}

func withDegraded(o taskOpts, degraded bool) taskOpts {
	o.degraded = degraded
	return o
}

func (sp *spec) taskOpts(p *specPackage, rel, base string) taskOpts {
	in := strings.Join(p.owns, ", ")
	if in == "" {
		in = "research only, no repo edits"
	}
	out := sp.scopeOut(p, rel)
	auth := sp.sections["Authority"]
	return taskOpts{
		id: p.id, kind: p.kind, harness: p.harness, model: p.model, effort: p.effort, base: base,
		deps: p.deps, spec: rel + "#" + p.id, owns: p.owns, specHash: specInputsHash(p),
		fill: map[string]string{
			"Intent": fmt.Sprintf("%s: %s. The feature is specified in `%s`; read the sections under Pointers, not the whole file.", sp.title, p.title, rel),
			"Scope":  "In: " + in + "\nOut: " + out,
			"Pointers": fmt.Sprintf("%s - what the feature does\n%s - decisions already made\n%s - this package",
				sp.cite(rel, "## Behaviour", "Behaviour"), sp.cite(rel, "## Design rules", "Design rules"), sp.cite(rel, "### "+p.id, p.id)),
			"Spec":       specText(p),
			"Acceptance": p.acceptance,
			"Authority": fmt.Sprintf("May decide: %s\nMust escalate: %s\nNever: %s",
				labelled(auth, "May decide"), labelled(auth, "Must escalate"), labelled(auth, "Never")),
		},
	}
}

// scopeOutMax is the longest file list a brief's Out line spells out; past it
// the line names the other packages and points at their owns lines.
const scopeOutMax = 1000

// scopeOut is the Out line of a package's Scope.
func (sp *spec) scopeOut(p *specPackage, rel string) string {
	var others, ids []string
	for _, q := range sp.packages {
		if q != p {
			others = append(others, q.owns...)
			ids = append(ids, q.id)
		}
	}
	if len(others) == 0 {
		return "anything not listed in In"
	}
	list := strings.Join(others, ", ")
	if len(list) <= scopeOutMax {
		return "files owned by other packages of this spec: " + list
	}
	return fmt.Sprintf("every file owned by another package of this spec (%d files cut from this list; each package's `owns:` line is in %s): %s",
		len(others), sp.cite(rel, "## Packages", "Packages"), strings.Join(ids, ", "))
}

// cite is a pointer to a spec section as `path:first-last`, or the heading
// when the line range is not known.
func (sp *spec) cite(rel, heading, key string) string {
	if r, ok := sp.lines[key]; ok {
		return fmt.Sprintf("`%s:%d-%d` %s", rel, r[0], r[1], heading)
	}
	return fmt.Sprintf("`%s` %s", rel, heading)
}

// specRanges maps "## Name" sections (by name) and "### id: title" packages
// (by id) to their 1-based line ranges in src. A range ends before the next
// heading of the same or a higher level; headings inside fences are ignored.
func specRanges(src string) map[string][2]int {
	out := map[string][2]int{}
	type open struct {
		key   string
		level int
		start int
	}
	var stack []open
	closeTo := func(level, end int) {
		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			o := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			out[o.key] = [2]int{o.start, end}
		}
	}
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	fenced := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		switch {
		case strings.HasPrefix(l, "## "):
			closeTo(2, i)
			stack = append(stack, open{strings.TrimSpace(l[3:]), 2, i + 1})
		case strings.HasPrefix(l, "### "):
			closeTo(3, i)
			id, _, _ := strings.Cut(strings.TrimSpace(l[4:]), ":")
			stack = append(stack, open{strings.TrimSpace(id), 3, i + 1})
		}
	}
	closeTo(2, len(lines))
	for k, r := range out {
		for r[1] > r[0] && strings.TrimSpace(lines[r[1]-1]) == "" {
			r[1]--
		}
		out[k] = r
	}
	return out
}

// specText is the Spec section of a task: the package body, then its Builds
// bullets as written.
func specText(p *specPackage) string {
	if len(p.builds) == 0 {
		return p.body
	}
	var b strings.Builder
	b.WriteString(p.body)
	b.WriteString("\n\nBuilds:\n")
	for _, it := range packageItems(p) {
		fmt.Fprintf(&b, "- %s: %s\n", it.ID, it.Text)
		if len(it.Supersedes) > 0 {
			fmt.Fprintf(&b, "  supersedes: %s\n", strings.Join(it.Supersedes, ", "))
		}
		for _, q := range it.Queries {
			fmt.Fprintf(&b, "  search: %s\n", q)
		}
		if len(it.Reuse) > 0 {
			fmt.Fprintf(&b, "  reuse: %s\n", strings.Join(it.Reuse, ", "))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
