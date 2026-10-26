package main

import "strings"

// How to write a concept query. It is shown where agents look: the top of
// `orch ctx find --help`, the home view, a tip under a weak `find` answer, the
// planner prompt and the docs. Queries agents type are never rewritten.

// Shared by the embedded review notice and concept help so retrieval limits
// and recovery advice cannot drift between those entry points.
const ctxResultGuide = `DEFAULT TO BATCH. Put independent lookups in ONE call with orch ctx batch; inspect source with native Read/read, not retrieval snippets. A single lookup is fine for one question. No blanket speed advantage is assumed.
RESULTS. Check status (ok, empty, partial, failed), count_kind (exact, lower_bound, unknown), scope, exclusions, errors, cuts and sources. An exact count can exceed retained excerpts. Semantic candidates never establish absence; empty text means no pattern match only within its reported scope. Symbols distinguish exact identity from labelled candidates; by-name matches are not a call graph.
FAILURES. A failed single lookup or all-failed batch exits 1; a partial batch exits 0 with status: partial and preserves usable results. Usage errors exit 2. Missing servers or failed references are partial/failed data, not zero callers. Use the named external-server install hint, scoped text/native search, or native reads.
CAPS. Text retains 2000 excerpts; semantic retrieval is bounded to 2000 raw ranks; symbols display four definitions after filters. Narrow glob/path/in/kind or read the checkout when capped. --full raises finite display caps (default 1000 text hits/page, 600 columns, 48000 characters/lookup, 280000/batch), not backend limits. --all broadens text globs but keeps gitignore/hidden rules and the tests filter.
TRUST. Repo-local servers need user-config ctx_trust = ["/absolute/repo"] outside the installed-skill repo; ctx_deny wins. Read-only mode never runs repo-local servers, even if trusted.`

const ctxQueryGuide = "guide: When you do not know where code lives, search one behavior at a time in plain language:\n" +
	"  `Code that <action> on <entity>, when <trigger>, producing <result>`. Include only what you know.\n" +
	"  Do not invent symbols, frameworks or paths. Read the best hits, adopt the repo's own names, then use `sym` / `text` for exact lookups.\n" +
	"  Weak: `authentication`, `upload service controller queue`. Better: `Code that checks a user's credentials and establishes a signed-in session.`\n" +
	"  Read files with your native read tool (offset and limit for part of a file); orch ctx is for finding things, not for reading them; never use sed, cat, head or tail.\n" + ctxResultGuide

const conceptTipText = `tip: describe the behavior: "Code that <action> <entity> ..." (one behavior per query)`

var behaviorWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`shows checks creates sends stores handles returns lists validates builds starts restores loads saves reads writes updates deletes renders fetches parses computes calculates emits publishes finds gets sets adds removes retries signs issues authenticates uploads downloads converts resolves verifies applies calls triggers when that which whether establishes`) {
		behaviorWords[w] = true
	}
}

// conceptTip reports whether a concept answer should carry the tip: the query
// is a short noun list, or the answer is weak (at most one hit). A query that
// already starts "Code that" or has six or more words never does.
func conceptTip(query string, hits int) bool {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 || len(words) >= 6 || strings.HasPrefix(strings.Join(words, " "), "code that") {
		return false
	}
	if hits <= 1 {
		return true
	}
	if len(words) > 4 {
		return false
	}
	for _, w := range words {
		if behaviorWords[strings.Trim(w, ".,;:!?\"'`")] {
			return false
		}
	}
	return true
}
