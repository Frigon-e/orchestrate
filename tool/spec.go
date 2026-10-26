package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type modelRunner func(role, prompt string) (string, error)

var queryModelRunner modelRunner // Optional test override; production resolves the role.

func (a *app) generateQueries(role, prompt string) (string, error) {
	if queryModelRunner != nil {
		return queryModelRunner(role, prompt)
	}
	s, err := a.site()
	if err != nil {
		return "", err
	}
	return s.runModel(role, prompt)
}

type specQueriesPackage struct {
	ID    string     `json:"id"`
	Items []gateItem `json:"items"`
}

func cmdSpec(a *app, args []string) error {
	if len(args) > 0 && args[0] == "ids" {
		return cmdSpecIDs(a, args[1:])
	}
	if len(args) == 0 || args[0] != "queries" {
		return &exitError{code: 2, msg: "usage: orch spec ids <spec> | orch spec queries <spec> [--package id | --item pkg#id] [--json] [--generate [--dry-run] [--all]]"}
	}
	fs := a.flags("spec queries")
	pkg := fs.String("package", "", "select one package")
	item := fs.String("item", "", "select one item (<package>#<slug>, or legacy <package>#<n>)")
	asJSON := fs.Bool("json", false, "print JSON")
	gen := fs.Bool("generate", false, "propose missing queries; never edit the spec")
	dry := fs.Bool("dry-run", false, "print the generation prompt without calling a model")
	all := fs.Bool("all", false, "allow generation across the whole spec")
	fs.Usage = func() {
		fmt.Fprintln(a.errw, "usage: orch spec queries <spec> [options]\nExamples:\n  orch spec queries spec.md --package api\n  orch spec queries spec.md --item 'api#1' --generate --dry-run\n  orch spec queries spec.md --generate --all")
		fs.PrintDefaults()
	}
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "spec queries <spec> [--package id | --item id#n] [--json] [--generate [--dry-run] [--all]]"); err != nil {
		return err
	}
	if *pkg != "" && *item != "" {
		return &exitError{code: 2, msg: "use --package or --item, not both"}
	}
	if (*dry || *all) && !*gen {
		return &exitError{code: 2, msg: "--dry-run and --all require --generate"}
	}
	if *gen && *pkg == "" && *item == "" && !*all {
		return &exitError{code: 2, msg: "generation needs --package, --item, or explicit --all"}
	}
	path := pos[0]
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.cwd, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sp := parseSpec(string(b))
	if len(sp.problems) > 0 {
		return fail("spec has problems: %s", strings.Join(sp.problems, "; "))
	}
	var groups []specQueriesPackage
	var selected []*specPackage
	for _, p := range sp.packages {
		if *pkg != "" && p.id != *pkg {
			continue
		}
		its := []gateItem{}
		for _, it := range packageItems(p) {
			if *item == "" || it.ID == *item {
				its = append(its, it)
			}
		}
		if *item != "" && len(its) == 0 {
			continue
		}
		groups = append(groups, specQueriesPackage{ID: p.id, Items: its})
		selected = append(selected, p)
	}
	if (*pkg != "" || *item != "") && len(groups) == 0 {
		return fail("no matching package or item: %s%s", *pkg, *item)
	}
	if *gen {
		prompt, count := queriesPrompt(groups, selected)
		if count == 0 {
			fmt.Fprintln(a.out, "No selected items need search lines.")
			return nil
		}
		output := prompt
		if !*dry {
			output, err = a.generateQueries("spec_queries", prompt)
			if err != nil {
				return fail("spec_queries skipped: %v", err)
			}
		}
		if *asJSON {
			return json.NewEncoder(a.out).Encode(output)
		}
		fmt.Fprintln(a.out, output)
		return nil
	}
	if *asJSON {
		return json.NewEncoder(a.out).Encode(groups)
	}
	if len(groups) == 0 {
		fmt.Fprintln(a.out, "No packages found.")
	}
	for _, g := range groups {
		fmt.Fprintf(a.out, "Package %s\n", g.ID)
		if len(g.Items) == 0 {
			fmt.Fprintln(a.out, "  No Builds bullets; reuse checklist skipped.")
		}
		for _, it := range g.Items {
			fmt.Fprintf(a.out, "- %s: %s\n", it.ID, it.Text)
			for _, q := range it.Queries {
				fmt.Fprintln(a.out, "  search: "+q)
			}
			if len(it.Reuse) > 0 {
				fmt.Fprintln(a.out, "  reuse: "+strings.Join(it.Reuse, ", "))
			}
		}
	}
	return nil
}

const queriesGuidance = `Propose spec-format text a human can paste. Never edit the spec. Return only the selected packages/items, with #### Builds and plain-prose bullets, one human-readable deliverable per bullet. Keep existing bullet text, id and supersedes lines, and reuse pointers as written. For legacy packages without Builds, propose bullets from the supplied body.
Add optional indented search: lines only where useful. Query guidance: Code that <action> on <entity>, when <trigger>, producing <result>. Drop any slot that does not apply. Do not invent symbols or paths. One behaviour per query, not noun lists. Multiple search lines are allowed for distinct behaviours. Omit search for tiny bullets without a search target or near-duplicates that would only duplicate candidates and skew counts. reuse: is optional, comma-separated known pointers; prefer path#Symbol or path#Type.Method, with path:line or path:line-line for blocks, SQL, config, generated code or docs.
Good (full): Code that saves a payment method for a customer, when a checkout completes, producing a stored method row
Good (short): Code that lists payment gateways
Bad (noun list): payments, customers, checkout, gateways

Selected input:
`

