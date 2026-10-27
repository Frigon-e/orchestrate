package main

import (
	"io"
	"strings"
	"testing"
)

func TestJbcontextBuffersRemainBoundedThroughIOCopy(t *testing.T) {
	b := ctxBoundedBuffer{limit: 32}
	n, err := io.Copy(&b, strings.NewReader(strings.Repeat("x", 10000)))
	if err != nil || n != 10000 || b.Len() != 32 || !b.cut {
		t.Fatalf("unbounded output: copied=%d retained=%d cut=%v err=%v", n, b.Len(), b.cut, err)
	}
}

func TestJbcontextMalformedTailCannotBecomeEmpty(t *testing.T) {
	for _, raw := range []string{`{"type":"search_result","results":[]} garbage`, `{"type":"search_result"}`, `{"type":"error"}`, ``} {
		if _, err := parseJbcontext([]byte(raw)); err == nil {
			t.Fatalf("malformed result accepted: %s", raw)
		}
	}
}

func TestConceptViewRelocatesUniqueAnchorsAndRejectsAmbiguity(t *testing.T) {
	c := newCodeHarness(t)
	writeTree(t, c.repo, map[string]string{"shop/current.py": "# inserted\n\ndef total():\n    return 2\n", "web/ambiguous.ts": "const x = 1;\nconst same = true;\nconst same = true;\n"})
	e := newCodeEnv(c.repo, codeOpts{})
	h, ok := e.checkoutHit(codeHit{Path: "shop/current.py", Start: 1, End: 2, Body: []string{"def total():", "    return 1"}})
	if !ok || h.Start != 3 || strings.Contains(strings.Join(h.Body, "\n"), "return 1") || !strings.Contains(strings.Join(h.Body, "\n"), "return 2") {
		t.Fatalf("relocation: %+v %v", h, ok)
	}
	if _, ok := e.checkoutHit(codeHit{Path: "web/ambiguous.ts", Start: 1, End: 1, Body: []string{"const same = true;"}}); ok {
		t.Fatal("ambiguous semantic location accepted")
	}
	if ctxPathInScope("src2/main.ts", "src") || !ctxPathInScope("src/main.ts", "src/") {
		t.Fatal("scope is not component aware")
	}
}
