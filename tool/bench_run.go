package main

// orch bench run: replay past commits of a target repo as agent tasks. One cell
// per (task, arm, repeat): a detached worktree at the commit's parent, a headless
// claude run configured by the arm, the commit's hidden tests restored, a check
// command, and the cell stored as a bench run of kind "replay" with free metrics.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"
)

const benchReplayFooter = "You are in a checkout of this repository at the commit before this change. Make the change, follow the repository's conventions, and run the tests that cover what you changed. Do not commit, push, or create branches or worktrees. Do not ask questions: decide and finish. End with a one-paragraph summary of what you changed."

var (
	benchReplayStepTimeout = 20 * time.Minute
	benchReplayDefaultTool = []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep", "LSP", "Agent", "Task", "TodoWrite"}
	benchLSPWordRE         = regexp.MustCompile(`(?i)\blsp\b`)
	benchReplayBuildRE     = regexp.MustCompile(`\b(go (build|test|vet)|scripts/agent/check|make (test|check|lint|build)|tsc|vitest|vp (test|check))\b`)
)

type benchTask struct {
	ID     string   `toml:"id"`
	Commit string   `toml:"commit"`
	Hidden []string `toml:"hidden"`
	Check  string   `toml:"check"`
	Setup  string   `toml:"setup"`
	Prompt string   `toml:"prompt"`
}
type benchTaskFile struct {
	Repo string      `toml:"repo"`
	Task []benchTask `toml:"task"`
}

type benchReplayArm struct {
	Name          string
	Model         string
	SettingSrc    string
	PluginDirs    []string
	AppendPrompt  string
	AllowedTools  []string
	Env           map[string]string
	StrictMCP     bool
	Tools         []string
	ToolsSet      bool
	Disallowed    []string
	MaxBudgetUSD  float64
	TimeoutMinute int
	Def           map[string]any
}

type benchReplayCell struct {
	Task   benchTask
	Arm    benchReplayArm
	Repeat int
}

func (c benchReplayCell) label() string {
	return c.Task.ID + "-" + c.Arm.Name + "-r" + strconv.Itoa(c.Repeat)
}

func benchStrings(v any) []string {
	var out []string
	switch x := v.(type) {
	case []string:
		out = append(out, x...)
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func benchNumber(v any, def float64) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case float64:
		return x
	}
	return def
}

func benchReplayArmParse(name string, def map[string]any) (benchReplayArm, error) {
	arm := benchReplayArm{Name: name, Def: def, Model: "sonnet", SettingSrc: "project,local", AllowedTools: benchReplayDefaultTool, MaxBudgetUSD: 10, TimeoutMinute: 60}
	if k, _ := def["kind"].(string); k != "replay" {
		return arm, fail("arm %s is not a replay arm (kind = \"replay\" required)", name)
	}
	if s, ok := def["model"].(string); ok && s != "" {
		arm.Model = s
	}
	if s, ok := def["setting_sources"].(string); ok && s != "" {
		arm.SettingSrc = s
	}
	for _, d := range benchStrings(def["plugin_dirs"]) {
		arm.PluginDirs = append(arm.PluginDirs, expandHome(d))
	}
	arm.AppendPrompt, _ = def["append_system_prompt"].(string)
	if t := benchStrings(def["allowed_tools"]); len(t) > 0 {
		arm.AllowedTools = t
	}
	if m, ok := def["env"].(map[string]any); ok {
		arm.Env = map[string]string{}
		for k, v := range m {
			if s, ok := v.(string); ok {
				arm.Env[k] = s
			}
		}
	}
	arm.StrictMCP, _ = def["strict_mcp_config"].(bool)
	if _, ok := def["tools"]; ok {
		arm.ToolsSet = true
		arm.Tools = benchStrings(def["tools"])
	}
	arm.Disallowed = benchStrings(def["disallowed_tools"])
	arm.MaxBudgetUSD = benchNumber(def["max_budget_usd"], 10)
	arm.TimeoutMinute = int(benchNumber(def["timeout_minutes"], 60))
	if arm.MaxBudgetUSD <= 0 || arm.TimeoutMinute <= 0 {
		return arm, fail("arm %s: max_budget_usd and timeout_minutes must be positive", name)
	}
	return arm, nil
}

func benchReplayArgv(c benchReplayCell, session string) []string {
	args := []string{"-p", c.Task.Prompt + "\n\n" + benchReplayFooter, "--model", c.Arm.Model, "--setting-sources", c.Arm.SettingSrc,
		"--session-id", session, "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits",
		"--allowedTools", strings.Join(c.Arm.AllowedTools, ","), "--max-budget-usd", strconv.FormatFloat(c.Arm.MaxBudgetUSD, 'f', -1, 64)}
	for _, d := range c.Arm.PluginDirs {
		args = append(args, "--plugin-dir", d)
	}
	if c.Arm.AppendPrompt != "" {
		args = append(args, "--append-system-prompt", c.Arm.AppendPrompt)
	}
	if c.Arm.StrictMCP {
		args = append(args, "--strict-mcp-config")
	}
	if c.Arm.ToolsSet {
		args = append(args, "--tools", strings.Join(c.Arm.Tools, ","))
	}
	if len(c.Arm.Disallowed) > 0 {
		args = append(args, "--disallowedTools", strings.Join(c.Arm.Disallowed, ","))
	}
	return args
}

func benchReplayCacheRoot() string {
	if r := os.Getenv("ORCH_CACHE_DIR"); r != "" {
		return expandHome(r)
	}
	c, e := os.UserCacheDir()
	if e != nil {
		h, _ := os.UserHomeDir()
		c = filepath.Join(h, ".cache")
	}
	return filepath.Join(c, "orchestrate")
}

func benchGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, e := cmd.Output()
	if e != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), e, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// benchReplayPlan loads the suite and tasks and validates every selected cell.
func benchReplayPlan(suiteName string, taskSel, armSel []string, repeats int) (string, []benchReplayCell, error) {
	suite, e := benchSuiteLoad(suiteName)
	if e != nil {
		return "", nil, e
	}
	var tf benchTaskFile
	tpath := filepath.Join(benchDir(), suiteName, "tasks.toml")
	if e := benchSafe(tpath); e != nil {
		return "", nil, e
	}
	if _, e := toml.DecodeFile(tpath, &tf); e != nil {
		return "", nil, e
	}
	repo := expandHome(suite.Repo)
	if tf.Repo != "" {
		tr := expandHome(tf.Repo)
		if repo != "" && repo != tr {
			return "", nil, fail("tasks.toml repo %s differs from suite repo %s", tr, repo)
		}
		repo = tr
	}
	if repo == "" {
		return "", nil, fail("no repo: set repo in suite.toml or tasks.toml")
	}
	var armNames []string
	if len(armSel) > 0 {
		armNames = armSel
	} else {
		for n, d := range suite.Arms {
			if k, _ := d["kind"].(string); k == "replay" {
				armNames = append(armNames, n)
			}
		}
		sort.Strings(armNames)
	}
	if len(armNames) == 0 {
		return "", nil, fail("suite %s has no replay arms", suiteName)
	}
	var arms []benchReplayArm
	for _, n := range armNames {
		d, ok := suite.Arms[n]
		if !ok {
			return "", nil, fail("unknown arm %q in suite %s", n, suiteName)
		}
		arm, e := benchReplayArmParse(n, d)
		if e != nil {
			return "", nil, e
		}
		arms = append(arms, arm)
	}
	byID := map[string]benchTask{}
	var order []string
	for _, t := range tf.Task {
		if e := benchName(t.ID); e != nil {
			return "", nil, e
		}
		if _, dup := byID[t.ID]; dup {
			return "", nil, fail("duplicate task id %q", t.ID)
		}
		if t.Commit == "" || t.Check == "" || t.Prompt == "" {
			return "", nil, fail("task %s needs commit, check and prompt", t.ID)
		}
		byID[t.ID] = t
		order = append(order, t.ID)
	}
	if len(taskSel) > 0 {
		order = taskSel
	}
	var tasks []benchTask
	for _, id := range order {
		t, ok := byID[id]
		if !ok {
			return "", nil, fail("unknown task %q", id)
		}
		if _, e := benchGit(repo, "rev-parse", "--verify", "--quiet", t.Commit+"^{commit}"); e != nil {
			return "", nil, fail("task %s: commit %s not found in %s", id, t.Commit, repo)
		}
		if _, e := benchGit(repo, "rev-parse", "--verify", "--quiet", t.Commit+"^^{commit}"); e != nil {
			return "", nil, fail("task %s: commit %s has no parent", id, t.Commit)
		}
		for _, h := range t.Hidden {
			if _, e := benchGit(repo, "cat-file", "-e", t.Commit+":"+h); e != nil {
				return "", nil, fail("task %s: hidden path %s does not exist at %s", id, h, t.Commit)
			}
		}
		tasks = append(tasks, t)
	}
	var cells []benchReplayCell
	for r := 1; r <= repeats; r++ {
		for _, t := range tasks {
			for _, arm := range arms {
				c := benchReplayCell{Task: t, Arm: arm, Repeat: r}
				if e := benchName(c.label()); e != nil {
					return "", nil, e
				}
				cells = append(cells, c)
			}
		}
	}
	return repo, cells, nil
}

type benchStreamStats struct {
	Result                                          bool
	IsError                                         bool
	Cost                                            float64
	Turns                                           float64
	In, Out, CacheRead, CacheWrite                  float64
	Tools, Read, LSP, Bash, Build, Sub              float64
	MainTools, MainRead, MainLSP, MainBash, MainSub float64
	Search, ShellEdits, OrchLSP, ToolSearch         float64
	LSPViaToolSearch                                bool
	InitSeen, LSPListed                             bool
}

func benchParseStream(r io.Reader) benchStreamStats {
	var s benchStreamStats
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, e := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.line(line)
		}
		if e != nil {
			break
		}
	}
	return s
}

