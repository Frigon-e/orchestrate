package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// codeHarness is a repo with a committed Go file, a jbcontext, a gopls and an
// rg stand-in, each logging its calls, and an app running in that repo.
type codeHarness struct {
	*harness
	head                    string
	jbCalls, rgCalls, gCall string
	jbDir                   string
}

func newCodeHarness(t *testing.T) *codeHarness {
	t.Helper()
	h := setup(t)
	c := &codeHarness{harness: h}
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

func (c *codeHarness) log(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}
