package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Cancel the whole runner/check process group, including children that keep
// output pipes open after their parent exits.
func processCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	return cmd
}

var modelRoles = []string{"gate", "spec_queries", "decider", "gate_adjudicator", "question_classifier"}

type modelRole struct {
	Enabled          *bool               `toml:"enabled"`
	Use              []profile           `toml:"use"`
	Budget           *adjudicationLimits `toml:"budget"`
	Promote          bool                `toml:"promote"`
	ClassifierLimits *classifierLimits   `toml:"classifier_limits"`
}

func runnerBinary(h string) string {
	if h == "claude" {
		return envOr("ORCH_CLAUDE", "claude")
	}
	return envOr("ORCH_PI", "pi")
}

func installed(h string) bool { _, err := exec.LookPath(runnerBinary(h)); return err == nil }

// Defaults follow the machine, not a mandatory pi installation. Optional judges
// have no Claude substitute: their absence must not prevent the main passes.
func (s site) role(name string) modelRole {
	if r, ok := s.cfg.Roles[name]; ok {
		if name == "decider" && s.cfg.TriageModel != "" {
			r.Use = append([]profile{}, r.Use...)
			for i := range r.Use {
				r.Use[i].Model = s.cfg.TriageModel
			}
		}
		return r
	}
	var r modelRole
	if name == "question_classifier" {
		// Inherit the connector list, not triage's enabled switch or behavior.
		// Explicit classifier configuration always wins over this default.
		r.Use = append([]profile(nil), s.role("decider").Use...)
		return r
	}
	if name == "gate_adjudicator" {
		disabled := false
		r.Enabled = &disabled
		return r
	}
	if name == "decider" {
		if installed("pi") || os.Getenv("OPENROUTER_API_KEY") != "" || os.Getenv("ORCH_TRIAGE_KEY") != "" {
			r.Use = []profile{{Harness: "api", Provider: "openrouter", Model: orDefault(s.cfg.TriageModel, triageModelDefault)}}
		}
		return r
	}
	if installed("pi") {
		r.Use = append(r.Use, profile{Harness: "pi", Model: gateModelDef})
	}
	if installed("claude") {
		r.Use = append(r.Use, profile{Harness: "claude", Model: "haiku", Effort: "low"})
	}
	return r
}

func parseRunners(text string) ([]profile, error) {
	var out []profile
	for _, choice := range strings.Split(text, ",") {
		fields := strings.Split(strings.TrimSpace(choice), ":")
		if len(fields) < 2 || len(fields) > 3 || (fields[0] != "pi" && fields[0] != "claude") || fields[1] == "" {
			return nil, fail("--runner needs harness:model[:effort],... (harness pi or claude), not %q", choice)
		}
		p := profile{Harness: fields[0], Model: fields[1]}
		if len(fields) == 3 {
			p.Effort = fields[2]
		}
		out = append(out, p)
	}
	return out, nil
}

type modelOptions struct {
	Runner        string
	AllowDegraded bool
}

type roleRun struct {
	s             site
	role          string
	choices       []profile
	enabled       bool
	allowDegraded bool
	mu            sync.Mutex
	down          map[string]string
	notes         []string
	keys          map[string]keyReadiness
	now           func() time.Time
}

func (s site) newRoleRun(name string, opts modelOptions) (*roleRun, error) {
	r := s.role(name)
	rr := &roleRun{s: s, role: name, choices: r.Use, enabled: r.Enabled == nil || *r.Enabled, allowDegraded: opts.AllowDegraded, down: map[string]string{}}
	if opts.Runner != "" {
		var err error
		rr.choices, err = parseRunners(opts.Runner)
		if err != nil {
			return nil, err
		}
	}
	return rr, nil
}