func (s *benchStreamStats) line(b []byte) {
	var ev struct {
		Type       string                    `json:"type"`
		Parent     *string                   `json:"parent_tool_use_id"`
		IsError    bool                      `json:"is_error"`
		Cost       float64                   `json:"total_cost_usd"`
		Turns      float64                   `json:"num_turns"`
		Usage      map[string]any            `json:"usage"`
		ModelUsage map[string]map[string]any `json:"modelUsage"`
		Subtype    string                    `json:"subtype"`
		Tools      []string                  `json:"tools"`
		Message    struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" && ev.Tools != nil {
			s.InitSeen = true
			for _, t := range ev.Tools {
				if t == "LSP" {
					s.LSPListed = true
				}
			}
		}
	case "assistant":
		var blocks []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				Command string `json:"command"`
				Query   string `json:"query"`
			} `json:"input"`
		}
		if json.Unmarshal(ev.Message.Content, &blocks) != nil {
			return
		}
		main := ev.Parent == nil
		for _, bl := range blocks {
			if bl.Type != "tool_use" {
				continue
			}
			s.Tools++
			if main {
				s.MainTools++
			}
			switch bl.Name {
			case "Read":
				s.Read++
				if main {
					s.MainRead++
				}
			case "Grep", "Glob":
				s.Search++
			case "ToolSearch":
				s.ToolSearch++
				if benchLSPWordRE.MatchString(bl.Input.Query) {
					s.LSPViaToolSearch = true
				}
			case "LSP":
				s.LSP++
				if main {
					s.MainLSP++
				}
			case "Bash":
				s.Bash++
				if main {
					s.MainBash++
				}
				if benchReplayBuildRE.MatchString(bl.Input.Command) {
					s.Build++
				}
				cc := benchClassifyCommand(bl.Input.Command)
				s.Read += float64(cc.Reads)
				if main {
					s.MainRead += float64(cc.Reads)
				}
				s.Search += float64(cc.Searches)
				s.ShellEdits += float64(cc.Edits)
				s.OrchLSP += float64(cc.OrchLSP)
			case "Agent", "Task":
				s.Sub++
				if main {
					s.MainSub++
				}
			}
		}
	case "result":
		s.Result = true
		s.IsError = ev.IsError
		s.Cost = ev.Cost
		s.Turns = ev.Turns
		s.In, s.Out, s.CacheRead, s.CacheWrite = 0, 0, 0, 0
		if len(ev.ModelUsage) > 0 {
			for _, m := range ev.ModelUsage {
				s.In += benchNumber(m["inputTokens"], 0)
				s.Out += benchNumber(m["outputTokens"], 0)
				s.CacheRead += benchNumber(m["cacheReadInputTokens"], 0)
				s.CacheWrite += benchNumber(m["cacheCreationInputTokens"], 0)
			}
		} else {
			s.In = benchNumber(ev.Usage["input_tokens"], 0)
			s.Out = benchNumber(ev.Usage["output_tokens"], 0)
			s.CacheRead = benchNumber(ev.Usage["cache_read_input_tokens"], 0)
			s.CacheWrite = benchNumber(ev.Usage["cache_creation_input_tokens"], 0)
		}
	}
}

func benchBool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// benchReplayStep runs sh -c in dir with a time limit, killing the process group on expiry.
func benchReplayStep(dir, script string, limit time.Duration, out io.Writer) (timedOut bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := processCommandContext(ctx, "sh", "-c", script)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Run()
	return ctx.Err() != nil, err
}

