package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPageFooterWordingNarrowingAndEnd(t *testing.T) {
	cases := []struct {
		lo, hi, total, limit, page int
		atLeast                    bool
		want                       string
	}{
		{0, 6, 40, 6, 1, false, "showing 1-6 of 40; next: --page 2"},
		{6, 12, 40, 6, 2, false, "showing 7-12 of 40; next: --page 3"},
		{0, 10, 80, 10, 1, false, "showing 1-10 of 80; 70 more; narrow with X, or --page 2"},
		{30, 40, 80, 10, 4, false, "showing 31-40 of 80; 40 more; narrow with X"},
		{30, 40, 40, 10, 4, false, "showing 31-40 of 40"},
		{18, 24, 40, 6, 4, false, "showing 19-24 of 40; narrow with X"},
		{4, 8, 8, 4, 2, true, "showing 5-8 of at least 8; next: --page 3"},
		{0, 5, 5, 10, 1, false, ""},
	}
	for _, c := range cases {
		if got := pageFooter(c.lo, c.hi, c.total, c.page, c.atLeast, false, "X"); got != c.want {
			t.Errorf("pageFooter(%+v) = %q, want %q", c, got, c.want)
		}
	}
	if got := pastEnd(40, 5, 9, "hits"); got != "page 9 is past the end (40 hits, 8 pages)" {
		t.Errorf("pastEnd = %q", got)
	}
}

func fakeHits(n int) rgResult {
	var res rgResult
	for i := range n {
		res.Hits = append(res.Hits, rgHit{Path: fmt.Sprintf("p/f%02d.go", i/3), Line: i%3 + 1, Text: "needle"})
	}
	return res
}

// locsOf lists the "path:line" of the match rows of a view.
func locsOf(v *ctxView) []string {
	var out []string
	for _, p := range v.Parts {
		for _, f := range p.Files {
			for _, l := range f.Lines {
				if l.Match {
					out = append(out, fmt.Sprintf("%s:%d", f.Path, l.N))
				}
			}
		}
	}
	return out
}

func TestTextPagesAreDisjointStableAndCoverTheList(t *testing.T) {
	dir := t.TempDir()
	for i := range 10 {
		os.MkdirAll(filepath.Join(dir, "p"), 0o755)
		os.WriteFile(filepath.Join(dir, "p", fmt.Sprintf("f%02d.go", i)), []byte("needle\nneedle\nneedle\n"), 0o644)
	}
	e := &ctxEnv{Code: dir}
	res := fakeHits(25)
	seen := map[string]int{}
	var order []string
	for page := 1; page <= 3; page++ {
		v := e.textView(rgQuery{Pattern: "needle"}, "", res, 10, page, 0, ctxFmt{})
		again := e.textView(rgQuery{Pattern: "needle"}, "", res, 10, page, 0, ctxFmt{})
		got := locsOf(v)
		if strings.Join(got, ",") != strings.Join(locsOf(again), ",") {
			t.Errorf("page %d is not stable", page)
		}
		for _, l := range got {
			seen[l]++
			order = append(order, l)
		}
		if v.Total != 25 {
			t.Errorf("page %d total %d", page, v.Total)
		}
		if page == 1 {
			want := "showing 1-10 of 25; next: --page 2; left out: p/f03.go (2), p/f04.go (3), p/f05.go (3), p/f06.go (3), p/f07.go (3), +1 more files"
			if v.Pager != want {
				t.Errorf("first-page footer = %q, want %q", v.Pager, want)
			}
		}
		if page == 3 && strings.Contains(v.Pager, "left out") {
			t.Errorf("the last page left nothing out: %q", v.Pager)
		}
	}
	if len(seen) != 25 || len(order) != 25 {
		t.Fatalf("pages cover %d distinct of %d rows", len(seen), len(order))
	}
	for l, n := range seen {
		if n != 1 {
			t.Errorf("%s on %d pages", l, n)
		}
	}
	for i := 1; i < len(order); i++ {
		pi, li := splitWhere(order[i-1])
		pj, lj := splitWhere(order[i])
		if pi > pj || (pi == pj && li >= lj) {
			t.Errorf("rows out of path/line order: %s then %s", order[i-1], order[i])
		}
	}
	v := e.textView(rgQuery{Pattern: "needle"}, "", res, 10, 4, 0, ctxFmt{})
	if v.Zero != "page 4 is past the end (25 hits, 3 pages)" {
		t.Errorf("past the end: %q", v.Zero)
	}
	if v := e.textView(rgQuery{Pattern: "needle"}, "", res, 10, 3, 0, ctxFmt{}); v.Pager != "showing 21-25 of 25" || v.Shown != 5 {
		t.Errorf("last page: %q shown %d", v.Pager, v.Shown)
	}
	// Above 50 hits narrowing comes first, and pages stop being suggested after page 3.
	big := fakeHits(60)
	if v := e.textView(rgQuery{Pattern: "needle"}, "", big, 10, 2, 0, ctxFmt{}); v.Pager != "showing 11-20 of 60; 40 more; narrow with --glob/--word/--tests/-p, or --page 3; left out: p/f06.go (1), p/f07.go (3), p/f08.go (3), p/f09.go (3), p/f10.go (3), +9 more files" {
		t.Errorf("narrowing hint: %q", v.Pager)
	}
	if v := e.textView(rgQuery{Pattern: "needle"}, "", big, 10, 5, 0, ctxFmt{}); strings.Contains(v.Pager, "--page") || !strings.Contains(v.Pager, "narrow with") {
		t.Errorf("page 5 still suggests pages: %q", v.Pager)
	}
}

