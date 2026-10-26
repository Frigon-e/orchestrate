package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ctxHarness is a repo with a committed Go file, a jbcontext, a gopls and an
// rg stand-in, each logging its calls, and an app running in that repo.
type ctxHarness struct {
	*harness
	head                    string
	jbCalls, rgCalls, gCall string
	jbDir                   string
}

func newCtxHarness(t *testing.T) *ctxHarness {
	t.Helper()
	h := setup(t)
	c := &ctxHarness{harness: h}
	repo := h.repo
	os.MkdirAll(filepath.Join(repo, "pay"), 0o755)
	os.WriteFile(filepath.Join(repo, "pay", "pay.go"), []byte("package pay\n\ntype Gateway interface {\n\tStartCheckout(ctx int) error\n}\ntype Stripe struct{}\nfunc (s *Stripe) StartCheckout(ctx int) error { return nil }\n"), 0o644)
	os.MkdirAll(filepath.Join(repo, "svc"), 0o755)
	os.WriteFile(filepath.Join(repo, "svc", "svc.go"), []byte("package svc\n\nimport \"pay\"\n\n// Handle starts a checkout.\nfunc Handle(g pay.Gateway) error {\n\treturn g.StartCheckout(1)\n}\n\nfunc Retry(g pay.Gateway) error { return Handle(g) }\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "pay", "pay_test.go"), []byte("package pay\n\nfunc TestPay(g Gateway) { g.StartCheckout(1) }\n"), 0o644)
	h.git("add", ".")
	h.git("commit", "-q", "-m", "pay")
	out, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	c.head = strings.TrimSpace(string(out))

	c.jbDir = t.TempDir()
	c.jbCalls = filepath.Join(c.jbDir, "calls.log")
	os.WriteFile(filepath.Join(c.jbDir, "rev"), []byte(c.head), 0o644)
	jb := filepath.Join(c.jbDir, "jbcontext")
	os.WriteFile(jb, []byte(`#!/bin/sh
echo "$*" >> "`+c.jbCalls+`"
case "$1" in
status) r=$(cat "`+c.jbDir+`/rev"); s=""; [ -n "$r" ] && s='{"revision":"'$r'","createdAt":1}'
	echo '{"type":"status_result","indices":[{"snapshots":['$s']}]}';;
index) echo `+c.head+` > "`+c.jbDir+`/rev";;
watch) if [ "$2" = status ]; then printf 'Watch daemon running (pid 7)\n  github.com/x/y  `+repo+`\n'; fi;;
search) cat <<'EOF'
{"type":"search_result","results":[{"result":{"sourcePosition":{"relativePath":"pay/pay.go"}},"content":"\ntype Gateway interface {\n\tStartCheckout(ctx int) error\n}","contentStartLine":3}]}
EOF
;;
esac
`), 0o755)
	t.Setenv("ORCH_JBCONTEXT", jb)

	c.gCall = filepath.Join(c.jbDir, "gopls.log")
	gopls := filepath.Join(c.jbDir, "gopls")
	os.WriteFile(gopls, []byte(`#!/bin/sh
echo "$*" >> "`+c.gCall+`"
C="`+evalOr(repo)+`"
case "$1 $2" in
"workspace_symbol StartCheckout")
	echo "$C/pay/pay.go:4:2-15 Gateway.StartCheckout Method"
	echo "$C/pay/pay.go:7:18-31 Stripe.StartCheckout Method";;
"references $C/pay/pay.go:4:2")
	echo "$C/pay/pay.go:4:2-15"
	for i in 7 1 2 3 4 5 6 8; do echo "$C/svc/svc.go:$i:5-18"; done
	echo "$C/pay/pay_test.go:3:30-43";;
"implementation $C/pay/pay.go:4:2") echo "$C/pay/pay.go:7:18-31";;
"references $C/pay/pay.go:7:18") echo "$C/svc/svc.go:30:5-18";;
esac
`), 0o755)
	t.Setenv("ORCH_GOPLS", gopls)

	c.rgCalls = filepath.Join(c.jbDir, "rg.log")
	rg := filepath.Join(c.jbDir, "rg")
	match := func(path string, n int, text string) string {
		return `{"type":"match","data":{"path":{"text":"` + path + `"},"lines":{"text":"` + text + `\n"},"line_number":` + fmt.Sprint(n) + `,"submatches":[{"match":{"text":"StartCheckout"}}]}}`
	}
	os.WriteFile(rg, []byte(`#!/bin/sh
echo "$*" >> "`+c.rgCalls+`"
case "$*" in
*zzzNone*) exit 1;;
*" -l "*) printf 'pay/pay.go\npay/pay_test.go\n';;
*" -c "*) printf 'pay/pay.go:2\npay/pay_test.go:1\n';;
*) cat <<'EOF'
`+match("pay/pay.go", 4, "\\tStartCheckout(ctx int) error")+`
`+match("pay/pay.go", 7, "func (s *Stripe) StartCheckout(ctx int) error { return nil }")+`
`+match("pay/pay_test.go", 3, "g.StartCheckout(1)")+`
{"type":"summary","data":{}}
EOF
;;
esac
`), 0o755)
	t.Setenv("ORCH_RG", rg)
	return c
}

func (c *ctxHarness) log(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func lastOutLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

func TestCtxStatusReportsIndexWatchAndTools(t *testing.T) {
	c := newCtxHarness(t)
	out := c.must("ctx", "status")
	info := ctxTestObject(t, out)["result"].(map[string]any)
	if info["ctx_index"] != "full" || info["indexed_revision"] != c.head || info["index_fresh"] != true || info["watched"] != true || info["watch_pid"] != "7" || info["watch_status"] != "Watch daemon running (pid 7)" {
		t.Fatalf("status: %#v", info)
	}
	if info["ts_server"] != "" || info["gopls"] == "" || info["rg"] == "" {
		t.Fatalf("tools: %#v", info)
	}
	if l := lastOutLine(out); !strings.HasPrefix(l, "sources: ") || !strings.Contains(l, "jbcontext index "+sha7(c.head)) {
		t.Errorf("last line = %q", l)
	}
	var js struct {
		Result struct {
			Head       string `json:"head"`
			IndexFresh bool   `json:"index_fresh"`
			Watched    bool   `json:"watched"`
			WatchPID   string `json:"watch_pid"`
		} `json:"result"`
		Sources string `json:"sources"`
	}
	if err := json.Unmarshal([]byte(c.must("ctx", "status", "--json")), &js); err != nil {
		t.Fatal(err)
	}
	if js.Result.Head != c.head || !js.Result.IndexFresh || !js.Result.Watched || js.Result.WatchPID != "7" || js.Sources == "" {
		t.Errorf("json = %+v", js)
	}
	if exists(runRoot(c.repo)) {
		t.Error("ctx status created run state")
	}
	if strings.Contains(c.log(c.jbCalls), "index") {
		t.Errorf("status refreshed the index: %s", c.log(c.jbCalls))
	}
}

func TestCtxRefreshFollowsThePolicy(t *testing.T) {
	c := newCtxHarness(t)
	os.WriteFile(filepath.Join(c.jbDir, "rev"), []byte(strings.Repeat("c", 40)), 0o644)
	out := c.must("ctx", "refresh")
	if ctxTestObject(t, out)["result"].(map[string]any)["current"] != true {
		t.Fatalf("refresh not current: %s", out)
	}
	wantContains(t, c.log(c.jbCalls), "index --only-incremental")

	os.WriteFile(filepath.Join(c.jbDir, "rev"), nil, 0o644)
	os.Remove(c.jbCalls)
	cfg := filepath.Join(c.tmp, "config.toml")
	os.WriteFile(cfg, []byte("ctx_index = \"incremental\"\n"), 0o644)
	out = c.must("ctx", "refresh")
	if strings.Contains(c.log(c.jbCalls), "index") {
		t.Errorf("incremental built a first index: %s", c.log(c.jbCalls))
	}
	if ctxTestObject(t, out)["result"].(map[string]any)["current"] != false {
		t.Fatalf("refresh unexpectedly current: %s", out)
	}
	wantContains(t, ctxTestStrings(t, out), "jbcontext has no index for this repo and ctx_index is incremental")
	c.must("ctx", "refresh", "--full")
	wantContains(t, c.log(c.jbCalls), "index --project-path")

	os.WriteFile(cfg, []byte("ctx_index = \"off\"\n"), 0o644)
	os.Remove(c.jbCalls)
	if got := ctxTestObject(t, c.must("ctx", "refresh"))["result"].(map[string]any)["skipped"]; got != "jbcontext is off (ctx_index = off)" {
		t.Fatalf("skip=%#v", got)
	}
	if c.log(c.jbCalls) != "" {
		t.Errorf("off called jbcontext: %s", c.log(c.jbCalls))
	}
	os.WriteFile(cfg, []byte("ctx_index = \"sometimes\"\n"), 0o644)
	if code, out := c.run("ctx", "status"); code == 0 {
		t.Errorf("a bad ctx_index was accepted: %d %s", code, out)
	}
}

func TestCtxWatchIsAThinWrapper(t *testing.T) {
	c := newCtxHarness(t)
	c.must("ctx", "watch", "add")
	c.must("ctx", "watch", "remove", c.repo)
	log := c.log(c.jbCalls)
	wantContains(t, log, "watch add --project-path "+evalOr(c.repo), "watch remove --project-path "+c.repo)
	if code, _ := c.run("ctx", "watch", "start"); code == 0 {
		t.Error("an unknown watch verb was accepted")
	}
}

func TestCtxFindSymTextBatchAndBriefOutsideAnyRunState(t *testing.T) {
	c := newCtxHarness(t)

	find := c.must("ctx", "find", "checkout gateway", "-p", "pay", "--limit", "2")
	findResult := ctxTestObject(t, find)["result"].(map[string]any)
	if findResult["kind"] != "find" || findResult["total"] != float64(1) || findResult["shown"] != float64(1) || findResult["scope"] != "path pay" {
		t.Fatalf("find=%#v", findResult)
	}
	wantContains(t, ctxTestStrings(t, find), "pay/pay.go", "type Gateway interface {", "\tStartCheckout(ctx int) error", "declares type Gateway", "package pay declares 3 exported symbols in 1 file")
	wantContains(t, c.log(c.jbCalls), "--limit 10", "--path-filter pay", "--revision "+c.head)
	if !strings.HasPrefix(lastOutLine(find), "sources: jbcontext index "+sha7(c.head)) {
		t.Errorf("find sources: %q", lastOutLine(find))
	}

	sym := c.must("ctx", "sym", "StartCheckout")
	symResult := ctxTestObject(t, sym)["result"].(map[string]any)
	if symResult["total"] != float64(2) {
		t.Fatalf("definitions=%#v", symResult)
	}
	wantContains(t, ctxTestStrings(t, sym), "StartCheckout  method  pay/pay.go:4  candidate (identity not verified)", "\tStartCheckout(ctx int) error", "used by 8, +1 in tests:", "svc/svc.go", "// Handle starts a checkout.", "showing 1-5 of 8; next: --page 2; left out: svc/svc.go (3)", "implemented by 1:", "func (s *Stripe) StartCheckout(ctx int) error { return nil }", "StartCheckout  method  pay/pay.go:7  candidate (identity not verified)", "used by 1, none in tests")
	if strings.Contains(sym, "pay_test.go") {
		t.Errorf("default sym listed test references:\n%s", sym)
	}
	wantContains(t, lastOutLine(sym), "gopls")
	if strings.Contains(lastOutLine(sym), "/") {
		t.Errorf("sources line carries a binary path: %q", lastOutLine(sym))
	}
	if got := ctxTestObject(t, c.must("ctx", "sym", "NoSuchSymbol"))["result"].(map[string]any); got["total"] != float64(0) || got["status"] != "empty" {
		t.Fatalf("missing symbol=%#v", got)
	}

	text := c.must("ctx", "text", "StartCheckout", "-p", "pay", "--max", "1")
	textResult := ctxTestObject(t, text)["result"].(map[string]any)
	if textResult["total"] != float64(2) || textResult["shown"] != float64(1) {
		t.Fatalf("text=%#v", textResult)
	}
	wantContains(t, ctxTestStrings(t, text), "pay/pay.go", "\tStartCheckout(ctx int) error", "type Gateway", "showing 1-1 of 2; next: --page 2")
	log := c.log(c.rgCalls)
	wantContains(t, log, "--json", "-e StartCheckout -- pay")
	c.must("ctx", "text", "Start.*Out", "--fixed")
	if lines := strings.Split(strings.TrimSpace(c.log(c.rgCalls)), "\n"); !strings.Contains(lines[len(lines)-1], "-F") {
		t.Errorf("--fixed did not search an exact string: %s", lines[len(lines)-1])
	}
	c.must("ctx", "text", "Start.*Out")
	if lines := strings.Split(strings.TrimSpace(c.log(c.rgCalls)), "\n"); strings.Contains(lines[len(lines)-1], "-F") {
		t.Errorf("the default pattern is not a regular expression: %s", lines[len(lines)-1])
	}
	if got := ctxTestObject(t, c.must("ctx", "text", "zzzNone"))["result"].(map[string]any); got["total"] != float64(0) || got["status"] != "empty" {
		t.Fatalf("empty text=%#v", got)
	}
	code, out := c.run("ctx", "text", "x", "-p", "../outside")
	if code != 1 || !strings.Contains(out, "error: path ../outside is outside the repo") {
		t.Errorf("a path outside the repo: %d %s", code, out)
	}

	// A batch answers in plan order, from the cache the second time.
	plan := `{"lookups":[{"kind":"text","query":"StartCheckout","why":"callers"},{"kind":"symbol","query":"StartCheckout"},{"kind":"concept","query":"checkout gateway"},{"kind":"read","path":"pay/pay.go","from":3,"to":4},{"kind":"text","query":"x","path":"../outside"}]}`
	c.stdin.WriteString(plan)
	batch := c.must("ctx", "batch", "-")
	batchInfo := ctxTestObject(t, batch)
	batchRows := batchInfo["result"].([]any)
	if len(batchRows) != 5 || batchInfo["status"] != "partial" {
		t.Fatalf("batch=%#v", batchInfo)
	}
	for i, kind := range []string{"text", "sym", "find", "read", "text"} {
		if batchRows[i].(map[string]any)["kind"] != kind {
			t.Fatalf("batch order: %#v", batchRows)
		}
	}
	if batchRows[0].(map[string]any)["why"] != "callers" || batchRows[4].(map[string]any)["status"] != "failed" {
		t.Fatalf("batch relationship/status: %#v", batchRows)
	}
	wantContains(t, ctxTestStrings(t, batch), "path ../outside is outside the repo")
	jb, rg := c.log(c.jbCalls), c.log(c.rgCalls)
	c.stdin.WriteString(plan)
	if again := c.must("ctx", "batch", "-"); again != batch {
		t.Errorf("a cached batch differs:\n%s", again)
	}
	if c.log(c.rgCalls) != rg || strings.Count(c.log(c.jbCalls), "search") != strings.Count(jb, "search") {
		t.Error("the second batch called rg or jbcontext again")
	}
	// ... and a changed working tree is a different cache.
	os.WriteFile(filepath.Join(c.repo, "pay", "new.go"), []byte("package pay\n"), 0o644)
	c.stdin.WriteString(plan)
	c.must("ctx", "batch", "-")
	if c.log(c.rgCalls) == rg {
		t.Error("a change in the working tree was served from the old cache")
	}
	big := `{"lookups":[` + strings.Repeat(`{"kind":"text","query":"a"},`, 60) + `{"kind":"text","query":"a"}]}`
	c.stdin.WriteString(big)
	if code, out := c.run("ctx", "batch", "-"); code != 2 || !strings.Contains(out, "at most 50") {
		t.Errorf("a huge plan: %d %s", code, out)
	}
	c.stdin.WriteString("not a plan")
	if code, _ := c.run("ctx", "batch", "-"); code != 2 {
		t.Error("a plan that is not JSON was accepted")
	}

	c.stdin.WriteString("### api: Checkout\nBuild the StartCheckout flow in `pay/pay.go:7`; it adds a `zzzNone_table`.\n")
	brief := c.must("ctx", "brief", "-", "--owns", "pay")
	briefResult := ctxTestObject(t, brief)["result"].(map[string]any)
	if len(briefResult["files"].([]any)) == 0 || len(briefResult["not_found"].([]any)) != 1 {
		t.Fatalf("brief=%#v", briefResult)
	}
	wantContains(t, ctxTestStrings(t, brief), "pay/pay.go", "zzzNone_table")
	c.stdin.WriteString("Build the StartCheckout flow")
	var js struct {
		Result struct{ Files []string } `json:"result"`
	}
	if err := json.Unmarshal([]byte(c.must("ctx", "brief", "-", "--json")), &js); err != nil {
		t.Fatal(err)
	}
	if len(js.Result.Files) == 0 {
		t.Error("JSON brief has no ranked files for StartCheckout")
	}
	if exists(runRoot(c.repo)) {
		t.Error("ctx commands created run state")
	}
}

func TestCtxPrivateTaskNeverUsesJbcontext(t *testing.T) {
	c := newCtxHarness(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "task.md"), []byte("---\nid: p\nprivate: true\n---\nbody\n"), 0o644)
	t.Setenv("ORCH_TASK_DIR", dir)
	code, out := c.run("ctx", "find", "checkout")
	if code != 1 {
		t.Fatalf("private find exit %d, want 1: %s", code, out)
	}
	if ctxTestObject(t, out)["status"] != "failed" {
		t.Fatalf("private status: %s", out)
	}
	wantContains(t, ctxTestStrings(t, out), "jbcontext is not used for private work")
	if c.log(c.jbCalls) != "" {
		t.Errorf("a private task reached jbcontext: %s", c.log(c.jbCalls))
	}
}

func ctxSpec() string {
	return planHeader + "### checkout: Checkout flow\nkind: ship\nmodel: haiku\nowns: pay\n\nBuild the StartCheckout flow in `pay/pay.go:7`; it adds a `zzzNone_table`.\n\n#### Builds\n- Code that starts a hosted checkout for an invoice.\n\n#### Acceptance\nTests pass.\n\n" + planTail
}

func TestPlanBriefsGetFactsGatheredByCodeAndNoCtxSkipsThem(t *testing.T) {
	c := newCtxHarness(t)
	c.commitSpec(ctxSpec())
	c.must("plan", "spec.md", "--skip-gate")
	text := string(mustRead(t, filepath.Join(c.dir("checkout"), "task.md")))
	i, j, k := strings.Index(text, "\n# Spec\n"), strings.Index(text, "\n# Facts gathered by code\n"), strings.Index(text, "\n# Acceptance\n")
	if !(i > 0 && j > i && k > j) {
		t.Fatalf("the facts section is not between Spec and Acceptance (%d %d %d):\n%s", i, j, k, text)
	}
	wantContains(t, text, "### 1. Ranked files", "- `pay/pay.go` (score ", "Names looked up that were not found: `zzzNone_table`", "Computed by code when this brief was written")
	if n := len(text[j:k]); n > taskBriefChars+800 {
		t.Errorf("the facts section is %d chars, want about %d at most", n, taskBriefChars)
	}
	if problems, _, err := checkTask(c.a.siteRoot(t), "checkout"); err != nil || len(problems) != 0 {
		t.Errorf("a brief with facts fails check: %v %v", problems, err)
	}

	c2 := newCtxHarness(t)
	c2.commitSpec(ctxSpec())
	c2.must("plan", "spec.md", "--skip-gate", "--no-ctx")
	if text := string(mustRead(t, filepath.Join(c2.dir("checkout"), "task.md"))); strings.Contains(text, "Facts gathered by code") {
		t.Errorf("--no-ctx still added the section:\n%s", text)
	}

	// Missing tools never fail the command.
	c3 := newCtxHarness(t)
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("ORCH_GOPLS", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("ORCH_RG", filepath.Join(t.TempDir(), "absent"))
	c3.commitSpec(ctxSpec())
	c3.must("plan", "spec.md", "--skip-gate")
	wantContains(t, string(mustRead(t, filepath.Join(c3.dir("checkout"), "task.md"))), "note: no concept search")

	// orch new has no task text to brief.
	c3.must("new", "plain", "--kind", "scout", "--harness", "claude", "--model", "sonnet", "--effort", "medium")
	if text := string(mustRead(t, filepath.Join(c3.dir("plain"), "task.md"))); strings.Contains(text, "Facts gathered by code") {
		t.Error("orch new added facts to a scaffold with placeholders")
	}
}

func TestDoctorReportsCtxReadiness(t *testing.T) {
	c := newCtxHarness(t)
	code, out := c.run("doctor")
	_ = code
	wantContains(t, out, "ctx: index fresh, watch on, gopls ok, ts server missing, rg ok")
	os.WriteFile(filepath.Join(c.jbDir, "rev"), []byte(strings.Repeat("d", 40)), 0o644)
	_, out = c.run("doctor")
	wantContains(t, out, "ctx: index stale, watch on", "(run: orch ctx refresh)")
	if strings.Contains(c.log(c.jbCalls), "index") {
		t.Error("doctor refreshed the index")
	}
}
