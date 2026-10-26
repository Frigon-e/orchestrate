package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const readyAskCard = `---
question: Keep Python task cancellation on disconnect?
recommend: cancel
change: hard
route: owner
---
## Context
The Python API uses asyncio.TaskGroup. Disconnect cancellation stops sibling tasks.
Checked api/stream.py and its disconnect test. This changes the product's delivery guarantee.
## Options
### cancel: Cancel the task group
Gain: No work after the client disconnects.
Cost: Pending writes can stop before delivery.
### finish: Complete pending writes
Gain: Accepted writes can finish.
Cost: The Python tasks need an explicit lifetime outside TaskGroup.
## Next
The API lead will implement the chosen disconnect contract and test cancellation.
`

const readyTSCard = `---
question: Use a discriminated union for TypeScript event handlers?
recommend: union
route: lead-may-decide
owner: client lead
---
## Context
The Authority delegates internal representations to the client lead.
The TypeScript boundary validates JSON before src/events.ts handles events.
The exhaustiveness test uses never to reject an unhandled union member.
## Options
### union: Use a discriminated union
Gain: TypeScript checks exhaustive event handling at compile time.
Cost: Each new event kind needs a union member and a handler.
### object: Use one object with optional fields
Gain: New payload fields fit the same object type.
Cost: TypeScript cannot narrow payload fields by event kind.
`

func TestAskKeepsImagesWithTheSameBasename(t *testing.T) {
	h := setup(t)
	qd := filepath.Join(h.tmp, "topic", "questions")
	for _, dir := range []string{qd, filepath.Join(qd, "a"), filepath.Join(qd, "b")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	card := strings.Replace(readyAskCard, "## Context\n", "## Context\ninclude: a/chart.png First chart\n\ninclude: b/chart.png Second chart\n\n", 1)
	writeFiles(t, qd, map[string]string{
		"charts.md":   card,
		"a/chart.png": "first image bytes",
		"b/chart.png": "second image bytes",
	})
	h.must("ask", "--cards", qd)
	out := filepath.Join(filepath.Dir(qd), "ask")
	page := string(mustRead(t, filepath.Join(out, "index.html")))
	images := regexp.MustCompile(`<img src="([^"]+)" alt="(?:First|Second) chart">`).FindAllStringSubmatch(page, -1)
	if len(images) != 2 || images[0][1] == images[1][1] {
		t.Fatalf("distinct image includes need distinct targets: %v", images)
	}
	for i, want := range []string{"first image bytes", "second image bytes"} {
		if got := string(mustRead(t, filepath.Join(out, images[i][1]))); got != want {
			t.Errorf("image %d = %q, want %q", i, got, want)
		}
	}
}

func TestAskCardsReadinessRouting(t *testing.T) {
	h := setup(t)
	topic := filepath.Join(h.tmp, "topic")
	qd := filepath.Join(topic, "questions")
	if err := os.MkdirAll(filepath.Join(qd, "answered"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, qd, map[string]string{
		"owner.md":            readyAskCard, // An old card without ready: must still work.
		"lead.md":             readyTSCard,
		"gap.md":              strings.Replace(readyAskCard, "Cost: Pending writes can stop before delivery.", "", 1),
		"failed.md":           readyAskCard + "\n## Review evidence\nCode search failed: backend unavailable.\n",
		"INDEX.md":            "# Gate questions\n\nNote: no code hints for some questions: backend unavailable\n",
		"answered/history.md": "Archived answer, do not change.\n",
	})
	out := h.must("ask", "--cards", qd)
	b, err := os.ReadFile(filepath.Join(topic, "ask", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	wantContains(t, out, "questions: 1\n", "cards: 4\n", "settled_by_lead: 1\n", "not_ready: 2\n", "lead,ready", "gap,missing_tradeoffs", "failed,failed_retrieval", "INDEX.md", "backend unavailable")
	wantContains(t, page, `data-lavish-question="owner"`, "asyncio.TaskGroup")
	for _, key := range []string{"lead", "gap", "failed", "history"} {
		if strings.Contains(page, `data-lavish-question="`+key+`"`) {
			t.Errorf("%s was asked as complete", key)
		}
	}
	before, err := os.ReadFile(filepath.Join(qd, "answered", "history.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != "Archived answer, do not change.\n" {
		t.Fatal("answered history changed")
	}
	// Outside a run, the caller archives only the answered card. Others remain open.
	if err := os.Rename(filepath.Join(qd, "owner.md"), filepath.Join(qd, "answered", "owner.md")); err != nil {
		t.Fatal(err)
	}
	out = h.must("ask", "--cards", qd)
	wantContains(t, out, "cards: 3\n", "questions: 0\n", "settled_by_lead: 1\n", "not_ready: 2\n", "gap,missing_tradeoffs")
	after, err := os.ReadFile(filepath.Join(qd, "answered", "history.md"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("answered history changed: %v", err)
	}
}

func TestAskCardsConservativeRoutesAndPartialAnswers(t *testing.T) {
	h := setup(t)
	qd := filepath.Join(h.tmp, "topic", "questions")
	if err := os.MkdirAll(filepath.Join(qd, "answered"), 0o755); err != nil {
		t.Fatal(err)
	}
	cards := map[string]string{
		"a-owner.md":     readyAskCard,
		"b-unclear.md":   strings.Replace(readyAskCard, "route: owner", "route: unclear", 1),
		"c-legacy.md":    strings.Replace(readyAskCard, "route: owner\n", "", 1),
		"d-unknown.md":   strings.Replace(readyAskCard, "route: owner", "route: other", 1),
		"e-malformed.md": "# Not a parseable card\n",
		"f-lead-gap.md":  strings.Replace(readyTSCard, "Gain: TypeScript checks exhaustive event handling at compile time.", "", 1),
	}
	writeFiles(t, qd, cards)
	out := h.must("ask", "--cards", qd)
	wantContains(t, out, "questions: 4\n", "cards: 6\n", "settled_by_lead: 0\n", "not_ready: 2\n", "e-malformed,unclear", "f-lead-gap,missing_tradeoffs")
	path := filepath.Join(filepath.Dir(qd), "ask", "index.html")
	page := string(mustRead(t, path))
	last := -1
	for _, key := range []string{"a-owner", "b-unclear", "c-legacy", "d-unknown"} {
		at := strings.Index(page, `data-lavish-question="`+key+`"`)
		if at < 0 || at <= last {
			t.Errorf("%s missing or out of filename order", key)
		}
		last = at
	}
	if strings.Count(page, `data-lavish-question=`) != 4 {
		t.Fatal("withheld cards received answer forms")
	}
	for name, src := range cards {
		if got := string(mustRead(t, filepath.Join(qd, name))); got != src {
			t.Errorf("ask rewrote %s", name)
		}
	}
	// Archive just this answer, then verify the other user questions remain open.
	if err := os.Rename(filepath.Join(qd, "b-unclear.md"), filepath.Join(qd, "answered", "b-unclear.md")); err != nil {
		t.Fatal(err)
	}
	out = h.must("ask", "--cards", qd)
	wantContains(t, out, "questions: 3\n", "cards: 5\n", "not_ready: 2\n")
	page = string(mustRead(t, path))
	for _, key := range []string{"a-owner", "c-legacy", "d-unknown"} {
		wantContains(t, page, `data-lavish-question="`+key+`"`)
	}
	if strings.Contains(page, `data-lavish-question="b-unclear"`) {
		t.Error("answered card reopened")
	}
	if string(mustRead(t, filepath.Join(qd, "answered", "b-unclear.md"))) != cards["b-unclear.md"] {
		t.Error("answered card changed")
	}
}