func TestCtxTextPageFlagsAndErrors(t *testing.T) {
	c := newCtxHarness(t)
	p1 := c.must("ctx", "text", "StartCheckout", "--limit", "1", "-C", "0")
	p2 := c.must("ctx", "text", "StartCheckout", "--limit", "1", "--page", "2", "-C", "0")
	wantContains(t, ctxTestStrings(t, p1), "showing 1-1 of 2; next: --page 2")
	wantContains(t, ctxTestStrings(t, p2), "showing 2-2 of 2")
	if one, two := ctxTestSourceLines(t, p1, "pay/pay.go"), ctxTestSourceLines(t, p2, "pay/pay.go"); len(one) != 1 || one[4] == nil || len(two) != 1 || two[7] == nil {
		t.Fatalf("page source overlap: %#v %#v", one, two)
	}
	alias := c.must("ctx", "text", "StartCheckout", "--max", "1", "--page", "2", "-C", "0")
	if lines := ctxTestSourceLines(t, alias, "pay/pay.go"); len(lines) != 1 || lines[7] == nil {
		t.Fatalf("max alias page=%#v", lines)
	}
	wantContains(t, c.must("ctx", "text", "StartCheckout", "--limit", "1", "--page", "3"), "page 3 is past the end (2 hits, 2 pages)")
	for _, args := range [][]string{{"ctx", "text", "x", "--page", "0"}, {"ctx", "text", "x", "--limit", "0"}, {"ctx", "find", "x", "--page", "0"}, {"ctx", "sym", "StartCheckout", "--limit", "-1"}} {
		if code, out := c.run(args...); code != 2 || !strings.Contains(out, "error: ") || !strings.Contains(out, "at least 1") {
			t.Errorf("%v: exit %d %s", args, code, out)
		}
	}
}

func TestCtxBatchPerLookupLimitAndPage(t *testing.T) {
	c := newCtxHarness(t)
	out := c.must("ctx", "batch", "-q", "text:StartCheckout limit=1 page=2 context=0", "-q", "text:StartCheckout limit=1 context=0")
	wantContains(t, out, "showing 2-2 of 2", "showing 1-1 of 2; next: page=2")
	c.stdin.WriteString(`{"lookups":[{"kind":"text","query":"StartCheckout","limit":1,"page":2,"context":0}]}`)
	fromPlan := c.must("ctx", "batch", "-")
	wantContains(t, ctxTestStrings(t, fromPlan), "showing 2-2 of 2")
	if lines := ctxTestSourceLines(t, fromPlan, "pay/pay.go"); len(lines) != 1 || lines[7] == nil {
		t.Fatalf("plan page=%#v", lines)
	}
	c.stdin.WriteString(`{"lookups":[{"kind":"text","query":"x","page":-1}]}`)
	if code, out := c.run("ctx", "batch", "-"); code != 2 || !strings.Contains(out, "at least 1") {
		t.Errorf("negative page: %d %s", code, out)
	}
	if code, out := c.run("ctx", "batch", "-q", "text:x page=0"); code != 2 || !strings.Contains(out, "page must be at least 1") {
		t.Errorf("page=0: %d %s", code, out)
	}
}

func TestCtxSymCallersPageAndImplementers(t *testing.T) {
	c := newCtxHarness(t)
	p2 := c.must("ctx", "sym", "StartCheckout", "--limit", "3", "--page", "2", "--impl=false")
	wantContains(t, ctxTestStrings(t, p2), "used by 8", "showing 4-6 of 8; next: --page 3")
	if lines := ctxTestSourceLines(t, p2, "svc/svc.go"); len(lines) != 3 || lines[4] == nil || lines[5] == nil || lines[6] == nil {
		t.Fatalf("callers page2=%#v", lines)
	}
	p3 := c.must("ctx", "sym", "StartCheckout", "--limit", "3", "--page", "3", "--impl=false")
	wantContains(t, ctxTestStrings(t, p3), "showing 7-8 of 8")
	if lines := ctxTestSourceLines(t, p3, "svc/svc.go"); len(lines) != 2 || lines[7] == nil || lines[8] == nil {
		t.Fatalf("callers page3=%#v", lines)
	}
	wantContains(t, c.must("ctx", "sym", "StartCheckout", "--limit", "3", "--page", "4", "--impl=false"), "page 4 is past the end (8 callers, 3 pages)")
	wantContains(t, c.must("ctx", "sym", "StartCheckout", "--limit", "1", "--page", "2"), "implemented by 1 - page 2 is past the end (1 implementers, 1 page)")
}

