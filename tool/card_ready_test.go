package main

import (
	"strings"
	"testing"
)

func TestCardReady(t *testing.T) {
	cases := []struct{ name, src, status, missing string }{
		{"legacy Python", readyAskCard, "ready", ""},
		{"legacy TypeScript", readyTSCard, "ready", ""},
		{"context", strings.Replace(readyAskCard, "## Context", "## Background", 1), "missing_context", "Context"},
		{"placeholder context", "---\nquestion: Q?\n---\n## Context\nTBD\n", "missing_context", "Context"},
		{"no effects", strings.ReplaceAll(strings.ReplaceAll(readyAskCard, "Gain:", "Benefit"), "Cost:", "Expense"), "missing_tradeoffs", "option cancel"},
		{"missing cost", strings.Replace(readyAskCard, "Cost: Pending writes can stop before delivery.", "", 1), "missing_tradeoffs", "Cost:"},
		{"empty gain", strings.Replace(readyAskCard, "Gain: No work after the client disconnects.", "Gain: TBD", 1), "missing_tradeoffs", "Gain:"},
		{"no options", strings.Replace(readyAskCard, "## Options", "## Possibilities", 1), "unclear", "not an option"},
		{"free text", "---\nquestion: Q?\n---\n## Context\nThe API contract is not set.\n", "missing_tradeoffs", "two options"},
		{"recommendation", strings.Replace(readyAskCard, "recommend: cancel\n", "", 1), "missing_evidence", "recommend:"},
		{"action", strings.Split(readyAskCard, "## Next")[0], "missing_evidence", "action:"},
		{"explicit owner", strings.Replace(strings.Split(readyAskCard, "## Next")[0], "route: owner", "owner: API lead", 1), "ready", ""},
		{"explicit action", strings.Replace(strings.Split(readyAskCard, "## Next")[0], "route: owner", "action: API lead tests the chosen contract", 1), "ready", ""},
		{"invalid card", "# Question\n", "unclear", "front matter"},
		{"stale ready", strings.Replace(strings.Replace(readyAskCard, "route: owner", "route: owner\nready: ready", 1), "## Context", "## Background", 1), "missing_context", "Context"},
		{"recorded research gap", strings.Replace(readyAskCard, "route: owner", "ready: missing_evidence", 1), "missing_evidence", "research gap"},
		{"recorded context gap", strings.Replace(readyAskCard, "route: owner", "ready: missing_context", 1), "missing_context", "research gap"},
		{"recorded tradeoff gap", strings.Replace(readyAskCard, "route: owner", "ready: missing_tradeoffs", 1), "missing_tradeoffs", "research gap"},
		{"recorded failure", strings.Replace(readyAskCard, "route: owner", "ready: failed_retrieval", 1), "failed_retrieval", "recorded retrieval failure"},
		{"parseable uncertainty", strings.Replace(readyAskCard, "route: owner", "ready: unclear", 1), "missing_evidence", "preparation uncertainty"},
		{"search failure", readyAskCard + "\n## Review evidence\nCode search failed: service timed out.\n", "failed_retrieval", "service timed out"},
		{"search disabled", readyAskCard + "\n## Review evidence\nCode search did not run: index missing.\n", "failed_retrieval", "index missing"},
		{"search skipped", readyAskCard + "\n## Review evidence\nCode search skipped after earlier search failures.\n", "failed_retrieval", "skipped"},
		{"failed before structural gaps", "---\nquestion: Q?\n---\n## Review evidence\nCode search failed: offline.\n", "failed_retrieval", "offline"},
		{"code example is not a failure", readyAskCard + "\n## Example\n```python\nprint('Code search failed: offline')\n```\n", "ready", ""},
		{"markdown effect labels", strings.ReplaceAll(strings.ReplaceAll(readyAskCard, "Gain:", "- **Gain:**"), "Cost:", "- **Cost:**"), "ready", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			result := checkCardReady(tt.src)
			if result.Status != tt.status {
				t.Fatalf("got %+v, want %s", result, tt.status)
			}
			if !strings.Contains(strings.Join(result.Missing, "; "), tt.missing) {
				t.Errorf("missing %q in %+v", tt.missing, result)
			}
		})
	}
}

func TestReadyIndexWarnings(t *testing.T) {
	src := "# Gate questions\n\nNote: code search did not run: index unavailable\nRetry before choosing.\n\n| card | tag |\n|a.md|owner|\n\n## Warnings\nRetrieval failed: backend down\n\nAnother warning.\n\n## Cards\nNot a warning.\n"
	warnings := cardIndexWarnings(src)
	got := strings.Join(warnings, "\n")
	wantContains(t, got, "index unavailable", "Retry before choosing.", "backend down", "Another warning.")
	if len(warnings) != 3 || strings.Contains(got, "Not a warning") || strings.Contains(got, "a.md") {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}
}