func (r *roleRun) state(p profile) string {
	if !r.enabled {
		return "disabled"
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	if p.Degraded && !r.allowDegraded {
		return "not allowed (degraded; needs --degraded)"
	}
	if p.Harness != "api" {
		if err := r.s.cfg.modelAllowed(p.Harness, p.Model, now); err != nil {
			return "not allowed: " + err.Error()
		}
		if !installed(p.Harness) {
			return "not installed"
		}
	}
	if parent, _, ok := limitedBy(loadHealth(now), p.lane()); ok {
		return "limited (" + parent + ")"
	}
	provider := p.Provider
	if p.Harness == "pi" {
		provider, _, _ = strings.Cut(p.Model, "/")
		if !strings.Contains(p.Model, "/") {
			provider = ""
		}
	}
	if provider != "" {
		key := r.providerReadiness(provider)
		if key.state != "ok" && laneWithin(p.lane(), key.lane) {
			return key.state
		}
	}
	return "ok"
}

func (r *roleRun) available() ([]profile, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var choices []profile
	var skipped []string
	for _, p := range r.choices {
		state := r.state(p)
		for down, why := range r.down {
			if laneWithin(p.lane(), down) {
				state = why
				break
			}
		}
		if state == "ok" {
			choices = append(choices, p)
		} else {
			skipped = append(skipped, p.String()+": "+state)
		}
	}
	if !r.enabled && len(skipped) == 0 {
		skipped = append(skipped, "disabled")
	}
	if len(r.choices) == 0 {
		skipped = append(skipped, "not available")
	}
	return choices, skipped
}

func (r *roleRun) unavailable() error {
	_, reasons := r.available()
	return fail("%s off: %s", r.role, strings.Join(reasons, "; "))
}

var providerStatusRe = regexp.MustCompile(`\b(?:401|402|403|429|5[0-9][0-9])\b`)

func providerDown(err error) bool {
	if err == nil {
		return false
	}
	var limit *passLimit
	if errors.As(err, &limit) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var missing *exec.Error
	if errors.As(err, &missing) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, transport := range []string{"connection refused", "connection reset", "request timed out", "connection timed out", "econnrefused", "econnreset", "etimedout"} {
		if strings.Contains(text, transport) {
			return true
		}
	}
	if providerStatusRe.MatchString(text) {
		return true
	}
	for _, match := range []string{"credit", "usage limit", "usage-limit", "rate limit", "rate-limit", "rate_limit", "authentication", "unauthorized", "forbidden", "api key", "quota exceeded", "insufficient_quota", "overloaded", "executable file not found", "command not found", "no such file or directory", "enoent"} {
		if strings.Contains(text, match) {
			return true
		}
	}
	return false
}

// A provider failure is run-local. It never writes the person's health file.
// Each pass gets at most one retry on the next usable choice.
func (r *roleRun) run(fn func(profile) (string, string, error)) (string, string, profile, error) {
	return r.runAfter("", fn)
}

// A repair may use the next available route without marking a replying provider
// down for other passes. With no next route, retain the existing repair choice.
func (r *roleRun) runAfter(after string, fn func(profile) (string, string, error)) (string, string, profile, error) {
	choices, _ := r.available()
	skip := map[string]bool{}
	for i, p := range choices {
		if p.String() == after && i+1 < len(choices) {
			for _, earlier := range choices[:i+1] {
				skip[earlier.String()] = true
			}
			break
		}
	}
	var last error
	var used profile
	for range 2 {
		choices, _ := r.available()
		filtered := choices[:0]
		for _, p := range choices {
			if !skip[p.String()] {
				filtered = append(filtered, p)
			}
		}
		choices = filtered
		if len(choices) == 0 {
			if last != nil {
				return "", "", used, fmt.Errorf("%v; %w", last, r.unavailable())
			}
			return "", "", used, r.unavailable()
		}
		used = choices[0]
		out, hit, err := fn(used)
		if !providerDown(err) {
			return out, hit, used, err
		}
		hint := fmt.Sprintf("orch health set %s limited --for 3h", used.lane())
		last = fmt.Errorf("%s failed: %w; if the limit persists run: %s", used.String(), err, hint)
		fmt.Fprintln(os.Stderr, last)
		r.mu.Lock()
		r.down[used.lane()] = "provider unavailable; " + hint
		r.notes = append(r.notes, last.Error())
		r.mu.Unlock()
	}
	return "", "", used, last
}

// runModel is the tool-less, read-only entry point for small model tasks.
func (s site) runModel(role, prompt string, opts ...modelOptions) (string, error) {
	var opt modelOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	r, err := s.newRoleRun(role, opt)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "orch-model-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	out, _, _, err := r.run(func(p profile) (string, string, error) {
		pass := gatePass{prompt: prompt, noTools: true}
		out, err := runHeadless(s.repo, p, pass, dir)
		if err == nil && strings.TrimSpace(out) == "" {
			err = fmt.Errorf("%s unavailable: empty reply from %s", role, p.String())
		}
		return out, "", err
	})
	return out, err
}

func runHeadless(repo string, p profile, pass gatePass, session string) (string, error) {
	if p.Harness == "claude" {
		return runClaude(repo, p, pass, session)
	}
	pass.effort = p.Effort
	return runPi(repo, p.Model, pass, session)
}

func newClaudeSessionID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}

var claudeProjectRe = regexp.MustCompile(`[^a-zA-Z0-9]`)

func claudeTranscript(repo, id string) string {
	return filepath.Join(expandHome(envOr("ORCH_CLAUDE_PROJECTS", filepath.Join(envOr("CLAUDE_CONFIG_DIR", "~/.claude"), "projects"))), claudeProjectRe.ReplaceAllString(repo, "-"), id+".jsonl")
}

func copyClaudeTranscript(src, session string) {
	if b, err := os.ReadFile(src); err == nil {
		target := filepath.Join(session, "claude.jsonl")
		old, _ := os.ReadFile(target)
		if bytes.Equal(old, b) {
			return
		}
		_ = os.MkdirAll(session, 0o755)
		_ = os.WriteFile(target, b, 0o600)
	}
}