func benchReplayCell1(a *app, suite, repo string, c benchReplayCell, keep bool, stdout io.Writer) (*benchRun, error) {
	label := c.label()
	wtParent := filepath.Join(benchReplayCacheRoot(), "bench-wt", suite)
	if e := os.MkdirAll(wtParent, 0o700); e != nil {
		return nil, e
	}
	stage, e := os.MkdirTemp(wtParent, ".stage-"+label+"-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(stage)
	wt, e := os.MkdirTemp(wtParent, label+"-")
	if e != nil {
		return nil, e
	}
	var warnings []string
	if _, e := benchGit(repo, "worktree", "add", "--detach", wt, c.Task.Commit+"^"); e != nil {
		os.Remove(wt)
		return nil, e
	}
	defer func() {
		if keep {
			fmt.Fprintf(stdout, "kept worktree %s\n", wt)
			return
		}
		if _, e := benchGit(repo, "worktree", "remove", "--force", wt); e != nil {
			fmt.Fprintf(stdout, "could not remove worktree %s: %v\n", wt, e)
		}
	}()
	files := map[string]string{
		"stream": filepath.Join(stage, "stream.jsonl"), "stderr": filepath.Join(stage, "stderr.txt"),
		"patch": filepath.Join(stage, "agent.patch"), "check_log": filepath.Join(stage, "check.log"),
	}
	m := map[string]benchMetric{}
	put := func(name string, v float64, unit, better string) {
		m[name] = benchMetric{Value: v, Unit: unit, Better: better}
	}
	session := newClaudeSessionID()
	transcript := claudeTranscript(evalOr(wt), session)
	setupFailed, agentErr, timedOut := false, false, false
	var wall time.Duration
	if c.Task.Setup != "" {
		var buf bytes.Buffer
		to, e := benchReplayStep(wt, c.Task.Setup, benchReplayStepTimeout, &buf)
		if e != nil || to {
			setupFailed = true
			reason := "timeout"
			if e != nil {
				reason = e.Error()
			}
			warnings = append(warnings, "setup failed: "+reason)
			_ = os.WriteFile(files["stderr"], buf.Bytes(), 0o600)
		}
	}
	if !setupFailed {
		so, e := os.Create(files["stream"])
		if e != nil {
			return nil, e
		}
		se, e := os.Create(files["stderr"])
		if e != nil {
			so.Close()
			return nil, e
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.Arm.TimeoutMinute)*time.Minute)
		cmd := processCommandContext(ctx, runnerBinary("claude"), benchReplayArgv(c, session)...)
		cmd.Dir = wt
		cmd.Env = append(append(os.Environ(), benchReplayEnv(c.Arm.Env)...), "ORCH_TOOL_GUARD=off", "ORCH_LOOKUP_COACH=off", "ORCH_LOOKUP_LOG=off")
		cmd.Stdout, cmd.Stderr = so, se
		start := time.Now()
		runErr := cmd.Run()
		wall = time.Since(start)
		timedOut = ctx.Err() != nil
		cancel()
		so.Close()
		se.Close()
		if runErr != nil {
			agentErr = true
			warnings = append(warnings, "agent error: "+runErr.Error())
		}
		if timedOut {
			warnings = append(warnings, fmt.Sprintf("agent timed out after %d minutes", c.Arm.TimeoutMinute))
		}
	} else {
		agentErr = true
	}
	var st benchStreamStats
	if f, e := os.Open(files["stream"]); e == nil {
		st = benchParseStream(f)
		f.Close()
	}
	if !setupFailed && !st.Result {
		agentErr = true
	}
	if st.IsError {
		agentErr = true
		warnings = append(warnings, "agent reported an error result")
	}
	// Patch and touched files.
	var touched []string
	if !setupFailed {
		_, _ = benchGit(wt, "add", "-A", "-N")
		patch, e := benchGit(wt, "diff")
		if e == nil {
			_ = os.WriteFile(files["patch"], []byte(patch+"\n"), 0o600)
		}
		if names, e := benchGit(wt, "diff", "--name-only"); e == nil && names != "" {
			touched = strings.Split(names, "\n")
		}
	}
	solution := map[string]bool{}
	if names, e := benchGit(repo, "diff", "--name-only", c.Task.Commit+"^", c.Task.Commit); e == nil && names != "" {
		for n := range strings.SplitSeq(names, "\n") {
			solution[n] = true
		}
	}
	outside := 0
	for _, n := range touched {
		if !solution[n] {
			outside++
		}
	}
	// Hidden tests, then the check.
	pass, collisions := false, 0
	if !setupFailed {
		var chk bytes.Buffer
		if len(c.Task.Hidden) > 0 {
			for _, r := range benchRemoveCollidingTests(wt, c.Task.Commit, c.Task.Hidden) {
				collisions++
				warnings = append(warnings, "removed agent test colliding with hidden tests: "+r)
			}
			args := append([]string{"checkout", c.Task.Commit, "--"}, c.Task.Hidden...)
			if _, e := benchGit(wt, args...); e != nil {
				fmt.Fprintf(&chk, "restore hidden tests failed: %v\n", e)
				warnings = append(warnings, "restore hidden tests failed")
			}
		}
		to, e := benchReplayStep(wt, c.Task.Check, benchReplayStepTimeout, &chk)
		pass = e == nil && !to && !strings.Contains(chk.String(), "restore hidden tests failed")
		if to {
			warnings = append(warnings, "check timed out")
		}
		_ = os.WriteFile(files["check_log"], chk.Bytes(), 0o600)
	}
	put("pass", benchBool(pass), "bool", "higher")
	put("cost_usd", st.Cost, "usd", "lower")
	put("tokens_total", st.In+st.Out+st.CacheRead+st.CacheWrite, "tokens", "lower")
	put("tokens_input", st.In, "tokens", "lower")
	put("tokens_cache_read", st.CacheRead, "tokens", "lower")
	put("tokens_cache_write", st.CacheWrite, "tokens", "lower")
	put("tokens_output", st.Out, "tokens", "lower")
	put("turns", st.Turns, "turns", "lower")
	put("wall_seconds", wall.Seconds(), "s", "lower")
	put("tool_calls", st.Tools, "calls", "lower")
	put("read_calls", st.Read, "calls", "lower")
	put("search_calls", st.Search, "calls", "lower")
	put("shell_edits", st.ShellEdits, "calls", "lower")
	put("orch_lsp_calls", st.OrchLSP, "calls", "lower")
	put("toolsearch_calls", st.ToolSearch, "calls", "lower")
	put("hidden_collisions", float64(collisions), "files", "lower")
	tr := benchParseTranscript(transcript)
	if tr.Found {
		put("diagnostics_pushed", tr.Diagnostics, "events", "none")
		put("diagnostics_errors", tr.DiagErrors, "entries", "lower")
		put("hook_context_events", tr.Hook, "events", "none")
		if st.InitSeen && st.LSPListed {
			put("lsp_visible", benchBool(!tr.LSPDeferred), "bool", "none")
		}
	}
	if st.InitSeen && st.LSPListed {
		put("lsp_loaded_via_toolsearch", benchBool(st.LSPViaToolSearch), "bool", "none")
	}
	put("lsp_calls", st.LSP, "calls", "lower")
	put("bash_calls", st.Bash, "calls", "lower")
	put("build_test_calls", st.Build, "calls", "lower")
	put("subagent_calls", st.Sub, "calls", "lower")
	put("main_tool_calls", st.MainTools, "calls", "lower")
	put("main_read_calls", st.MainRead, "calls", "lower")
	put("main_lsp_calls", st.MainLSP, "calls", "lower")
	put("main_bash_calls", st.MainBash, "calls", "lower")
	put("main_subagent_calls", st.MainSub, "calls", "lower")
	put("files_touched", float64(len(touched)), "files", "none")
	put("files_outside_solution", float64(outside), "files", "lower")
	put("timed_out", benchBool(timedOut), "bool", "lower")
	put("agent_error", benchBool(agentErr), "bool", "lower")
	note := fmt.Sprintf("task %s commit %s repeat %d", c.Task.ID, c.Task.Commit, c.Repeat)
	return benchRecordReplay(a, suite, repo, label, c.Arm.Name, c.Arm.Def, note, m, warnings, files, transcript)
}

