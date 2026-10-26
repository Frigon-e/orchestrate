package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func sameLocation(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return pointerRe.MatchString(a)
	}
	x, y := pointerRe.FindStringSubmatch(a), pointerRe.FindStringSubmatch(b)
	if x == nil || y == nil || x[1] != y[1] {
		return false
	}
	if x[4] != "" || y[4] != "" {
		return x[4] != "" && x[4] == y[4]
	}
	startX, _ := strconv.Atoi(x[2])
	startY, _ := strconv.Atoi(y[2])
	endX, endY := startX, startY
	if x[3] != "" {
		endX, _ = strconv.Atoi(x[3])
	}
	if y[3] != "" {
		endY, _ = strconv.Atoi(y[3])
	}
	return startX <= endY && startY <= endX
}

// Rendered evidence must contain the entire requested behavior span. Mere
// overlap, a signature, or an author hint cannot prove the body was delivered.
func evidenceContainsTarget(e evidenceRecord, target string) bool {
	m := pointerRe.FindStringSubmatch(target)
	if m == nil || e.Path != m[1] || e.LocatorValid == nil || !*e.LocatorValid {
		return false
	}
	if e.Range == nil {
		return true
	} // explicitly rendered whole source
	if e.Range.Validate() != nil || e.Range.StartColumn != nil || e.Range.EndColumn != nil || m[2] == "" {
		return false
	}
	start, _ := strconv.Atoi(m[2])
	end := start
	if m[3] != "" {
		end, _ = strconv.Atoi(m[3])
	}
	return start > 0 && end >= start && e.Range.StartLine <= start && e.Range.EndLine >= end
}

func (a *armData) addLocation(judge, kind, loc string) {
	if (kind != verdictReuse && kind != verdictExtend) || loc == "" {
		return
	}
	if a.locations[judge] == nil {
		a.locations[judge] = map[string]map[string]bool{}
	}
	if a.locations[judge][kind] == nil {
		a.locations[judge][kind] = map[string]bool{}
	}
	for have := range a.locations[judge][kind] {
		if sameLocation(have, loc) {
			return
		}
	}
	a.locations[judge][kind][loc] = true
}

// Old runs saved the checklist only in the rendered dossiers. Recover locations
// there so their shadow candidate numbers can still be scored against new runs.
func legacyChecklist(dir string) map[string][]itemCands {
	files, _ := filepath.Glob(filepath.Join(dir, "dossier", "*.md"))
	out := map[string][]itemCands{}
	item := regexp.MustCompile("^- `([^`#]+#(?:[0-9]+|[a-z][a-z0-9._-]*))` (.*)$")
	cand := regexp.MustCompile("^  [0-9]+\\. `([^`]+)`")
	for _, file := range files {
		b, _ := os.ReadFile(file)
		var cur *itemCands
		for _, line := range strings.Split(string(b), "\n") {
			if m := item.FindStringSubmatch(line); m != nil {
				pkg, _, _ := strings.Cut(m[1], "#")
				out[pkg] = append(out[pkg], itemCands{Item: gateItem{ID: m[1], Package: pkg, Text: m[2]}})
				cur = &out[pkg][len(out[pkg])-1]
			} else if m := cand.FindStringSubmatch(line); m != nil && cur != nil {
				p := pointerRe.FindStringSubmatch(m[1])
				if p == nil || p[2] == "" {
					continue
				}
				start, _ := strconv.Atoi(p[2])
				end := start
				if p[3] != "" {
					end, _ = strconv.Atoi(p[3])
				}
				cur.Cands = append(cur.Cands, reuseCand{Path: p[1], Start: start, End: end})
			} else if strings.HasPrefix(line, "### ") {
				cur = nil
			}
		}
	}
	return out
}

func checklistStats(dir string, checklist map[string][]itemCands) string {
	if checklist == nil {
		return ""
	}
	withQueries, queries, bodies := 0, 0, 0
	distinct := map[string]bool{}
	header := regexp.MustCompile("^  [0-9]+\\. `")
	for pkg, ics := range checklist {
		for _, ic := range ics {
			if len(ic.Item.Queries) > 0 {
				withQueries++
			}
			queries += ic.QueriesRun
			for _, c := range ic.Cands {
				distinct[c.at()] = true
			}
		}
		b, _ := os.ReadFile(filepath.Join(dir, "dossier", passFile(pkg)+".md"))
		pending := false
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "- `") {
				pending = false
			}
			if header.MatchString(l) {
				pending = true
			}
			if pending && strings.HasPrefix(l, "     | ") {
				bodies++
				pending = false
			}
		}
	}
	return fmt.Sprintf("items with queries %d; queries run %d; distinct candidate locations %d; bodies shown %d", withQueries, queries, len(distinct), bodies)
}
