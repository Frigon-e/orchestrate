package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

func benchRunPackages(r *benchRun, groups []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range benchSelectedGroups(r, groups) {
		for _, p := range r.Groups[name].Packages {
			out[p] = true
		}
	}
	return out
}
func compat(a, b *benchRun, key *benchKey) (level string, notes []string, refuse bool) {
	if a.Subject == "" || b.Subject == "" {
		return "not recorded", []string{"subject: not recorded for one or both runs"}, false
	}
	x, e := benchSubjectLoad(a.Subject)
	if e != nil {
		return "invalid subject", []string{e.Error()}, true
	}
	y, e := benchSubjectLoad(b.Subject)
	if e != nil {
		return "invalid subject", []string{e.Error()}, true
	}
	if x.Repo.Name != y.Repo.Name || x.Repo.CommonDir != y.Repo.CommonDir {
		return "other repo", []string{"different repository identities; use --cross-subject to compare"}, true
	}
	if x.Spec.Path != y.Spec.Path {
		vx, ex := benchSpecLoad(x.Spec.Hash)
		vy, ey := benchSpecLoad(y.Spec.Hash)
		if ex != nil || ey != nil || (vx.VariantOf != vy.Hash && vy.VariantOf != vx.Hash) {
			return "other spec path", []string{"different spec paths; use --cross-subject to compare"}, true
		}
	}
	if a.Subject == b.Subject {
		level = "same-subject"
	} else if x.Spec.Hash == y.Spec.Hash {
		level = "same-spec, other commit"
		notes = append(notes, fmt.Sprintf("code differed (%s vs %s): reuse counts and locations may not be comparable", orDefault(benchPrefix(x.Commit, 7), "commit unknown for "+a.Label), orDefault(benchPrefix(y.Commit, 7), "commit unknown for "+b.Label)))
	} else {
		level = "other spec version, same path"
		vx, ex := benchSpecLoad(x.Spec.Hash)
		vy, ey := benchSpecLoad(y.Spec.Hash)
		if ex != nil || ey != nil {
			notes = append(notes, "spec package content hashes unavailable")
		} else {
			hashes := map[string]string{}
			for _, p := range vx.Packages {
				hashes[p.ID] = p.ContentHash
			}
			count, unchanged := 0, 0
			changed := []string{}
			px, py := benchRunPackages(a, nil), benchRunPackages(b, nil)
			for _, p := range vy.Packages {
				if hashes[p.ID] == "" || !px[p.ID] || !py[p.ID] {
					continue
				}
				count++
				if hashes[p.ID] == p.ContentHash {
					unchanged++
				} else {
					changed = append(changed, p.ID)
				}
			}
			notes = append(notes, fmt.Sprintf("packages compared: %d; unchanged between versions: %d; changed: %s", count, unchanged, strings.Join(changed, ", ")))
			if vx.VariantOf == vy.Hash || vy.VariantOf == vx.Hash {
				level = "variant"
				variant := vy
				if vx.VariantOf == vy.Hash {
					variant = vx
				}
				notes = append(notes, fmt.Sprintf("variant %s of %s; package equality is measured by stored content hashes", variant.VariantLabel, variant.VariantOf))
			}
		}
	}
	if key != nil && key.Subject != "" && key.Subject != b.Subject {
		if k, e := benchSubjectLoad(key.Subject); e == nil && (k.Commit != y.Commit || k.Commit == "") {
			asserted := ""
			if y.CommitBasis != "git" && y.Commit != "" {
				asserted = " (asserted)"
			}
			notes = append(notes, fmt.Sprintf("key made at %s, run at %s%s: line ranges may have drifted", orDefault(benchPrefix(k.Commit, 7), "unknown"), orDefault(benchPrefix(y.Commit, 7), "unknown"), asserted))
		}
	}
	return level, notes, false
}
func benchChangedKeyItems(r *benchRun, key *benchKey) (int, bool) {
	runSubject, e := benchSubjectLoad(r.Subject)
	if e != nil {
		return 0, false
	}
	keySubject, e := benchSubjectLoad(key.Subject)
	if e != nil || keySubject.Spec.Hash == runSubject.Spec.Hash {
		return 0, false
	}
	runSpec, e := benchSpecLoad(runSubject.Spec.Hash)
	if e != nil {
		return 0, false
	}
	keySpec, e := benchSpecLoad(keySubject.Spec.Hash)
	if e != nil {
		return 0, false
	}
	runHashes, keyHashes := map[string]string{}, map[string]string{}
	for _, p := range runSpec.Packages {
		runHashes[p.ID] = p.ContentHash
	}
	for _, p := range keySpec.Packages {
		keyHashes[p.ID] = p.ContentHash
	}
	count := 0
	for _, item := range key.Items {
		if runHashes[item.Package] != "" && keyHashes[item.Package] != "" && runHashes[item.Package] != keyHashes[item.Package] {
			count++
		}
	}
	return count, true
}