func benchReplayEnv(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// benchRecordReplay publishes one replay cell as a run. Files maps the raw name
// (stream, stderr, patch, check_log) to a staged file copied into snapshot/.
func benchRecordReplay(a *app, suite, repo, label, arm string, def map[string]any, note string, metrics map[string]benchMetric, warnings []string, files map[string]string, transcript string) (*benchRun, error) {
	for _, n := range []string{suite, label, arm} {
		if e := benchName(n); e != nil {
			return nil, e
		}
	}
	root := filepath.Join(benchDir(), suite)
	if e := benchMkdir(root); e != nil {
		return nil, e
	}
	var result *benchRun
	e := benchWithLock(filepath.Join(root, ".lock"), func() error {
		at := a.now().UTC()
		id := at.Format("20060102-1504") + "-" + benchPrefix(label, 50)
		base := id
		for n := 2; benchExists(filepath.Join(root, "runs", id)); n++ {
			suffix := "-" + strconv.Itoa(n)
			id = benchPrefix(base, 64-len(suffix)) + suffix
		}
		if e := benchMkdir(filepath.Join(root, "runs")); e != nil {
			return e
		}
		tmp, e := os.MkdirTemp(filepath.Join(root, "runs"), ".record-")
		if e != nil {
			return e
		}
		defer os.RemoveAll(tmp)
		snap := filepath.Join(tmp, "snapshot")
		if e := benchMkdir(snap); e != nil {
			return e
		}
		dest := filepath.Join(root, "runs", id)
		if e := benchSafe(dest); e != nil {
			return e
		}
		raw := map[string]string{}
		names := map[string]string{"stream": "stream.jsonl", "stderr": "stderr.txt", "patch": "agent.patch", "check_log": "check.log"}
		for key, src := range files {
			b, e := os.ReadFile(src)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return e
			}
			if e := benchWrite(filepath.Join(snap, names[key]), b); e != nil {
				return e
			}
			raw[key] = filepath.Join(dest, "snapshot", names[key])
		}
		raw["transcript"] = transcript
		r := &benchRun{SchemaVersion: 2, ID: id, Label: label, Suite: suite, Arm: arm, CreatedAt: at,
			Build: benchBuild{Version: "unknown"}, Inputs: benchInputs{Repo: repo}, Groups: map[string]benchGroup{}, Scores: map[string]benchScore{},
			Raw: raw, Metrics: metrics, ArmDefinition: def, Kind: "replay", Note: note, Warnings: warnings}
		if e := benchJSON(filepath.Join(tmp, "run.json"), r); e != nil {
			return e
		}
		if e := benchVerifyRun(r, snap); e != nil {
			return e
		}
		if e := os.Rename(tmp, dest); e != nil {
			return e
		}
		r.dir = dest
		result = r
		return nil
	})
	if e != nil {
		return nil, e
	}
	if e := benchReadme(); e != nil {
		return nil, e
	}
	return result, nil
}

type benchReplayRow struct {
	Task      string  `json:"task,omitempty"`
	Arm       string  `json:"arm"`
	N         int     `json:"n"`
	Pass      int     `json:"pass"`
	CostUSD   float64 `json:"median_cost_usd"`
	Tokens    float64 `json:"median_tokens_total"`
	Turns     float64 `json:"median_turns,omitempty"`
	Wall      float64 `json:"median_wall_seconds,omitempty"`
	LSP       float64 `json:"median_lsp_calls,omitempty"`
	BuildTest float64 `json:"median_build_test_calls,omitempty"`
	Search    float64 `json:"median_search_calls,omitempty"`
	Read      float64 `json:"median_read_calls,omitempty"`
	Diag      float64 `json:"median_diagnostics_pushed,omitempty"`
}

func benchMedian(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func benchReplaySummary(suite string) (tasks, arms []benchReplayRow, err error) {
	runs, e := benchRuns(suite)
	if e != nil {
		return nil, nil, e
	}
	type key struct{ task, arm string }
	group := map[key][]*benchRun{}
	byArm := map[string][]*benchRun{}
	for _, r := range runs {
		if r.Kind != "replay" {
			continue
		}
		f := strings.Fields(r.Note)
		task := ""
		if len(f) >= 2 && f[0] == "task" {
			task = f[1]
		}
		group[key{task, r.Arm}] = append(group[key{task, r.Arm}], r)
		byArm[r.Arm] = append(byArm[r.Arm], r)
	}
	med := func(rs []*benchRun, name string) float64 {
		var v []float64
		for _, r := range rs {
			if m, ok := r.Metrics[name]; ok {
				v = append(v, m.Value)
			}
		}
		return benchMedian(v)
	}
	passes := func(rs []*benchRun) int {
		n := 0
		for _, r := range rs {
			if r.Metrics["pass"].Value == 1 {
				n++
			}
		}
		return n
	}
	var keys []key
	for k := range group {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].task != keys[j].task {
			return keys[i].task < keys[j].task
		}
		return keys[i].arm < keys[j].arm
	})
	for _, k := range keys {
		rs := group[k]
		tasks = append(tasks, benchReplayRow{Task: k.task, Arm: k.arm, N: len(rs), Pass: passes(rs), CostUSD: med(rs, "cost_usd"), Tokens: med(rs, "tokens_total"),
			Turns: med(rs, "turns"), Wall: med(rs, "wall_seconds"), LSP: med(rs, "lsp_calls"), BuildTest: med(rs, "build_test_calls"),
			Search: med(rs, "search_calls"), Read: med(rs, "read_calls"), Diag: med(rs, "diagnostics_pushed")})
	}
	var names []string
	for n := range byArm {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rs := byArm[n]
		arms = append(arms, benchReplayRow{Arm: n, N: len(rs), Pass: passes(rs), CostUSD: med(rs, "cost_usd"), Tokens: med(rs, "tokens_total")})
	}
	return tasks, arms, nil
}

func benchReplayPrintSummary(a *app, suite string, asJSON bool) error {
	tasks, arms, e := benchReplaySummary(suite)
	if e != nil {
		return e
	}
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{"suite": suite, "tasks": tasks, "arms": arms}, "", "  ")
		fmt.Fprintln(a.out, string(b))
		return nil
	}
	if len(tasks) == 0 {
		fmt.Fprintln(a.out, "no replay runs recorded for suite "+suite)
		return nil
	}
	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tARM\tN\tPASS\tCOST_USD\tTOKENS\tTURNS\tWALL_S\tLSP\tBUILD_TEST\tSEARCH\tREAD\tDIAG")
	for _, r := range tasks {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d/%d\t%.4f\t%.0f\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\n", r.Task, r.Arm, r.N, r.Pass, r.N, r.CostUSD, r.Tokens, r.Turns, r.Wall, r.LSP, r.BuildTest, r.Search, r.Read, r.Diag)
	}
	tw.Flush()
	fmt.Fprintln(a.out)
	tw = tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ARM\tN\tPASS_RATE\tMEDIAN_COST_USD\tMEDIAN_TOKENS")
	for _, r := range arms {
		fmt.Fprintf(tw, "%s\t%d\t%.0f%% (%d/%d)\t%.4f\t%.0f\n", r.Arm, r.N, 100*float64(r.Pass)/float64(r.N), r.Pass, r.N, r.CostUSD, r.Tokens)
	}
	return tw.Flush()
}