func sessionFailure(dir string) error {
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	newest := ""
	var at time.Time
	for _, path := range files {
		if info, err := os.Stat(path); err == nil && info.ModTime().After(at) {
			newest, at = path, info.ModTime()
		}
	}
	raw, _ := os.ReadFile(newest)
	last := ""
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var event struct {
			Type       string `json:"type"`
			IsAPIError bool   `json:"isApiErrorMessage"`
			Message    struct {
				Role         string `json:"role"`
				ErrorMessage string `json:"errorMessage"`
				Content      []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &event) != nil || event.Message.Role != "assistant" {
			continue
		}
		if event.Type != "message" && event.Type != "message_end" && event.Type != "assistant" {
			continue
		}
		last = event.Message.ErrorMessage
		if event.IsAPIError {
			for _, part := range event.Message.Content {
				last += part.Text
			}
		}
	}
	if last != "" {
		return fmt.Errorf("runner failed: %s", last)
	}
	return nil
}

func claudeSessionReply(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := ""
	eachPiEvent(bytes.NewReader(raw), func(e *piEvent) {
		if e.Type == "assistant" && e.Message.Role == "assistant" {
			text = e.text()
		}
	})
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("claude gave no reply")
	}
	return text, nil
}

func claudeArgs(p profile, noTools bool) []string {
	tools := "Read,Grep,Glob"
	if noTools {
		tools = ""
	}
	args := []string{"--model", p.Model, "--tools", tools, "--permission-mode", "dontAsk", "--disable-slash-commands", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`}
	if tools != "" {
		args = append(args, "--allowedTools", tools)
	}
	if p.Effort != "" {
		args = append(args, "--effort", p.Effort)
	}
	return args
}

func runClaude(repo string, p profile, pass gatePass, session string) (string, error) {
	var prepErr error
	pass, prepErr = gateAgentPass(repo, "claude", pass)
	if prepErr != nil {
		return "", prepErr
	}
	if err := os.MkdirAll(session, 0o755); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(passContext(pass), gateTimeout)
	defer cancel()
	args := append([]string{"-p", "--output-format", "stream-json", "--verbose"}, claudePassArgs(p, pass)...)
	cmd := processCommandContext(ctx, runnerBinary("claude"), append(args, pass.prompt)...)
	defer cmd.Cancel()
	cmd.Dir = repo
	// Saving output as it arrives also makes the existing stall detector usable.
	f, err := os.Create(filepath.Join(session, "claude.json"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	cmd.Stdout = f
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stalled := make(chan struct{})
	poll, stall := gatePoll, gateStall
	go func() {
		last, changed := sessionStamp(session), time.Now()
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if stamp := sessionStamp(session); stamp != last {
					last, changed = stamp, time.Now()
				} else if time.Since(changed) >= stall {
					close(stalled)
					cancel()
					return
				}
			}
		}
	}()
	err = cmd.Run()
	select {
	case <-stalled:
		return "", &passLimit{"stalled: no activity for " + stall.String()}
	default:
	}
	if passContext(pass).Err() != nil {
		return "", passContext(pass).Err()
	}
	if ctx.Err() != nil {
		return "", &passLimit{fmt.Sprintf("timed out after %s", gateTimeout)}
	}
	raw, readErr := os.ReadFile(f.Name())
	if readErr != nil {
		return "", readErr
	}
	var result struct {
		Type    string `json:"type"`
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	seen := false
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if json.Unmarshal(line, &result) == nil && result.Type == "result" {
			seen = true
			break
		}
	}
	if result.IsError {
		return "", fmt.Errorf("claude failed: %s", result.Result)
	}
	if err != nil {
		return "", fmt.Errorf("claude failed: %w: %s %s", err, short(stderr.String(), 300), short(string(raw), 300))
	}
	if !seen {
		return "", fmt.Errorf("claude gave no result")
	}
	return result.Result, nil
}

func printRoleReadiness(w io.Writer, s site) {
	checker := &roleRun{}
	for _, provider := range []string{"openrouter", "openai", "anthropic"} {
		key := checker.providerReadiness(provider)
		scope := key.lane
		if key.unknownScope {
			scope += "; unknown scope"
		}
		fmt.Fprintf(w, "provider %s: %s (%s)\n", provider, key.state, scope)
		if key.err != nil {
			fmt.Fprintf(w, "  key check: %v\n", key.err)
		}
		if key.state == "key empty" {
			fmt.Fprintf(w, "  orch health set %s limited --for 3h\n", key.lane)
		}
	}
	for _, name := range modelRoles {
		r, _ := s.newRoleRun(name, modelOptions{})
		r.keys = checker.keys
		if !r.enabled {
			fmt.Fprintf(w, "role %s: disabled\n", name)
			continue
		}
		if len(r.choices) == 0 {
			fmt.Fprintf(w, "role %s: off: not available\n", name)
			continue
		}
		for _, p := range r.choices {
			state := r.state(p)
			fmt.Fprintf(w, "role %s: %s: %s\n", name, p.String(), state)
		}
	}
	for _, binary := range []string{"pi", "claude", "herdr", "jbcontext", "gopls", "rg"} {
		path := binary
		if binary == "pi" || binary == "claude" {
			path = runnerBinary(binary)
		}
		state := "ok"
		if _, err := exec.LookPath(path); err != nil {
			state = "not installed"
		}
		fmt.Fprintf(w, "binary %s: %s\n", binary, state)
	}
}