func TestCtxFindPagesSliceTheRankedListFromJbcontext(t *testing.T) {
	c := newCtxHarness(t)
	jb := filepath.Join(c.jbDir, "jbcontext")
	// The hits are for files that exist, each with its function on the line
	// the search reports: hits for files that are not in the checkout are dropped.
	for i := 1; i <= 11; i++ {
		src := strings.Repeat("\n", i-1) + fmt.Sprintf("func F%d() {}\n", i)
		os.WriteFile(filepath.Join(c.repo, "pay", fmt.Sprintf("f%02d.go", i)), []byte(src), 0o644)
	}
	c.git("add", ".")
	c.git("commit", "-q", "-m", "hits")
	head, _ := git(c.repo, "rev-parse", "HEAD")
	os.WriteFile(jb, []byte(`#!/bin/sh
echo "$*" >> "`+c.jbCalls+`"
case "$1" in
status) echo '{"type":"status_result","indices":[{"snapshots":[{"revision":"`+head+`","createdAt":1}]}]}';;
watch) printf 'Watch daemon running (pid 7)\n';;
search)
	lim=5
	while [ $# -gt 0 ]; do [ "$1" = --limit ] && lim=$2; shift; done
	n=11; [ "$lim" -lt "$n" ] && n=$lim
	printf '{"type":"search_result","results":['
	i=1; while [ $i -le $n ]; do
		[ $i -gt 1 ] && printf ','
		printf '{"result":{"sourcePosition":{"relativePath":"pay/f%02d.go"}},"content":"func F%d() {}","contentStartLine":%d}' $i $i $i
		i=$((i+1))
	done
	printf ']}\n';;
esac
`), 0o755)
	p1 := c.must("ctx", "find", "anything", "--limit", "4")
	wantContains(t, c.log(c.jbCalls), "--limit 12")
	wantContains(t, p1, "showing 1-4 of at least 11; next: --page 2; left out: pay/f05.go (1), pay/f06.go (1), pay/f07.go (1), pay/f08.go (1), pay/f09.go (1), +2 more files")
	if strings.Contains(p1, "f05.go\n") {
		t.Errorf("the left-out files are named, not shown:\n%s", p1)
	}
	p2 := c.must("ctx", "find", "anything", "--limit", "4", "--page", "2")
	wantContains(t, p2, "pay/f05.go", "pay/f08.go", "showing 5-8 of at least 11; next: --page 3; left out: pay/f09.go (1), pay/f10.go (1), pay/f11.go (1)")
	if strings.Contains(p2, "func F4()") || strings.Contains(p2, "func F9()") {
		t.Errorf("page 2 is not rows 5-8:\n%s", p2)
	}
	wantContains(t, c.log(c.jbCalls), "--limit 16")
	wantContains(t, c.must("ctx", "find", "anything", "--limit", "4", "--page", "3"), "pay/f09.go", "pay/f11.go", "showing 9-11 of 11")
	wantContains(t, c.must("ctx", "find", "anything", "--limit", "4", "--page", "4"), "page 4 is past the end (11 hits, 3 pages)")
	wantContains(t, c.must("ctx", "batch", "-q", "find:anything limit=4 page=2"), "f05.go")
}

func TestPageFooterInABatchSpeaksInQueryKeys(t *testing.T) {
	for _, c := range []struct {
		lo, hi, total, limit, page int
		narrow, want               string
	}{
		{0, 4, 31, 4, 1, narrowText, "showing 1-4 of 31; next: page=2"},
		{0, 10, 80, 10, 1, narrowText, "showing 1-10 of 80; 70 more; narrow with glob=/word=/tests=/path=, or page=2"},
		{30, 40, 80, 10, 4, narrowFind, "showing 31-40 of 80; 40 more; narrow with path=/tests="},
		{18, 24, 40, 6, 4, narrowSym, "showing 19-24 of 40; narrow with in=/def_kind=/tests="},
	} {
		if got := pageFooter(c.lo, c.hi, c.total, c.page, false, true, c.narrow); got != c.want {
			t.Errorf("batch footer = %q, want %q", got, c.want)
		}
	}
	if got := pageFooter(0, 4, 31, 1, false, false, narrowText); got != "showing 1-4 of 31; next: --page 2" {
		t.Errorf("a single call keeps the flag form: %q", got)
	}
}
