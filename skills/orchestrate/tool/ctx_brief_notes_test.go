package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestBriefSearchNotesGroupFailuresByReason(t *testing.T) {
	var lks []evidenceLookup
	for i := range 6 {
		lks = append(lks, lookupRecord(fmt.Sprintf("q%d", i), ".", "concept", 0, 4, fmt.Errorf("%s", score429), ""))
	}
	lks = append(lks, lookupRecord("ok", ".", "text", 3, 0, nil, ""), lookupRecord("part", ".", "concept", 2, 4, nil, ""))
	got := searchNotes(lks, nil, "/work/repo")
	wantContains(t, got, "### Search notes\n", "Lookups: 8 run, 1 ok, 1 partial, 6 failed, 0 unavailable.", "6 lookups failed: Exception during request to https://api.jetbrains.ai/user/v5/indexing/search with code 429: Limit reached")
	if n := strings.Count(got, "\n"); n != 3 {
		t.Errorf("notes are %d lines, want 3:\n%s", n, got)
	}
	if searchNotes(nil, nil, "") != "" {
		t.Error("notes with nothing to say")
	}
}