func cmdBenchRun(a *app, args []string) error {
	fs := a.flags("bench run")
	tasksF := fs.String("tasks", "", "comma-separated task ids (default all)")
	armsF := fs.String("arms", "", "comma-separated replay arms (default all)")
	repeats := fs.Int("repeats", 1, "repeats per task and arm")
	parallel := fs.Int("parallel", 2, "cells run at once")
	keep := fs.Bool("keep-worktrees", false, "keep each cell's worktree")
	dry := fs.Bool("dry-run", false, "validate and print the cells; run nothing")
	summary := fs.Bool("summary", false, "print the summary of stored replay runs; run nothing")
	asJSON := fs.Bool("json", false, "print the summary as JSON")
	pos, e := parseArgs(fs, args)
	if e != nil {
		return e
	}
	if e := exactArgs(pos, 1, "bench run <suite> [--tasks a,b] [--arms x,y] [--repeats N] [--parallel N] [--keep-worktrees] [--dry-run] [--summary] [--json]"); e != nil {
		return e
	}
	suite := pos[0]
	if *repeats < 1 || *parallel < 1 {
		return fail("--repeats and --parallel must be at least 1")
	}
	if *summary {
		if _, e := benchSuiteLoad(suite); e != nil {
			return e
		}
		return benchReplayPrintSummary(a, suite, *asJSON)
	}
	split := func(s string) []string {
		var out []string
		for p := range strings.SplitSeq(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	repo, cells, e := benchReplayPlan(suite, split(*tasksF), split(*armsF), *repeats)
	if e != nil {
		return e
	}
	if *dry {
		bin := runnerBinary("claude")
		for _, c := range cells {
			argv := benchReplayArgv(c, "<session-id>")
			q := make([]string, len(argv))
			for i, s := range argv {
				q[i] = strconv.Quote(s)
			}
			fmt.Fprintf(a.out, "cell %s (task %s commit %s arm %s repeat %d)\n  %s %s\n", c.label(), c.Task.ID, c.Task.Commit, c.Arm.Name, c.Repeat, bin, strings.Join(q, " "))
		}
		return nil
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	work := make(chan benchReplayCell)
	failed := 0
	for i := 0; i < *parallel; i++ {
		wg.Go(func() {
			for c := range work {
				var buf bytes.Buffer
				r, e := benchReplayCell1(a, suite, repo, c, *keep, &buf)
				mu.Lock()
				if e != nil {
					failed++
					fmt.Fprintf(a.out, "cell %s: error: %v\n", c.label(), e)
				} else {
					fmt.Fprintf(a.out, "cell %s: pass=%.0f cost=$%.4f -> %s/%s\n", c.label(), r.Metrics["pass"].Value, r.Metrics["cost_usd"].Value, r.Suite, r.ID)
				}
				a.out.Write(buf.Bytes())
				mu.Unlock()
			}
		})
	}
	for _, c := range cells {
		work <- c
	}
	close(work)
	wg.Wait()
	fmt.Fprintln(a.out)
	if e := benchReplayPrintSummary(a, suite, *asJSON); e != nil {
		return e
	}
	if failed > 0 {
		return fail("%d of %d cells could not be recorded", failed, len(cells))
	}
	return nil
}

// benchCmdCounts is what one Bash command line does, by segment.
type benchCmdCounts struct{ Reads, Searches, Edits, OrchLSP int }

type benchSeg struct {
	text      string
	afterPipe bool
}

// benchSplitCommand splits a shell line on ; && || | & and newlines outside
// quotes, dropping heredoc bodies.
func benchSplitCommand(cmd string) []benchSeg {
	var segs []benchSeg
	var cur strings.Builder
	pipe := false
	flush := func() {
		if strings.TrimSpace(cur.String()) != "" {
			segs = append(segs, benchSeg{cur.String(), pipe})
		}
		cur.Reset()
		pipe = false
	}
	r := []rune(cmd)
	var quote rune
	var heredocs []string
	for i := 0; i < len(r); i++ {
		c := r[i]
		if quote != 0 {
			cur.WriteRune(c)
			if c == '\\' && quote == '"' && i+1 < len(r) {
				i++
				cur.WriteRune(r[i])
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '\\' && i+1 < len(r):
			cur.WriteRune(c)
			i++
			cur.WriteRune(r[i])
		case c == '\'' || c == '"':
			quote = c
			cur.WriteRune(c)
		case c == '<' && i+1 < len(r) && r[i+1] == '<' && (i+2 >= len(r) || r[i+2] != '<'):
			j := i + 2
			if j < len(r) && r[j] == '-' {
				j++
			}
			for j < len(r) && (r[j] == ' ' || r[j] == '\t') {
				j++
			}
			k := j
			for k < len(r) && !strings.ContainsRune(" \t\n;&|<>()", r[k]) {
				k++
			}
			if k > j {
				heredocs = append(heredocs, strings.Trim(string(r[j:k]), `'"\`))
			}
			cur.WriteString("<<")
			i++
		case c == '\n':
			flush()
			for len(heredocs) > 0 {
				d := heredocs[0]
				heredocs = heredocs[1:]
				for i+1 < len(r) {
					end := i + 1
					for end < len(r) && r[end] != '\n' {
						end++
					}
					line := strings.TrimSpace(string(r[i+1 : end]))
					i = end
					if line == d {
						break
					}
				}
			}
		case c == ';':
			flush()
		case c == '&' && i+1 < len(r) && r[i+1] == '&':
			i++
			flush()
		case c == '|' && i+1 < len(r) && r[i+1] == '|':
			i++
			flush()
		case c == '|':
			flush()
			pipe = true
		case c == '&' && (i+1 < len(r) && r[i+1] == '>' || i > 0 && r[i-1] == '>'):
			cur.WriteRune(c)
		case c == '&':
			flush()
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return segs
}

type benchTok struct{ raw, s string }

func benchTokenize(seg string) []benchTok {
	var out []benchTok
	var raw, val strings.Builder
	var quote rune
	have := false
	end := func() {
		if have {
			out = append(out, benchTok{raw.String(), val.String()})
		}
		raw.Reset()
		val.Reset()
		have = false
	}
	r := []rune(seg)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(r) {
				i++
				val.WriteRune(r[i])
			} else {
				val.WriteRune(c)
			}
			raw.WriteRune(c)
		case c == '\'' || c == '"':
			quote, have = c, true
			raw.WriteRune(c)
		case c == '\\' && i+1 < len(r):
			i++
			have = true
			raw.WriteRune(c)
			raw.WriteRune(r[i])
			val.WriteRune(r[i])
		case c == ' ' || c == '\t':
			end()
		default:
			have = true
			raw.WriteRune(c)
			val.WriteRune(c)
		}
	}
	end()
	return out
}

// benchSegArgs separates a segment's words from its redirections.
func benchSegArgs(toks []benchTok) (words []string, outTargets []string) {
	for i := 0; i < len(toks); i++ {
		raw, s := toks[i].raw, toks[i].s
		p := 0
		for p < len(raw) && raw[p] >= '0' && raw[p] <= '9' {
			p++
		}
		if strings.HasPrefix(raw, "&>") {
			p = 1
		}
		if p < len(raw) && (raw[p] == '>' || raw[p] == '<') {
			dir := raw[p]
			q := p
			for q < len(raw) && (raw[q] == dir || raw[q] == '|' || raw[q] == '-' && dir == '<') {
				q++
			}
			target := s[q:]
			if target == "" && i+1 < len(toks) {
				i++
				target = toks[i].s
			}
			if dir == '>' && target != "" {
				outTargets = append(outTargets, target)
			}
			continue
		}
		words = append(words, s)
	}
	return words, outTargets
}

var benchValueFlags = map[string]bool{"-g": true, "--glob": true, "-t": true, "--type": true, "-T": true, "--type-not": true, "-A": true, "-B": true,
	"-C": true, "-m": true, "--max-count": true, "-d": true, "--max-depth": true, "-j": true, "--threads": true, "-r": true, "--replace": true,
	"--include": true, "--exclude": true, "--exclude-dir": true, "--max-columns": true, "--iglob": true}

// benchNonFlags returns the non-flag words and whether -e/-f/--regexp/--file supplied the pattern.
func benchNonFlags(args []string, valueFlags map[string]bool) (rest []string, patternFlag bool) {
	done := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case done:
			rest = append(rest, a)
		case a == "--":
			done = true
		case a == "-e" || a == "-f" || a == "--regexp" || a == "--file":
			patternFlag = true
			i++
		case a == "-" || !strings.HasPrefix(a, "-"):
			rest = append(rest, a)
		case valueFlags[a]:
			i++
		}
	}
	return rest, patternFlag
}

func benchShortFlagHas(args []string, c rune) bool {
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a[1:], c) {
			return true
		}
	}
	return false
}

// benchClassifyCommand counts file reads, searches, file writes and `orch lsp`
// calls in a Bash command line. A pipeline stage that only filters stdin does
// not count as a read or search.
func benchClassifyCommand(cmd string) benchCmdCounts {
	var cc benchCmdCounts
	for _, sg := range benchSplitCommand(cmd) {
		words, outs := benchSegArgs(benchTokenize(sg.text))
		for len(words) > 0 {
			w := words[0]
			if strings.Contains(w, "=") && !strings.HasPrefix(w, "-") && !strings.Contains(strings.SplitN(w, "=", 2)[0], "/") ||
				w == "time" || w == "then" || w == "do" || w == "else" || w == "if" || w == "while" || w == "!" || w == "command" {
				words = words[1:]
				continue
			}
			break
		}
		if len(words) > 0 {
			words[0] = strings.TrimLeft(words[0], "({")
		}
		for _, o := range outs {
			if !strings.HasPrefix(o, "&") && !strings.HasPrefix(o, "/dev/") && !strings.HasPrefix(o, "/tmp/") && !strings.HasPrefix(o, "/var/") && !benchNonSourceRE.MatchString(o) {
				cc.Edits++
				break
			}
		}
		if len(words) == 0 {
			continue
		}
		name, args := filepath.Base(words[0]), words[1:]
		switch name {
		case "cat", "less", "nl":
			if f, _ := benchNonFlags(args, nil); len(f) > 0 {
				cc.Reads++
			}
		case "head", "tail":
			if f, _ := benchNonFlags(args, map[string]bool{"-n": true, "-c": true}); len(f) > 0 {
				cc.Reads++
			}
		case "sed":
			f, pat := benchNonFlags(args, map[string]bool{"-e": true})
			switch {
			case benchShortFlagHas(args, 'i') || slicesContains(args, "--in-place"):
				cc.Edits++
			case benchShortFlagHas(args, 'n'):
				need := 2
				if pat || slicesContains(args, "-e") {
					need = 1
				}
				if len(f) >= need {
					cc.Reads++
				}
			}
		case "perl":
			if benchShortFlagHas(args, 'i') {
				cc.Edits++
			}
		case "tee":
			if f, _ := benchNonFlags(args, nil); len(f) > 0 {
				cc.Edits++
			}
		case "rg", "grep", "egrep", "fgrep", "ag":
			benchCountSearch(&cc, args, sg.afterPipe)
		case "git":
			if f, _ := benchNonFlags(args, map[string]bool{"-C": true, "-c": true}); len(f) > 0 && f[0] == "grep" {
				var rest []string
				for i, a := range args {
					if a == "grep" {
						rest = args[i+1:]
						break
					}
				}
				benchCountSearch(&cc, rest, sg.afterPipe)
			}
		case "orch":
			if len(args) > 0 && args[0] == "lsp" {
				cc.OrchLSP++
			}
		}
	}
	return cc
}

var benchNonSourceRE = regexp.MustCompile(`\.(log|out|txt|diff|patch)$`)

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func benchCountSearch(cc *benchCmdCounts, args []string, afterPipe bool) {
	f, pat := benchNonFlags(args, benchValueFlags)
	paths := len(f)
	if !pat && paths > 0 {
		paths--
	}
	if !afterPipe || paths > 0 {
		cc.Searches++
	}
}

// benchTranscriptStats are the counts taken from a Claude Code session transcript.
type benchTranscriptStats struct {
	Found                         bool
	Diagnostics, DiagErrors, Hook float64
	LSPDeferred                   bool
}

func benchParseTranscript(path string) benchTranscriptStats {
	var t benchTranscriptStats
	f, e := os.Open(path)
	if e != nil {
		return t
	}
	defer f.Close()
	t.Found = true
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, e := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 && bytes.Contains(line, []byte(`"attachment"`)) {
			var ev struct {
				Attachment struct {
					Type  string `json:"type"`
					Files []struct {
						Diagnostics []struct {
							Severity string `json:"severity"`
						} `json:"diagnostics"`
					} `json:"files"`
					AddedNames []string        `json:"addedNames"`
					Content    json.RawMessage `json:"content"`
				} `json:"attachment"`
			}
			if json.Unmarshal(line, &ev) == nil {
				a := ev.Attachment
				switch a.Type {
				case "diagnostics":
					t.Diagnostics++
					for _, fl := range a.Files {
						for _, d := range fl.Diagnostics {
							if d.Severity == "Error" {
								t.DiagErrors++
							}
						}
					}
				case "hook_additional_context":
					if c := strings.TrimSpace(string(a.Content)); c != "" && c != "null" && c != "[]" && c != `""` {
						t.Hook++
					}
				case "deferred_tools_delta", "deferred_tools_record":
					if slicesContains(a.AddedNames, "LSP") {
						t.LSPDeferred = true
					}
				}
			}
		}
		if e != nil {
			break
		}
	}
	return t
}

// benchGoDecls returns the package name and top-level names a Go file declares.
func benchGoDecls(name string, src []byte) (string, map[string]bool) {
	f, e := parser.ParseFile(token.NewFileSet(), name, src, parser.SkipObjectResolution)
	if e != nil || f == nil {
		return "", nil
	}
	names := map[string]bool{}
	add := func(n string) {
		if n != "_" && n != "init" {
			names[n] = true
		}
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				add(d.Name.Name)
			}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					add(sp.Name.Name)
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						add(n.Name)
					}
				}
			}
		}
	}
	return f.Name.Name, names
}

// benchRemoveCollidingTests deletes agent-added _test.go files that declare a
// top-level name also declared (in the same package and directory) by a hidden
// Go file, which would stop the package from building once hidden tests return.
func benchRemoveCollidingTests(wt, commit string, hidden []string) (removed []string) {
	type decl struct {
		pkg   string
		names map[string]bool
	}
	byDir := map[string][]decl{}
	hiddenSet := map[string]bool{}
	for _, h := range hidden {
		hiddenSet[filepath.ToSlash(h)] = true
		if !strings.HasSuffix(h, ".go") {
			continue
		}
		src, e := benchGit(wt, "show", commit+":"+h)
		if e != nil {
			continue
		}
		if pkg, names := benchGoDecls(h, []byte(src)); names != nil {
			d := filepath.ToSlash(filepath.Dir(h))
			byDir[d] = append(byDir[d], decl{pkg, names})
		}
	}
	var dirs []string
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		entries, e := os.ReadDir(filepath.Join(wt, filepath.FromSlash(d)))
		if e != nil {
			continue
		}
		for _, en := range entries {
			rel := path.Join(d, en.Name())
			if en.IsDir() || !strings.HasSuffix(en.Name(), "_test.go") || hiddenSet[rel] {
				continue
			}
			if _, e := benchGit(wt, "cat-file", "-e", commit+"^:"+rel); e == nil {
				continue // existed before the change
			}
			src, e := os.ReadFile(filepath.Join(wt, filepath.FromSlash(rel)))
			if e != nil {
				continue
			}
			pkg, names := benchGoDecls(rel, src)
			var clash []string
			for _, h := range byDir[d] {
				if h.pkg != pkg {
					continue
				}
				for n := range names {
					if h.names[n] {
						clash = append(clash, n)
					}
				}
			}
			if len(clash) > 0 && os.Remove(filepath.Join(wt, filepath.FromSlash(rel))) == nil {
				sort.Strings(clash)
				removed = append(removed, rel+" ("+strings.Join(clash, ", ")+")")
			}
		}
	}
	return removed
}
