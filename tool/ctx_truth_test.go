package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func truthResult(t *testing.T, out string) (ctxView, string) {
	t.Helper()
	var env struct {
		Result ctxView `json:"result"`
		Status string  `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err, out)
	}
	return env.Result, env.Status
}

func TestCtxTextMalformedBackendIsNotEmpty(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			c := newCtxHarness(t)
			script := "#!/bin/sh\n"
			if partial {
				script += "printf '%s\\n' '{\"type\":\"match\",\"data\":{\"path\":{\"text\":\"pay/pay.go\"},\"lines\":{\"text\":\"needle\"},\"line_number\":1}}'\n"
			}
			script += "printf '%s\\n' '{broken'\n"
			os.WriteFile(os.Getenv("ORCH_RG"), []byte(script), 0755)
			code, out := c.run("ctx", "text", "needle", "--json")
			v, _ := truthResult(t, out)
			if partial {
				if code != 0 || v.Status != "partial" || v.CountKind != "lower_bound" || v.Total != 1 {
					t.Fatalf("%d %s", code, out)
				}
			} else if code != 1 || v.Status != "failed" || v.CountKind != "unknown" {
				t.Fatalf("%d %s", code, out)
			}
			if v.Zero != "" || len(v.Errors) == 0 {
				t.Fatalf("backend failure looks empty: %s", out)
			}
			// A failed response must not be cached as a successful zero.
			os.WriteFile(os.Getenv("ORCH_RG"), []byte("#!/bin/sh\nexit 1\n"), 0755)
			code, out = c.run("ctx", "text", "needle", "--json")
			v, _ = truthResult(t, out)
			if code != 0 || v.Status != "empty" {
				t.Fatalf("failure cached: %d %s", code, out)
			}
		})
	}
}

func TestCtxBatchTruthfulStatusesAndJSONErrors(t *testing.T) {
	c := newCtxHarness(t)
	for _, tc := range []struct {
		args   []string
		code   int
		status string
	}{
		{[]string{"ctx", "read", "missing.go", "--json"}, 1, "failed"},
		{[]string{"ctx", "batch", "-q", "read:missing.go", "-q", "read:also-missing.go", "--json"}, 1, "failed"},
		{[]string{"ctx", "batch", "-q", "read:missing.go", "-q", "text:StartCheckout", "--json"}, 0, "partial"},
		{[]string{"ctx", "batch", "-q", "text:zzzNone", "--json"}, 0, "empty"},
		{[]string{"ctx", "text", "x", "--bogus", "--json"}, 2, "failed"},
	} {
		code, out := c.run(tc.args...)
		var env map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatal(err, out)
		}
		var status string
		_ = json.Unmarshal(env["status"], &status)
		if status == "" {
			var v ctxView
			_ = json.Unmarshal(env["result"], &v)
			status = v.Status
		}
		if code != tc.code || status != tc.status {
			t.Fatalf("%v: %d %s", tc.args, code, out)
		}
		if strings.Contains(strings.Join(tc.args, " "), "batch") && len(env["result"]) > 0 && env["result"][0] != '[' {
			t.Fatal("result must remain an array", out)
		}
	}
}

func TestCtxBatchRejectsIrrelevantOptionsBeforeDependencies(t *testing.T) {
	for _, q := range []string{"find:x fixed=false", "symbol:x all=false", "read:pay/pay.go tests=none", "text:x from=0", "text:x limit=0"} {
		t.Run(q, func(t *testing.T) {
			c := newCtxHarness(t)
			code, out := c.run("ctx", "batch", "-q", q)
			if code != 2 {
				t.Fatalf("irrelevant option accepted: %d %s", code, out)
			}
			if c.log(c.rgCalls) != "" || c.log(c.jbCalls) != "" || c.log(c.gCall) != "" {
				t.Fatal("dependency ran before validation")
			}
		})
	}
	c := newCtxHarness(t)
	c.stdin.WriteString(`{"lookups":[{"kind":"concept","query":"x","fixed":false}]}`)
	code, out := c.run("ctx", "batch", "-", "--json")
	if code != 2 || !strings.Contains(out, "fixed") {
		t.Fatalf("irrelevant JSON false accepted: %d %s", code, out)
	}
	if c.log(c.jbCalls) != "" {
		t.Fatal("dependency ran")
	}
}

func TestConceptViewAlwaysRendersCurrentCheckoutBody(t *testing.T) {
	for _, tc := range []struct{ path, old, current string }{
		{"shop/cart.py", "def total():\n    return 1", "def total():\n    return 2"},
		{"web/cart.ts", "export function total() {\n  return 1;\n}", "export function total() {\n  return 2;\n}"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			c := newCtxHarness(t)
			os.MkdirAll(filepath.Dir(filepath.Join(c.repo, tc.path)), 0755)
			os.WriteFile(filepath.Join(c.repo, tc.path), []byte(tc.current+"\n"), 0644)
			record := map[string]any{"type": "search_result", "results": []any{map[string]any{"result": map[string]any{"sourcePosition": map[string]any{"relativePath": tc.path}}, "content": tc.old, "contentStartLine": 1}}}
			b, _ := json.Marshal(record)
			script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\nstatus) echo '{\"type\":\"status_result\",\"indices\":[]}';;\nsearch) printf '%%s\\n' '%s';;\nesac\n", b)
			os.WriteFile(os.Getenv("ORCH_JBCONTEXT"), []byte(script), 0755)
			code, out := c.run("ctx", "find", "Code that computes totals", "--json")
			if code != 0 || strings.Contains(out, "return 1") || !strings.Contains(out, "return 2") {
				t.Fatalf("stale body rendered: %d %s", code, out)
			}
		})
	}
}

func TestJbcontextBuffersRemainBoundedThroughIOCopy(t *testing.T) {
	b := ctxBoundedBuffer{limit: 32}
	n, err := io.Copy(&b, strings.NewReader(strings.Repeat("x", 10000)))
	if err != nil || n != 10000 || b.Len() != 32 || !b.cut {
		t.Fatalf("unbounded output: copied=%d retained=%d cut=%v err=%v", n, b.Len(), b.cut, err)
	}
}

func TestCtxBatchPartialDataIsNotCountedAsFailed(t *testing.T) {
	v := &ctxView{Kind: "text", Query: "x", Total: 1, Shown: 1, Status: "partial", CountKind: "lower_bound", Err: "malformed tail"}
	v.part(0).Raw = []string{"successful data"}
	out := renderBatch([]*ctxView{v}, ctxFmt{})
	if !strings.Contains(out, "1 lookups, 1 with results, 0 empty, 0 failed") || !strings.Contains(out, "status: partial") || !strings.Contains(out, "successful data") {
		t.Fatal(out)
	}
}

func TestCtxTextCountRowsAreCountedBeyondRetention(t *testing.T) {
	c := newCtxHarness(t)
	e := newCtxEnv(c.repo, ctxOpts{})
	var raw strings.Builder
	for i := range 2105 {
		fmt.Fprintf(&raw, "src/f%04d.ts:2\n", i)
	}
	q := rgQuery{Pattern: "needle", Count: true, ViewFilters: true}
	res, err := e.streamRG(strings.NewReader(raw.String()), q)
	if err != nil || res.Total != 4210 || res.Rows != 2105 || len(res.Counts) != ctxRgMax {
		t.Fatalf("total=%d rows=%d retained=%d err=%v", res.Total, res.Rows, len(res.Counts), err)
	}
	v := e.textView(q, "", res, 5, 1, 0, ctxFmt{})
	if v.Total != 4210 || v.Shown != 10 || !strings.Contains(v.Pager, "of 2105") {
		t.Fatalf("%+v", v)
	}
}

func TestCtxBatchAvailabilityErrorsCanNameDefinitions(t *testing.T) {
	v := &ctxView{Kind: "sym", Query: "Run", Errors: []string{"shop/cart.py:1 references unavailable", "web/cart.ts:3 implementations unavailable"}}
	v.part(0).Raw = []string{"definition preview"}
	v.finishTruth()
	if v.Status != "partial" || v.CountKind != "unknown" || len(v.Errors) != 2 || v.Zero != "" {
		t.Fatalf("availability metadata lost: %+v", v)
	}
}

func TestJbcontextMalformedTailCannotBecomeEmpty(t *testing.T) {
	for _, raw := range []string{`{"type":"search_result","results":[]} garbage`, `{"type":"search_result"}`, `{"type":"error"}`, ``} {
		if _, err := parseJbcontext([]byte(raw)); err == nil {
			t.Fatalf("malformed result accepted: %s", raw)
		}
	}
}

func TestCtxBatchManyLookupsDoNotNestSemaphore(t *testing.T) {
	c := newCtxHarness(t)
	args := []string{"ctx", "batch", "--json"}
	for range 18 {
		args = append(args, "-q", "symbol:StartCheckout")
	}
	done := make(chan struct{})
	var code int
	var out string
	go func() { code, out = c.run(args...); close(done) }()
	select {
	case <-done:
		if code != 0 {
			t.Fatalf("%d %s", code, out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("batch deadlocked with more lookups than semaphore slots")
	}
}

func TestCtxBatchLegacyFailuresAreErrors(t *testing.T) {
	c := newCtxHarness(t)
	e := newCtxEnv(c.repo, ctxOpts{})
	e.RG = filepath.Join(t.TempDir(), "missing-rg")
	got := e.Batch([]ctxLookup{{Kind: "text", Query: "needle"}}, nil, 1000)
	if len(got) != 1 || !strings.HasPrefix(got[0].Output, "error:") || strings.Contains(got[0].Output, "no result") {
		t.Fatalf("%+v", got)
	}
}

// Exercise the same command boundary and checkout an agent uses, not a parser
// alone: excluded Python tests sort before the TypeScript production match.
func TestCtxTextRetentionCapCountsIndependentlyAndDoesNotCache(t *testing.T) {
	c := newCtxHarness(t)
	t.Setenv("ORCH_RG", realRG(t))
	writeTree(t, c.repo, map[string]string{"src/main.ts": strings.Repeat("export const needle = true;\n", 2105)})
	e := newCtxEnv(c.repo, ctxOpts{})
	res, err := e.Text(rgQuery{Pattern: "needle", ViewFilters: true})
	if err != nil || res.Total != 2105 || len(res.Hits) != ctxRgMax || !res.Limited {
		t.Fatalf("count=%d kept=%d limited=%v err=%v", res.Total, len(res.Hits), res.Limited, err)
	}
	caches, err := filepath.Glob(filepath.Join(e.CacheDir, "rg3-*.json"))
	if err != nil || len(caches) != 0 {
		t.Fatalf("truncated results cached: %v %v", caches, err)
	}
	code, out := c.run("ctx", "text", "needle", "--page", "401", "--json")
	v, _ := truthResult(t, out)
	if code != 0 || v.Status != "partial" || v.CountKind != "exact" || v.Total != 2105 || v.Zero != "" || !strings.Contains(v.Cut, "2000") || !strings.Contains(v.Head, "beyond retained") {
		t.Fatalf("cap presented as exhaustion: %d %s", code, out)
	}
}

func TestCtxTextIncompleteStreamHasNoDefinitiveEmpty(t *testing.T) {
	c := newCtxHarness(t)
	if err := os.WriteFile(os.Getenv("ORCH_RG"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"begin\",\"data\":{}}'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	code, out := c.run("ctx", "text", "needle", "--json")
	v, _ := truthResult(t, out)
	if code != 1 || v.Status != "failed" || v.Zero != "" || !strings.Contains(v.Err, "missing summary") {
		t.Fatalf("incomplete stream looks empty: %d %s", code, out)
	}
}

func TestConceptViewRelocatesUniqueAnchorsAndRejectsAmbiguity(t *testing.T) {
	c := newCtxHarness(t)
	writeTree(t, c.repo, map[string]string{"shop/current.py": "# inserted\n\ndef total():\n    return 2\n", "web/ambiguous.ts": "const x = 1;\nconst same = true;\nconst same = true;\n"})
	e := newCtxEnv(c.repo, ctxOpts{})
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

func TestCtxTextExcludedHitsCannotHideProduction(t *testing.T) {
	c := newCtxHarness(t)
	t.Setenv("ORCH_RG", realRG(t))
	writeTree(t, c.repo, map[string]string{
		"aaa/__tests__/test_many.py": strings.Repeat("needle = True\n", 2100),
		"bbb/header.ts":              "// @generated\n" + strings.Repeat("export const needle = true;\n", 2100),
		"zzz/main.ts":                "export const needle = true;\n",
	})
	code, out := c.run("ctx", "text", "needle", "--json")
	var env struct {
		Result ctxView `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err, out)
	}
	if code != 0 || env.Result.Total != 1 || env.Result.Hidden != 2100 || env.Result.HiddenG != 2100 || !strings.Contains(out, "zzz/main.ts") {
		t.Fatalf("production hit hidden: exit %d %s", code, out)
	}
}