func benchTestedOn(r *benchRun) string {
	if r.Subject == "" {
		return "tested on: subject: not recorded"
	}
	s, e := benchSubjectLoad(r.Subject)
	if e != nil {
		return "tested on: " + r.Subject + " (" + e.Error() + ")"
	}
	v, _ := benchSpecLoad(s.Spec.Hash)
	return "tested on: " + s.ID + " = " + benchDescribe(*s, v)
}
func benchReproduce(r *benchRun) string {
	if r.Subject == "" {
		return "not reproducible: subject not recorded"
	}
	s, e := benchSubjectLoad(r.Subject)
	if e != nil {
		return "not reproducible: " + e.Error()
	}
	if s.Repo.Path == "" || !benchExists(s.Repo.Path) {
		return "repo not reachable"
	}
	if s.Commit == "" {
		return "not reproducible: commit unknown"
	}
	if s.CommitBasis != "git" {
		return "not reproducible: commit asserted from " + s.CommitBasis + "; evidence: " + s.CommitEvidence
	}
	return fmt.Sprintf("Subject: `%s`\n\n`git -C %q checkout %s`\n\nSpec object to diff: `%s`", s.ID, s.Repo.Path, s.Commit, filepath.Join(benchDir(), "objects", "spec", s.Spec.Hash+".md"))
}
func benchComparisonScope(s benchSuite, runs []*benchRun, base *benchRun, key *benchKey, total bool) ([]string, []string, error) {
	notes := []string{}
	groups := map[string]bool{}
	for g := range base.Groups {
		if strings.HasPrefix(g, "group:") {
			groups[g] = true
		}
	}
	if len(s.Groups) > 0 {
		groups = map[string]bool{}
		for _, g := range s.Groups {
			groups[g] = true
		}
	}
	different := false
	baseGroups := benchSortedKeys(benchOnlyGroupKeys(base))
	for _, r := range runs {
		level, ns, refuse := compat(base, r, key)
		if refuse && !s.crossSubject {
			return nil, nil, fail("%s vs %s: %s: %s", base.Label, r.Label, level, strings.Join(ns, "; "))
		}
		if r.ID != base.ID || len(ns) > 0 {
			notes = append(notes, r.Label+": "+level)
			notes = append(notes, ns...)
		}
		if strings.Join(baseGroups, "\x00") != strings.Join(benchSortedKeys(benchOnlyGroupKeys(r)), "\x00") {
			different = true
		}
		for g := range groups {
			if _, ok := r.Groups[g]; !ok {
				delete(groups, g)
			}
		}
	}
	if total && different && !s.totalAll {
		return nil, nil, fail("group sets differ; use --total=all to explicitly include all groups")
	}
	if s.totalAll {
		notes = append(notes, "WARNING: totals include all groups despite differing group sets")
		return nil, notes, nil
	}
	if len(groups) == 0 && len(baseGroups) > 0 {
		return nil, nil, fail("no common comparison groups")
	}
	return benchSortedKeys(groups), notes, nil
}
func benchArmScoped(snap string, rec *gateRecord, packages map[string]bool) *armData {
	cp := *rec
	cp.Items = nil
	cp.Findings = nil
	for _, it := range rec.Items {
		if len(packages) == 0 || packages[it.Package] {
			cp.Items = append(cp.Items, it)
		}
	}
	for _, f := range rec.Findings {
		if len(packages) == 0 || packages[strings.TrimPrefix(f.Where, "package ")] {
			cp.Findings = append(cp.Findings, f)
		}
	}
	ad := loadArm(snap, rec.Experiment, snap, &cp)
	items := ad.items[:0]
	for _, it := range ad.items {
		if len(packages) == 0 || packages[it.Package] {
			items = append(items, it)
		}
	}
	ad.items = items
	allowed := map[string]bool{}
	for _, it := range items {
		allowed[it.ID] = true
	}
	for judge, verdicts := range ad.judges {
		for id := range verdicts {
			if !allowed[id] {
				delete(ad.judges[judge], id)
			}
		}
	}
	rows := ad.rows[:0]
	for _, row := range ad.rows {
		if len(packages) == 0 || packages[row.Package] {
			rows = append(rows, row)
		}
	}
	ad.rows = rows
	return ad
}

// Rankings need the same measured population and execution coverage. Permission
// to display cross-subject data is not permission to declare a winner.
func benchRankingCompatibility(a, b *benchRun) string {
	if a.Subject == "" || b.Subject == "" {
		return "subject unknown"
	}
	if a.Subject != b.Subject {
		return "different subjects"
	}
	if a.Itemset == "" || b.Itemset == "" {
		return "item population unknown"
	}
	if a.Itemset != b.Itemset {
		return "different item populations"
	}
	if strings.Join(benchSortedKeys(a.Groups), "\x00") != strings.Join(benchSortedKeys(b.Groups), "\x00") {
		return "different pass coverage"
	}
	for g, x := range a.Groups {
		px, py := map[string]bool{}, map[string]bool{}
		for _, p := range x.Packages {
			px[p] = true
		}
		for _, p := range b.Groups[g].Packages {
			py[p] = true
		}
		if strings.Join(benchSortedKeys(px), "\x00") != strings.Join(benchSortedKeys(py), "\x00") {
			return "different package populations"
		}
	}
	return ""
}

func benchOnlyGroupKeys(r *benchRun) map[string]bool {
	out := map[string]bool{}
	for g := range r.Groups {
		if strings.HasPrefix(g, "group:") {
			out[g] = true
		}
	}
	return out
}
