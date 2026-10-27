package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeGatePaneVerbs adds the verbs the gate uses to the fake herdr without
// touching its state file while passes run in parallel. A gate `agent start`
// (recognised by --session-dir) launches a detached runner that runs the fake
// pi on the prompt made of the @files, then saves a session file holding the
// reply and goes idle, like interactive pi. Status lives in <state>.panes/<pane>.status.
// Every call is appended to <state>.gate.log. It reports false for verbs it
// does not own.
func fakeGatePaneVerbs(path string, args []string) (int, bool) {
	if len(args) < 3 {
		return 0, false
	}
	verb := args[0] + " " + args[1]
	panes := path + ".panes"
	statusF := filepath.Join(panes, args[2]+".status")
	markF := filepath.Join(panes, args[2]+".gate")
	pidF := filepath.Join(panes, args[2]+".pid")
	switch verb {
	case "agent start", "agent __run", "agent __late", "agent wait", "agent get", "agent send-keys", "agent prompt", "agent rename", "pane rename", "pane run", "pane close", "tab close":
	default:
		return 0, false
	}
	logCall := func(extra map[string]any) {
		b, _ := json.Marshal(map[string]any{"args": args, "extra": extra})
		f, err := os.OpenFile(path+".gate.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	os.MkdirAll(panes, 0o755)
	status := func() string { b, _ := os.ReadFile(statusF); return strings.TrimSpace(string(b)) }
	notFound := func(target string) (int, bool) {
		fmt.Fprintf(os.Stderr, `{"error":{"code":"agent_not_found","message":"agent target %s not found"}}`+"\n", target)
		return 1, true
	}
	if verb != "agent start" && verb != "agent __run" && verb != "tab close" && verb != "pane rename" && verb != "pane run" && !exists(markF) {
		return 0, false // a lead's agent, not a gate pass
	}
	switch verb {
	case "agent start":
		if !strings.HasPrefix(args[2], "gate-") {
			return 0, false
		}
		if flagValue(args, "--session-dir") == "" && !strings.Contains(strings.Join(args, " "), "--session-dir") && flagValue(args, "--session-id") == "" {
			return 0, false
		}
		// The prompt is the @files in order, as pi would send them.
		var prompt strings.Builder
		for _, a := range args {
			if f, ok := strings.CutPrefix(a, "@"); ok {
				b, _ := os.ReadFile(f)
				prompt.WriteString(string(b))
			}
		}
		session := ""
		for i, a := range args {
			if a == "--session-dir" && i+1 < len(args) {
				session = args[i+1]
			}
		}
		pane := flagValue(args, "--pane")
		if id := flagValue(args, "--session-id"); id != "" {
			session = filepath.Dir(claudeTranscript(os.Getenv("ORCH_FAKE_CLAUDE_REPO"), id))
			os.WriteFile(filepath.Join(panes, pane+".claude"), []byte(id), 0o644)
			prompt.WriteString(args[len(args)-1])
		}
		statusF, markF, pidF = filepath.Join(panes, pane+".status"), filepath.Join(panes, pane+".gate"), filepath.Join(panes, pane+".pid")
		extra := map[string]any{}
		lateF := filepath.Join(panes, pane+".late")
		if status() != "" && !exists(lateF) {
			extra["overlap"] = true // a second agent started in a busy pane
		}
		os.Remove(lateF)
		extra["prompt"] = prompt.String()
		extra["name"] = args[2]
		logCall(extra)
		os.WriteFile(markF, nil, 0o644)
		os.MkdirAll(session, 0o755) // pi creates its session dir at launch
		os.WriteFile(statusF, []byte("working"), 0o644)
		promptF := filepath.Join(panes, pane+".prompt")
		os.WriteFile(promptF, []byte(prompt.String()), 0o644)
		os.WriteFile(filepath.Join(panes, pane+".session"), []byte(session), 0o644)
		os.Remove(filepath.Join(panes, pane+".soft"))
		cmd := exec.Command(os.Args[0], "agent", "__run", pane, session, promptF)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return 1, true
		}
		os.WriteFile(pidF, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
		for i := 0; i < 50 && status() == "working"; i++ {
			time.Sleep(20 * time.Millisecond)
		}
		if status() == "" { // pi exited at once: herdr never saw the agent
			fmt.Fprintln(os.Stderr, `{"error":{"code":"timeout","message":"timed out waiting for agent startup"}}`)
			return 1, true
		}
		fmt.Println(`{"result":{"agent":{"agent_status":"` + status() + `"}}}`)
		return 0, true
	case "agent __run": // args: pane, session dir, prompt file
		prompt, _ := os.ReadFile(args[4])
		pi := exec.Command(envOr("ORCH_PI", "pi"), "-p", string(prompt))
		// A shell keeps what `pane run` exported: the fake pi sees it.
		if b, err := os.ReadFile(filepath.Join(panes, args[2]+".env")); err == nil {
			pi.Env = append(os.Environ(), "PI_CACHE_RETENTION="+strings.TrimSpace(string(b)))
		}
		// <state>.budget holds "soft:N" or "hard:N": the pass first makes N turns
		// of tool calls and then waits to be told to answer.
		if b, err := os.ReadFile(path + ".budget"); err == nil && strings.Contains(string(prompt), "GATE-GROUP") {
			mode, n, _ := strings.Cut(strings.TrimSpace(string(b)), ":")
			turns, _ := strconv.Atoi(n)
			var sb strings.Builder
			sb.WriteString(`{"type":"session"}` + "\n")
			for range turns {
				call, _ := json.Marshal(map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "content": []map[string]any{
					{"type": "toolCall", "name": "read", "arguments": map[string]any{"path": "internal/store.go"}}, {"type": "toolCall", "name": "grep"}},
					"usage": map[string]any{"input": 10, "output": 1, "cacheRead": 100}}})
				res, _ := json.Marshal(map[string]any{"type": "message", "message": map[string]any{"role": "toolResult", "toolName": "read", "content": []map[string]any{{"type": "text", "text": "package internal"}}}})
				sb.WriteString(string(call))
				sb.WriteString("\n")
				sb.WriteString(string(res))
				sb.WriteString("\n")
			}
			os.MkdirAll(args[3], 0o755)
			os.WriteFile(filepath.Join(args[3], "session.jsonl"), []byte(sb.String()), 0o644)
			if mode == "hard" {
				time.Sleep(time.Minute) // only ctrl+c ends this
				return 0, true
			}
			for i := 0; i < 500 && !exists(filepath.Join(panes, args[2]+".soft")); i++ {
				time.Sleep(20 * time.Millisecond)
			}
			out, _ := pi.Output()
			msg, _ := json.Marshal(map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": string(out)}}}})
			f, _ := os.OpenFile(filepath.Join(args[3], "session.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
			f.Write(append(msg, '\n'))
			f.Close()
			os.WriteFile(statusF, []byte("idle"), 0o644)
			return 0, true
		}
		out, err := pi.Output()
		if err != nil {
			os.MkdirAll(args[3], 0o755) // pi creates its session dir even when it dies
			os.WriteFile(filepath.Join(args[3], "session.jsonl"), []byte(`{"type":"session"}`+"\n"), 0o644)
			os.Remove(statusF)
			return 0, true
		}
		msg := map[string]any{"role": "assistant", "content": []map[string]any{{"type": "toolCall", "name": "read"}, {"type": "text", "text": string(out)}},
			"usage": map[string]any{"input": 100, "output": 10, "cacheRead": 300, "cacheWrite": 5, "totalTokens": 415}}
		line, _ := json.Marshal(map[string]any{"type": "message", "message": msg})
		os.MkdirAll(args[3], 0o755)
		if id, err := os.ReadFile(filepath.Join(panes, args[2]+".claude")); err == nil {
			line, _ = json.Marshal(map[string]any{"type": "assistant", "message": msg})
			os.WriteFile(filepath.Join(args[3], string(id)+".jsonl"), append(line, '\n'), 0o644)
		} else {
			os.WriteFile(filepath.Join(args[3], "session.jsonl"), append([]byte(`{"type":"session"}`+"\n"), append(line, '\n')...), 0o644)
		}
		if status() != "" {
			os.WriteFile(statusF, []byte("idle"), 0o644)
		}
		return 0, true
	case "agent __late": // args: pane, then the agent start to replay once pi's turn begins
		time.Sleep(time.Second)
		// The same agent, not a second one in a busy pane. Its idle status stays
		// until the replay sets it working, so herdr never loses sight of it.
		os.WriteFile(filepath.Join(panes, args[2]+".late"), nil, 0o644)
		fakeGatePaneVerbs(path, args[3:])
		return 0, true
	case "agent wait":
		logCall(nil)
		ms, _ := strconv.Atoi(flagValue(args, "--timeout"))
		deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
		for time.Now().Before(deadline) {
			switch st := status(); st {
			case "":
				return notFound(args[2])
			case "working":
			default:
				fmt.Println(`{"result":{"agent":{"agent_status":"` + st + `"}}}`)
				return 0, true
			}
			time.Sleep(20 * time.Millisecond)
		}
		fmt.Fprintln(os.Stderr, `{"error":{"code":"timeout","message":"wait timed out"}}`)
		return 1, true
	case "agent get":
		st := status()
		if st == "" {
			return notFound(args[2])
		}
		fmt.Println(`{"result":{"agent":{"agent_status":"` + st + `"}}}`)
		return 0, true
	case "agent send-keys":
		logCall(map[string]any{"key": args[3]})
		kill := func() {
			if b, err := os.ReadFile(pidF); err == nil {
				pid, _ := strconv.Atoi(string(b))
				syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
		switch args[3] {
		case "ctrl+c":
			kill()
			if status() != "" {
				os.WriteFile(statusF, []byte("idle"), 0o644)
			}
		case "ctrl+d":
			if os.Remove(path+".stuck") != nil { // the first ctrl+d finds the agent stuck
				kill()
				os.Remove(statusF)
			}
		}
		fmt.Println(`{"result":{"type":"ok"}}`)
		return 0, true
	case "pane close": // only recorded: the shared state file is not safe under parallel passes
		logCall(nil)
		fmt.Println("{}")
		return 0, true
	case "pane rename", "agent rename":
		logCall(map[string]any{"label": strings.Join(args[3:], " ")})
		fmt.Println("{}")
		return 0, true
	case "pane run":
		cmd := strings.Join(args[3:], " ")
		file, ok := strings.CutPrefix(cmd, "sh ")
		if !ok {
			logCall(map[string]any{"cmd": cmd})
			fmt.Println("{}")
			return 0, true
		}
		return fakePaneLaunch(path, args[2], cmd, strings.Trim(file, "'"), logCall)
	case "agent prompt":
		text := args[3]
		logCall(map[string]any{"text": text, "wait": strings.Contains(strings.Join(args, " "), "--wait")})
		if strings.HasPrefix(text, "Budget reached") {
			os.WriteFile(filepath.Join(panes, args[2]+".soft"), nil, 0o644)
			fmt.Println("{}")
			return 0, true
		}
		// The final prompt after ctrl+c: answer from the stored prompt.
		session, _ := os.ReadFile(filepath.Join(panes, args[2]+".session"))
		prompt, _ := os.ReadFile(filepath.Join(panes, args[2]+".prompt"))
		pi := exec.Command(envOr("ORCH_PI", "pi"), "-p", string(prompt))
		out, _ := pi.Output()
		msg, _ := json.Marshal(map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": string(out)}}}})
		f, _ := os.OpenFile(filepath.Join(string(session), "session.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
		f.Write(append(msg, '\n'))
		f.Close()
		os.WriteFile(statusF, []byte("idle"), 0o644)
		fmt.Println(`{"result":{"agent":{"agent_status":"idle"}}}`)
		return 0, true
	default: // tab close: record how many panes the tab held, then drop them
		st := loadFake(path)
		n := 0
		for pane, tab := range st.Panes {
			if tab == args[2] {
				n++
				delete(st.Panes, pane)
			}
		}
		logCall(map[string]any{"panes": n})
		saveFake(path, st)
		fmt.Println("{}")
		return 0, true
	}
}

var launchAgentRe = regexp.MustCompile(`herdr agent (\S+)`)

// fakePaneLaunch plays a pane shell running `sh <launcher>`: the launcher runs
// for real with stub pi and claude first on PATH, which record the argv and
// environment they got. The capture then starts the fake agent through the
// `agent start` verb, so a test reads a launch like any agent start; the
// pane run call itself also records the command and the launcher text.
func fakePaneLaunch(path, pane, cmd, file string, logCall func(map[string]any)) (int, bool) {
	script, _ := os.ReadFile(file)
	logCall(map[string]any{"cmd": cmd, "script": string(script)})
	panes := path + ".panes"
	stubs := path + ".stubs"
	os.MkdirAll(stubs, 0o755)
	for _, kind := range []string{"pi", "claude"} {
		// Passes launch in parallel: write each stub once, atomically.
		if exists(filepath.Join(stubs, kind)) {
			continue
		}
		stub := "#!/bin/sh\necho " + kind + " > \"$FAKE_LAUNCH.kind\"\nprintf '%s\\0' \"$@\" > \"$FAKE_LAUNCH.args\"\nenv -0 > \"$FAKE_LAUNCH.env\"\n"
		tmp := filepath.Join(stubs, fmt.Sprintf(".%s.%d", kind, os.Getpid()))
		os.WriteFile(tmp, []byte(stub), 0o755)
		os.Rename(tmp, filepath.Join(stubs, kind))
	}
	capture := filepath.Join(panes, pane+".launch")
	sh := exec.Command("sh", file)
	sh.Env = append(os.Environ(), "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_LAUNCH="+capture)
	if out, err := sh.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "launcher failed: %v %s\n", err, out)
		return 1, true
	}
	if exists(path + ".nostart") { // the agent never shows up in the pane
		os.WriteFile(filepath.Join(panes, pane+".gate"), nil, 0o644)
		os.Remove(filepath.Join(panes, pane+".status"))
		fmt.Println("{}")
		return 0, true
	}
	kind, _ := os.ReadFile(capture + ".kind")
	rawArgs, _ := os.ReadFile(capture + ".args")
	rawEnv, _ := os.ReadFile(capture + ".env")
	envF := filepath.Join(panes, pane+".env")
	os.Remove(envF)
	for _, kv := range strings.Split(string(rawEnv), "\x00") {
		if v, ok := strings.CutPrefix(kv, "PI_CACHE_RETENTION="); ok {
			os.WriteFile(envF, []byte(v), 0o644)
		}
	}
	name := ""
	if m := launchAgentRe.FindStringSubmatch(string(script)); m != nil {
		name = m[1]
	}
	start := []string{"agent", "start", name, "--kind", strings.TrimSpace(string(kind)), "--pane", pane, "--"}
	start = append(start, strings.Split(strings.TrimSuffix(string(rawArgs), "\x00"), "\x00")...)
	if exists(path + ".idlefirst") { // pi sits idle at its prompt before its turn begins
		os.WriteFile(filepath.Join(panes, pane+".gate"), nil, 0o644)
		os.WriteFile(filepath.Join(panes, pane+".status"), []byte("idle"), 0o644)
		late := exec.Command(os.Args[0], append([]string{"agent", "__late", pane}, start...)...)
		late.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := late.Start(); err != nil {
			return 1, true
		}
		fmt.Println("{}")
		return 0, true
	}
	fakeGatePaneVerbs(path, start)
	return 0, true
}

type gateCall struct {
	Args  []string       `json:"args"`
	Extra map[string]any `json:"extra"`
}

func (h *harness) gateCalls(prefix ...string) []gateCall {
	h.t.Helper()
	var out []gateCall
	b, _ := os.ReadFile(h.fake + ".gate.log")
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var c gateCall
		if json.Unmarshal([]byte(line), &c) != nil {
			continue
		}
		if len(c.Args) >= len(prefix) && strings.Join(c.Args[:len(prefix)], " ") == strings.Join(prefix, " ") {
			out = append(out, c)
		}
	}
	return out
}

// gateRepo prepares a repo and a committed spec of n packages, and returns the
// gate dir of that spec.
func (h *harness) gateRepo(n int) string {
	h.t.Helper()
	os.MkdirAll(filepath.Join(h.repo, "internal"), 0o755)
	writeFiles(h.t, h.repo, map[string]string{"internal/store.go": "package internal\n\nfunc Put() {}\n"})
	spec := planHeader
	for i := range n {
		id := "api"
		if i > 0 {
			id = fmt.Sprintf("p%d", i)
		}
		spec += planPkg(id, "kind: ship\nowns: internal/"+id+".go", "")
	}
	h.commitSpec(spec + planTail)
	return filepath.Join(filepath.Dir(h.dir("x")), "gate", specHash(mustRead(h.t, filepath.Join(h.repo, "spec.md"))))
}

// sessionPi wraps the fake pi so it saves a session file in --session-dir, as
// the real one does, and sleeps instead of answering GATE-ADVERSARY-A when
// <fakepi>/adv-a.sleep exists.
func (h *harness) sessionPi(passes map[string]string) {
	h.t.Helper()
	dir := h.fakePi(passes)
	bin := filepath.Join(dir, "pi")
	if err := os.Rename(bin, bin+".real"); err != nil {
		h.t.Fatal(err)
	}
	script := `#!/bin/sh
d=""; prev=""
for a in "$@"; do [ "$prev" = --session-dir ] && d="$a"; prev="$a"; done
[ -n "$d" ] && { mkdir -p "$d"; echo '{"type":"message","message":{"role":"assistant","usage":{"input":100,"output":10,"cacheRead":300,"cacheWrite":5,"totalTokens":415}}}' > "$d/session.jsonl"; }
case "$*" in
*GATE-ADVERSARY-A*) [ -f "` + dir + `/adv-a.sleep" ] && exec sleep 30;;
*GATE-GROUP*)
	echo attempt >> "` + dir + `/attempts.log"
	[ -f "` + dir + `/group.sleep" ] && exec sleep 30
	[ -f "` + dir + `/group.sleep.once" ] && { rm -f "` + dir + `/group.sleep.once"; exec sleep 30; };;
esac
exec "` + bin + `.real" "$@"
`
	os.WriteFile(bin, []byte(script), 0o755)
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestGatePanePassesRunInPanesAndApplyLikeHeadless(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(3)
	blocker := map[string]string{"adv-b": `{"findings":[{"severity":"blocker","where":"Design rules","claim":"Put ignores errors","evidence":"internal/store.go:3","why":"The failure path cannot return an error","support_assessment":{"schema_version":1,"state":"supported","rationale":"Put has no error return","excerpt":"func Put() {}","failure_scenario":"A write failure is hidden from the caller"}}]}`}
	h.sessionPi(gatePasses(blocker))

	code, paneOut := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("a verified blocker passed the gate in pane mode: %d %s", code, paneOut)
	}
	if view := wantGateState(t, paneOut, "fail"); view.Blockers != "1 verified" {
		t.Fatalf("accepted blocker count missing: %+v", view)
	}
	// Compare decoded selections, retaining every fact and the exact note multiset.
	// Validate each complete budget against its own persisted run before removing
	// only clock readings, which legitimately differ between independent runs.
	stableSelection := func(out string) map[string]any {
		view := decodedOutput(t, []byte(out), outputAgent).(map[string]any)
		budgetJSON, err := json.Marshal(readRecord(t, gdir).Budget)
		if err != nil {
			t.Fatal(err)
		}
		if want := decodedOutput(t, budgetJSON, outputJSON); !reflect.DeepEqual(view["budget"], want) {
			t.Fatalf("output budget differs from recorded run: %#v != %#v", view["budget"], want)
		}
		budget := view["budget"].(map[string]any)
		for _, key := range []string{"started", "elapsed_seconds", "attempt_seconds"} {
			delete(budget, key)
		}
		delete(budget["remaining"].(map[string]any), "seconds")
		slices.SortFunc(view["notes"].([]any), func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
		return view
	}
	paneView := stableSelection(paneOut)
	if n := len(h.gateCalls("agent", "start")); n != 3 {
		t.Fatalf("agent start calls = %d, want 3 (2 adversaries + one group pass for 3 packages)", n)
	}
	creates := callsOf(h.fakeState(), "tab", "create")
	if len(creates) != 1 {
		t.Fatalf("tab create calls = %d, want 1", len(creates))
	}
	create := creates[0]
	hash := filepath.Base(gdir)
	if flagValue(create, "--label") != "gate-"+hash[:8] || flagValue(create, "--workspace") != "w1" || !strings.Contains(strings.Join(create, " "), "--no-focus") {
		t.Fatalf("tab create = %v", create)
	}
	if st := h.fakeState(); len(callsOf(st, "agent", "start")) != 0 || len(st.Agents) != 0 {
		t.Fatal("gate panes were registered as agents")
	}
	if got := mustRead(t, filepath.Join(h.tmp, "fakepi", "calls.log")); strings.Count(string(got), "GATE-") != 3 {
		t.Fatalf("pi ran %d times, want 3", strings.Count(string(got), "GATE-"))
	}

	var labels []string
	for _, c := range h.gateCalls("pane", "rename") {
		labels = append(labels, fmt.Sprint(c.Extra["label"]))
	}
	for _, want := range []string{"adversary-a", "adversary-b", "group:api+2"} {
		found := false
		for _, l := range labels {
			found = found || l == want
		}
		if !found {
			t.Errorf("no pane was renamed %q: %v", want, labels)
		}
	}

	// The same passes headless give the same verdict and the same output.
	t.Setenv("HERDR_ENV", "")
	code, headOut := h.run("gate", "spec.md")
	headView := stableSelection(headOut)
	// The fake pane session holds a toolCall, so only the pane run reports turns.
	paneView["tokens"] = strings.Replace(paneView["tokens"].(string), ", 3 turns, budget hits: soft 0, hard 0", "", 1)
	if code != 1 || !reflect.DeepEqual(headView, paneView) {
		t.Fatalf("headless output differs from pane mode:\n--- pane\n%s\n--- headless\n%s", paneOut, headOut)
	}
}

func TestGatePaneCountIsCappedAndTabClosedAtTheEnd(t *testing.T) {
	h := setup(t)
	h.gateRepo(30)
	h.sessionPi(gatePasses(nil))
	h.must("gate", "spec.md")
	if n := len(h.gateCalls("agent", "start")); n != 10 {
		t.Fatalf("agent start calls = %d, want 10 (2 adversaries + 8 groups of at most 4) for 30 packages", n)
	}
	closes := h.gateCalls("tab", "close")
	if len(closes) != 1 {
		t.Fatalf("tab close calls = %d, want 1", len(closes))
	}
	if n := int(closes[0].Extra["panes"].(float64)); n != gateParallel {
		t.Fatalf("the tab held %d panes, want %d", n, gateParallel)
	}
	for _, c := range h.gateCalls("agent", "start") {
		if c.Extra["overlap"] != nil {
			t.Fatalf("an agent was started in a busy pane: %v", c.Args[2])
		}
	}
	if len(h.fakeState().Panes) != 0 {
		t.Fatal("panes outlived the gate")
	}
}

func TestGateFailedPassKeepsFilesAndSuccessDeletesThem(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(1)
	h.sessionPi(gatePasses(map[string]string{"adv-a": "FAIL"}))
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, out)
	}
	wantContains(t, wantGateState(t, out, "incomplete").PassErrors["adversary-a"], "pi did not start")
	for _, f := range []string{"passes/adversary-a.md", "passes/adversary-a.err", "passes/shared-adversary.md", "passes/shared-package.md", "sessions/adversary-a/session.jsonl"} {
		if !exists(filepath.Join(gdir, f)) {
			t.Errorf("failed pass lost %s", f)
		}
	}
	for _, f := range []string{"passes/adversary-b.md", "passes/adversary-b.err", "sessions/adversary-b", "passes/group-api.md", "sessions/group-api"} {
		if exists(filepath.Join(gdir, f)) {
			t.Errorf("succeeding pass left %s", f)
		}
	}
	report := string(mustRead(t, filepath.Join(gdir, "report.md")))
	wantContains(t, report, "## Passes that did not run", "adversary-a: kept "+filepath.Join(gdir, "passes", "adversary-a.md"))
	if n := len(h.gateCalls("tab", "close")); n != 1 {
		t.Fatalf("tab close calls = %d after a failed pass, want 1", n)
	}
}

func TestGateTimedOutPassGetsCtrlCAndDoesNotStallTheOthers(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(3)
	h.sessionPi(gatePasses(nil))
	os.WriteFile(filepath.Join(h.tmp, "fakepi", "adv-a.sleep"), nil, 0o644)
	// The cap covers a whole pass (setup, pane start, fake pi), which is slow
	// under -race on a busy machine. It must leave the two passes that answer
	// room to finish, so only the sleeping pass can hit it.
	old := gateTimeout
	gateTimeout = 12 * time.Second
	t.Cleanup(func() { gateTimeout = old })
	const wantTimeout = "timed out after 12s"

	start := time.Now()
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if time.Since(start) > gateTimeout+20*time.Second {
		t.Fatalf("gate took %s with a %s pass timeout", time.Since(start), gateTimeout)
	}
	view := wantGateState(t, out, "incomplete")
	if view.PassErrors["adversary-a"] != wantTimeout {
		t.Fatalf("wrong public timeout diagnostic: %+v", view.PassErrors)
	}
	failure := readRecord(t, gdir).PassErrors["adversary-a"]
	if failure != wantTimeout {
		t.Fatalf("recorded timeout = %q, want clear wall-cap diagnostic", failure)
	}
	t.Logf("timeout diagnostic: %s", failure)
	if n := len(view.PassErrors); n != 1 {
		t.Fatalf("%d passes failed, want only the timed-out one:\n%s", n, out)
	}
	ints, exits := 0, 0
	for _, k := range h.gateCalls("agent", "send-keys") {
		switch k.Args[3] {
		case "ctrl+c":
			ints++
		case "ctrl+d":
			exits++
		}
	}
	if ints != 1 || exits != 3 {
		t.Fatalf("ctrl+c sent %d times and ctrl+d %d times, want 1 and 3 (every pass leaves its pane)", ints, exits)
	}
	if n := len(h.gateCalls("agent", "start")); n != 3 {
		t.Fatalf("agent start calls = %d, want all 3 passes started", n)
	}
	if !exists(filepath.Join(gdir, "sessions", "adversary-a")) {
		t.Fatal("the timed-out pass lost its session")
	}
}

var agentNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func TestGateAgentStartNamesAreValidAndPromptFilesShareTheirPrefix(t *testing.T) {
	h := setup(t)
	h.gateRepo(10)
	h.sessionPi(gatePasses(nil))
	h.must("gate", "spec.md")
	sp := parseSpec(string(mustRead(t, filepath.Join(h.repo, "spec.md"))))
	names := map[string]bool{}
	shared := map[string][]string{} // shared file path -> prompts sent with it
	for _, c := range h.gateCalls("agent", "start") {
		name := c.Args[2]
		if !agentNameRe.MatchString(name) || names[name] || flagValue(c.Args, "--kind") != "pi" {
			t.Errorf("agent start %v: bad name or kind", c.Args[:6])
		}
		names[name] = true
		files := []string{}
		for _, a := range c.Args {
			if f, ok := strings.CutPrefix(a, "@"); ok {
				files = append(files, f)
			}
		}
		if len(files) != 2 || !strings.HasPrefix(filepath.Base(files[0]), "shared-") || strings.HasPrefix(filepath.Base(files[1]), "shared-") {
			t.Fatalf("want the shared file then the task file, got %v", files)
		}
		shared[files[0]] = append(shared[files[0]], fmt.Sprint(c.Extra["prompt"]))
	}
	if len(shared) != 2 {
		t.Fatalf("passes used %d shared files, want 2 (adversary and package): %v", len(shared), shared)
	}
	for f, prompts := range shared {
		want := 2 // both adversaries
		if strings.HasSuffix(f, "shared-package.md") {
			want = 3 // the three groups of 10 packages (4, 4 and 2)
		}
		if len(prompts) != want {
			t.Errorf("%s was sent %d times, want %d", f, len(prompts), want)
		}
		common := len(prompts[0])
		for _, prompt := range prompts[1:] {
			common = min(common, commonPrefix(prompts[0], prompt))
		}
		specSrc := string(mustRead(t, filepath.Join(h.repo, "spec.md")))
		sharedText := gateHeader + gateSpecContext(specSrc, "spec.md")
		if strings.HasSuffix(f, "shared-package.md") {
			sharedText = gateHeader + gatePackageShared(sp, specSrc, "spec.md")
		}
		if common < len(sharedText) {
			t.Errorf("prompts of %s share %d prefix bytes, want at least %d", filepath.Base(f), common, len(sharedText))
		}
		for _, prompt := range prompts {
			if !strings.Contains(prompt, sharedText) {
				t.Errorf("prompt for %s lacks the shared spec context", filepath.Base(f))
			}
		}
	}
}

// A launcher whose agent herdr never sees (pi died at once, a bad argument)
// fails the pass after gateStartWait instead of waiting for the pass timeout.
func TestGatePaneAgentThatNeverAppearsFailsThePass(t *testing.T) {
	h := setup(t)
	h.gateRepo(1)
	h.sessionPi(gatePasses(nil))
	old := gateStartWait
	gateStartWait = 200 * time.Millisecond
	t.Cleanup(func() { gateStartWait = old })
	os.WriteFile(h.fake+".nostart", nil, 0o644)
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, out)
	}
	view := wantGateState(t, out, "incomplete")
	wantContains(t, view.PassErrors["group:api"], "pi did not start: herdr saw no agent in pane", "within 200ms")
}

// pi is idle at its prompt for a moment after it starts (its extensions load
// before it sends the prompt from its arguments). An idle agent with an empty
// session is still starting, so the pass waits for its turn instead of ending
// with no reply.
func TestGatePaneWaitsForAnIdleAgentToBeginItsTurn(t *testing.T) {
	h := setup(t)
	h.gateRepo(1)
	h.sessionPi(gatePasses(nil))
	os.WriteFile(h.fake+".idlefirst", nil, 0o644)
	wantGateState(t, h.must("gate", "spec.md"), "pass")
	if starts, runs := len(h.gateCalls("agent", "start")), len(h.gateCalls("pane", "run")); starts != runs {
		t.Fatalf("agent starts = %d for %d launches, want one each", starts, runs)
	}
}

func TestGateStuckPaneIsReplacedByAFreshSplit(t *testing.T) {
	h := setup(t)
	h.gateRepo(30)
	h.sessionPi(gatePasses(nil))
	os.WriteFile(h.fake+".stuck", nil, 0o644)                                // the first pi to get ctrl+d does not exit
	gateLeaveWait, gateLeavePoll = 1500*time.Millisecond, 5*time.Millisecond // long enough that a healthy fake pane always exits first
	wantGateState(t, h.must("gate", "spec.md"), "pass")
	closed := h.gateCalls("pane", "close")
	if len(closed) != 1 {
		t.Fatalf("pane close calls = %d, want only the stuck pane", len(closed))
	}
	uses := 0
	for _, c := range h.gateCalls("agent", "start") {
		if flagValue(c.Args, "--pane") == closed[0].Args[2] {
			uses++
		}
	}
	if uses != 1 {
		t.Errorf("the closed pane %s started %d agents, want only the one that got stuck", closed[0].Args[2], uses)
	}
	if n := len(h.gateCalls("agent", "start")); n != 10 {
		t.Errorf("agent start calls = %d, want all 10 passes to run", n)
	}
}

func TestGateHeadlessWithoutHerdrKeepsOnlyFailedSessions(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(1)
	t.Setenv("HERDR_ENV", "")
	h.sessionPi(gatePasses(map[string]string{"adv-a": "FAIL"}))

	// A stale run beyond retention loses its saved files on the next gate.
	stale := filepath.Join(filepath.Dir(gdir), "stale")
	os.MkdirAll(filepath.Join(stale, "sessions", "x"), 0o755)
	os.WriteFile(filepath.Join(stale, "gate.json"), fmt.Appendf(nil, `{"at":%d}`, h.now.Add(-8*24*time.Hour).Unix()), 0o644)

	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if failure := wantGateState(t, out, "incomplete").PassErrors["adversary-a"]; failure != "pi failed: exit status 1: boom" {
		t.Fatalf("wrong headless failure: %q", failure)
	}
	if st := h.fakeState(); len(st.Calls) != 0 {
		t.Fatalf("headless gate called herdr: %v", st.Calls)
	}
	if !exists(filepath.Join(gdir, "sessions", "adversary-a", "session.jsonl")) {
		t.Error("failed headless pass lost its session")
	}
	if exists(filepath.Join(gdir, "sessions", "adversary-b")) {
		t.Error("succeeding headless pass kept its session")
	}
	if exists(filepath.Join(stale, "sessions")) || !exists(filepath.Join(stale, "gate.json")) {
		t.Error("stale run was not pruned, or lost gate.json")
	}
	calls := string(mustRead(t, filepath.Join(h.tmp, "fakepi", "calls.log")))
	wantContains(t, calls, "--session-dir")
	if strings.Contains(calls, "--no-session") {
		t.Error("headless pi still runs with --no-session")
	}
}

func TestPruneGateKeepsNewestRunsAndRecentOnes(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	mk := func(name string, age time.Duration) string {
		d := filepath.Join(root, "gate", name)
		for _, sub := range []string{"sessions/s", "passes", "questions"} {
			os.MkdirAll(filepath.Join(d, sub), 0o755)
		}
		os.WriteFile(filepath.Join(d, "passes", "a.out"), []byte("x"), 0o644)
		os.WriteFile(filepath.Join(d, "gate.json"), fmt.Appendf(nil, `{"at":%d}`, now.Add(-age).Unix()), 0o644)
		os.WriteFile(filepath.Join(d, "report.md"), []byte("r"), 0o644)
		return d
	}
	cur := mk("cur", 0)
	var recent []string
	for i := 1; i <= 6; i++ {
		recent = append(recent, mk(fmt.Sprintf("r%d", i), time.Duration(i)*time.Hour))
	}
	atLimit := mk("limit", 7*24*time.Hour+0)
	over := mk("over", 7*24*time.Hour+time.Second)
	pruneGate(root, "cur", now)

	pruned := func(d string) bool {
		return !exists(filepath.Join(d, "sessions")) && !exists(filepath.Join(d, "passes"))
	}
	// The current run plus the four newest others fill the five slots.
	for i, d := range recent {
		if want := i >= gateKeepRuns-1; pruned(d) != want {
			t.Errorf("run r%d pruned = %v, want %v", i+1, pruned(d), want)
		}
	}
	if !pruned(over) {
		t.Error("a run older than 7 days kept its sessions")
	}
	if !pruned(atLimit) {
		t.Error("a run beyond the newest 5 kept its sessions")
	}
	if pruned(cur) {
		t.Error("the current run was pruned")
	}
	for _, d := range append(recent, cur, atLimit, over) {
		for _, keep := range []string{"gate.json", "report.md", "questions"} {
			if !exists(filepath.Join(d, keep)) {
				t.Errorf("%s lost %s", d, keep)
			}
		}
	}

	// Age alone also prunes, and exactly 7 days old is still kept.
	root2 := t.TempDir()
	root = root2
	a := mk("exact", 7*24*time.Hour)
	b := mk("older", 7*24*time.Hour+time.Second)
	pruneGate(root2, "none", now)
	if pruned(a) || !pruned(b) {
		t.Errorf("7-day boundary: exact pruned = %v, older pruned = %v", pruned(a), pruned(b))
	}
}

func TestPruneGateKeepsRunningGates(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	mk := func(name string, json bool, mtime time.Time) string {
		d := filepath.Join(root, "gate", name)
		os.MkdirAll(filepath.Join(d, "sessions", "s"), 0o755)
		os.MkdirAll(filepath.Join(d, "passes"), 0o755)
		if json {
			os.WriteFile(filepath.Join(d, "gate.json"), fmt.Appendf(nil, `{"at":%d}`, now.Add(-time.Hour).Unix()), 0o644)
		}
		os.Chtimes(d, mtime, mtime)
		return d
	}
	running := mk("running", false, now.Add(-time.Minute))
	abandoned := mk("abandoned", false, now.Add(-gateKeepDays*24*time.Hour-time.Second))
	mk("cur", false, now)
	for i := range gateKeepRuns {
		mk(fmt.Sprintf("done%d", i), true, now)
	}
	pruneGate(root, "cur", now)

	if !exists(filepath.Join(running, "sessions")) || !exists(filepath.Join(running, "passes")) {
		t.Error("a gate that is still running was pruned")
	}
	if exists(filepath.Join(abandoned, "sessions")) {
		t.Error("a gate without gate.json, stale beyond the retention window, kept its sessions")
	}
}

func TestGatePaneLoadsHerdrExtensionOnlyWhenItExists(t *testing.T) {
	h := setup(t)
	h.gateRepo(1)
	h.sessionPi(gatePasses(nil))
	h.must("gate", "spec.md")
	for _, c := range h.gateCalls("agent", "start") {
		if got := flagValue(c.Args, "-e"); got != "" {
			t.Fatalf("expected no extension while the herdr extension is missing: %v", c.Args)
		}
	}

	ext := filepath.Join(h.tmp, "herdr-agent-state.ts")
	os.WriteFile(ext, []byte("// ext\n"), 0o644)
	gatePiExtension = ext
	os.Remove(h.fake + ".gate.log")
	h.must("gate", "spec.md")
	starts := h.gateCalls("agent", "start")
	if len(starts) != 3 {
		t.Fatalf("%d agent starts, want 3", len(starts))
	}
	for _, c := range starts {
		if flagValue(c.Args, "-e") != ext {
			t.Errorf("agent start does not load the extension: %v", c.Args)
		}
	}

	// Headless passes never load it.
	t.Setenv("HERDR_ENV", "")
	h.fakePi(gatePasses(nil))
	h.must("gate", "spec.md")
	if calls := string(mustRead(t, filepath.Join(h.tmp, "fakepi", "calls.log"))); strings.Contains(calls, ext) {
		t.Error("a headless pass loaded the herdr extension")
	}
}

func TestGateStalledGroupPassIsRetriedOnceThenFails(t *testing.T) {
	for _, mode := range []string{"pane", "headless"} {
		t.Run(mode, func(t *testing.T) {
			h := setup(t)
			h.gateRepo(1)
			if mode == "headless" {
				t.Setenv("HERDR_ENV", "")
			}
			h.sessionPi(gatePasses(nil))
			dir := filepath.Join(h.tmp, "fakepi")
			os.WriteFile(filepath.Join(dir, "group.sleep"), nil, 0o644)
			gateStall = 300 * time.Millisecond
			code, out := h.run("gate", "spec.md", "--runner", "pi:"+gateModelDef)
			if code != 1 {
				t.Fatalf("exit %d: %s", code, out)
			}
			view := wantGateState(t, out, "incomplete")
			wantContains(t, view.PassErrors["group:api"], "stalled: no activity for 300ms", "retried once and failed again")
			if n := len(view.PassErrors); n != 1 {
				t.Fatalf("%d passes failed, want only the stalled group:\n%s", n, out)
			}
			if n := strings.Count(string(mustRead(t, filepath.Join(dir, "attempts.log"))), "attempt"); n != 2 {
				t.Errorf("the group pass ran %d times, want the try and one retry", n)
			}
		})
	}
}

func TestGateStalledGroupPassSucceedsOnTheRetry(t *testing.T) {
	for _, mode := range []string{"pane", "headless"} {
		t.Run(mode, func(t *testing.T) {
			h := setup(t)
			h.gateRepo(1)
			if mode == "headless" {
				t.Setenv("HERDR_ENV", "")
			}
			h.sessionPi(gatePasses(nil))
			dir := filepath.Join(h.tmp, "fakepi")
			os.WriteFile(filepath.Join(dir, "group.sleep.once"), nil, 0o644)
			gateStall = 300 * time.Millisecond
			out := h.must("gate", "spec.md", "--runner", "pi:"+gateModelDef)
			if view := wantGateState(t, out, "pass"); len(view.PassErrors) != 0 {
				t.Fatalf("a retried pass was reported failed:\n%s", out)
			}
			if n := strings.Count(string(mustRead(t, filepath.Join(dir, "attempts.log"))), "attempt"); n != 2 {
				t.Errorf("the group pass ran %d times, want 2", n)
			}
		})
	}
}