func queriesPrompt(groups []specQueriesPackage, packages []*specPackage) (string, int) {
	var b strings.Builder
	b.WriteString(queriesGuidance)
	count := 0
	for i, g := range groups {
		var pending []gateItem
		for _, it := range g.Items {
			if len(it.Queries) == 0 {
				pending = append(pending, it)
			}
		}
		if len(packages[i].builds) == 0 {
			count++
			fmt.Fprintf(&b, "### %s: %s\nLegacy package body:\n%s\nAcceptance:\n%s\n", g.ID, packages[i].title, packages[i].body, packages[i].acceptance)
		} else if len(pending) > 0 {
			fmt.Fprintf(&b, "### %s: %s\n#### Builds\n", g.ID, packages[i].title)
			for _, it := range pending {
				count++
				fmt.Fprintf(&b, "- %s\n", it.Text)
				_, id, _ := strings.Cut(it.ID, "#")
				if requirementIDRe.MatchString(id) {
					fmt.Fprintf(&b, "  id: %s\n", id)
				}
				if len(it.Supersedes) > 0 {
					var prior []string
					for _, key := range it.Supersedes {
						_, slug, _ := strings.Cut(key, "#")
						prior = append(prior, slug)
					}
					fmt.Fprintf(&b, "  supersedes: %s\n", strings.Join(prior, ", "))
				}
				if len(it.Reuse) > 0 {
					fmt.Fprintf(&b, "  reuse: %s\n", strings.Join(it.Reuse, ", "))
				}
			}
		}
	}
	return b.String(), count
}

// cmdSpecIDs proposes fresh names only. A preview is not a historical mapping
// and never copies answers, even when wording or source pointers are identical.
func cmdSpecIDs(a *app, args []string) error {
	fs := a.flags("spec ids")
	fs.Usage = func() {
		fmt.Fprintln(a.errw, "usage: orch spec ids <spec>\nExamples:\n  orch spec ids spec.md\n  orch spec ids docs/specs/transfers.md\nPrint proposed IDs without editing the spec or inheriting historical answers.")
		fs.PrintDefaults()
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "spec ids <spec>"); err != nil {
		return err
	}
	path := pos[0]
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.cwd, path)
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sp := parseSpec(string(src))
	if len(sp.problems) > 0 {
		return fail("spec has problems: %s", strings.Join(sp.problems, "; "))
	}
	fmt.Fprintln(a.out, "ID preview only; spec unchanged. Historical answers require an explicit mapping and revalidation, never an ordinal or text match.")
	proposed := 0
	for _, p := range sp.packages {
		used, texts := map[string]bool{}, map[string]int{}
		for _, b := range p.builds {
			used[b.ID] = b.ID != ""
			for _, prior := range b.Supersedes {
				used[prior] = true
			}
			texts[strings.TrimSpace(b.Text)]++
		}
		for i, b := range p.builds {
			if b.ID != "" {
				fmt.Fprintf(a.out, "keep %s\n", itemKey(p.id, b.ID))
				continue
			}
			base := proposedRequirementID(b.Text)
			id := base
			for n := 2; used[id]; n++ {
				id = base + "-" + strconv.Itoa(n)
			}
			used[id] = true
			fmt.Fprintf(a.out, "%s#%d -> %s\n  id: %s\n", p.id, i+1, itemKey(p.id, id), id)
			if texts[strings.TrimSpace(b.Text)] > 1 {
				fmt.Fprintln(a.out, "  ambiguous history: repeated requirement text; explicit mapping required")
			}
			proposed++
		}
	}
	fmt.Fprintf(a.out, "Proposed IDs: %d\n", proposed)
	return nil
}

func proposedRequirementID(text string) string {
	var b strings.Builder
	separator := false
	for _, r := range strings.ToLower(text) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if separator && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			separator = false
		} else {
			separator = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	id := b.String()
	if id == "" {
		return "requirement"
	}
	if id[0] < 'a' || id[0] > 'z' {
		id = "requirement-" + id
	}
	return id
}

// The relation of an owning package to the package under review.
const (
	relDownstream = "which uses this package" // it depends on pkg: it wires pkg's exports in
	relUpstream   = "which this package uses" // pkg depends on it: it should already provide this
	relUnrelated  = "unrelated to this package"
)

// ownerOf is the package that owns file: the one with the longest matching
// owns entry, or nil when none does.
func (sp *spec) ownerOf(file string) *specPackage {
	var best *specPackage
	n := -1
	for _, p := range sp.packages {
		for _, o := range p.owns {
			if ownedPath([]string{o}, file) && len(strings.TrimSuffix(o, "/")) > n {
				best, n = p, len(strings.TrimSuffix(o, "/"))
			}
		}
	}
	return best
}

// dependsOn reports whether a depends on b, directly or through other packages.
func (sp *spec) dependsOn(a, b *specPackage) bool {
	seen := map[string]bool{}
	var walk func(p *specPackage) bool
	walk = func(p *specPackage) bool {
		for _, d := range p.deps {
			if d == b.id {
				return true
			}
			if seen[d] {
				continue
			}
			seen[d] = true
			for _, q := range sp.packages {
				if q.id == d && walk(q) {
					return true
				}
			}
		}
		return false
	}
	return walk(a)
}

// relation says how the owner relates to the package under review.
func (sp *spec) relation(pkg, owner *specPackage) string {
	switch {
	case sp.dependsOn(owner, pkg):
		return relDownstream
	case sp.dependsOn(pkg, owner):
		return relUpstream
	}
	return relUnrelated
}
