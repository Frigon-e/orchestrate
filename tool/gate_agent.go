package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const lookupBudgetHint = "Look up in this order: `jbcontext search` for an unknown location, `orch lsp def|refs|callers|impls` for a named symbol, `rg` for exact text, `fd` for file names, then read with offset and limit. Send independent lookups in one message."

const gatePacketContract = ` For every unresolved decisive question, include packet from your own review: goal, known_facts (checked state and constraints), evidence_ids, unknown (the exact missing fact or decision), options [{id,text,gain,cost}], recommendation (an option id), rationale, authority (the spec's actual authority basis), and next_action. Do not invent project vocabulary, trade-offs, facts or delegated authority. Leave unavailable fields empty; incomplete packets are research work, not ready owner questions. No formatting pass will fill them.`
const adversaryEvidenceScope = " Follow bounded relevant incoming AND outgoing references (at most two hops and eight additional files), registrations/configuration, and tests. Stop at the existing turn budget, not at forward imports. Include coverage_gaps (an array of strings) alongside findings in your JSON. Name failed/unrun checks explicitly; an empty findings list does not establish complete exploration."

// Keep the original reviewer's bounded coverage report, including a missing
// report. Notes are observations, not evidence of complete repository coverage.
func recordAdversaryCoverage(name string, apply func(string, *gateRecord, string, *spec) error) func(string, *gateRecord, string, *spec) error {
	return func(raw string, rec *gateRecord, repo string, sp *spec) error {
		var reply struct {
			Gaps *[]string `json:"coverage_gaps"`
		}
		if err := extractJSON(raw, &reply); err != nil || reply.Gaps == nil {
			rec.Notes = append(rec.Notes, name+" coverage: not reported; bounded incoming/outgoing references, registrations/configuration and tests remain unverified")
		} else if len(*reply.Gaps) == 0 {
			rec.Notes = append(rec.Notes, name+" coverage: reviewer reported no further gaps within the exploration bound; this does not establish repository-wide coverage")
		} else {
			for _, gap := range *reply.Gaps {
				rec.Notes = append(rec.Notes, name+" reported coverage gap: "+gap)
			}
		}
		return apply(raw, rec, repo, sp)
	}
}

func gateEvidenceContract(p gatePass) gatePass {
	if p.noTools {
		return p
	}
	old := p.task
	if p.class == "adversary" {
		p.task = strings.ReplaceAll(p.task, ". Open other files only when one of these imports or calls them directly. Stop once you have checked these; an empty findings list is a valid answer.", ".")
		if !strings.Contains(p.task, adversaryEvidenceScope) {
			p.task = strings.ReplaceAll(p.task, `{"findings":`, `{"coverage_gaps":[],"findings":`)
			p.task += adversaryEvidenceScope
			if p.apply != nil {
				p.apply = recordAdversaryCoverage(p.name, p.apply)
			}
		}
	} else if p.apply != nil && strings.Contains(p.task, `"questions"`) && !strings.Contains(p.task, gatePacketContract) {
		p.task = strings.ReplaceAll(p.task, `"changes_result":false}`, `"changes_result":false,"packet":{"goal":"","known_facts":[],"evidence_ids":[],"unknown":"","options":[{"id":"a","text":"","gain":"","cost":""}],"recommendation":"","rationale":"","authority":"","next_action":""}}`)
		p.task += gatePacketContract
	}
	if old != p.task {
		p.prompt = p.shared + p.task
	}
	return p
}

func gateAgentPass(repo, harness string, p gatePass) (gatePass, error) {
	p = gateEvidenceContract(p)
	if !p.lookupTool {
		return p, nil
	}
	cli, err := LookupCLIFor(harness, repo)
	if err != nil {
		if p.restrictedShell && harness == "pi" {
			return p, fmt.Errorf("restricted-shell extension setup failed; refusing an open shell: %w", err)
		}
		return p, err
	}
	if p.restrictedShell && harness == "pi" && !p.noTools {
		if cli.Extension == "" {
			return p, fmt.Errorf("restricted-shell extension unavailable; refusing an open shell")
		}
		ready, err := os.CreateTemp(filepath.Dir(cli.Extension), "ready-*")
		if err != nil {
			return p, fmt.Errorf("restricted-shell readiness: %w", err)
		}
		cli.Ready = ready.Name()
		_ = ready.Close()
		_ = os.Remove(cli.Ready)
		cli.Env = append(cli.Env, "ORCH_LOOKUP_READY="+cli.Ready)
		cli.Notice += "\n" + restrictedShellNotice
	}
	if v, ok := os.LookupEnv("ORCH_TSLS"); ok {
		// The pane shell does not inherit orch's environment, so name the user's
		// TypeScript server override for paneAgentEnv. The value is never logged.
		cli.Env = append(cli.Env, "ORCH_TSLS="+v)
	}
	if piGuardLoaded(harness, p) {
		// A pane's shell, not orch, hands the environment to pi: name the guard
		// knobs so paneAgentEnv exports them there too.
		cli.Env = append(cli.Env, guardEnv(os.Environ())...)
	}
	p.cli = cli
	if !strings.HasPrefix(p.shared, cli.Notice+"\n\n") {
		p.shared = cli.Notice + "\n\n" + p.shared
		p.prompt = cli.Notice + "\n\n" + p.prompt
	}
	if !p.noTools && !strings.Contains(p.task, lookupBudgetHint) {
		p.task += "\n" + lookupBudgetHint
		p.prompt += "\n" + lookupBudgetHint
	}
	return p, nil
}

func passContext(p gatePass) context.Context {
	if p.ctx != nil {
		return p.ctx
	}
	return context.Background()
}

func agentEnv(base, extra []string) []string {
	if base == nil {
		base = os.Environ()
	}
	env := append([]string{}, base...)
	for _, value := range extra {
		key, _, ok := strings.Cut(value, "=")
		if !ok {
			continue
		}
		kept := env[:0]
		for _, entry := range env {
			if !strings.HasPrefix(entry, key+"=") {
				kept = append(kept, entry)
			}
		}
		env = append(kept, value)
	}
	return env
}

// piGuardLoaded reports whether a pass starts pi with the tool guard: pi with the
// ctx tool in open shell mode and a shell. Ctx-only mode restricts the shell
// itself and the guard is silent there; lookup off has no orch lsp to point to.
func piGuardLoaded(harness string, p gatePass) bool {
	return harness == "pi" && p.lookupTool && !p.restrictedShell && !p.noTools
}

// piGuardArgs is the guard's -e argument for a pi pass, or nil.
func piGuardArgs(p gatePass) []string {
	if piGuardLoaded("pi", p) && p.cli.Guard != "" {
		return []string{"-e", p.cli.Guard}
	}
	return nil
}

// guardEnv picks the guard's settings out of env: the switch, the skip list and
// the state directory.
func guardEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "ORCH_TOOL_GUARD", "ORCH_TOOL_GUARD_SKIP", "ORCH_TOOL_GUARD_STATE_DIR":
			out = append(out, kv)
		}
	}
	return out
}

func piPassTools(p gatePass) []string {
	if p.noTools {
		return []string{"--no-tools"}
	}
	if p.restrictedShell && p.lookupTool {
		// No shell until the loaded extension installs its guard and enables bash.
		// If loading fails, pi can continue, but never with an open shell.
		// A modifier selects the initial loadout without excluding bash from
		// pi's registry. A hard --tools read allowlist cannot later enable it.
		args := []string{"--no-builtin-tools", "--tools", "+read"}
		if p.cli.Extension != "" {
			args = append(args, "-e", p.cli.Extension)
		}
		return args
	}
	tools := append([]string{"read", "grep", "find", "ls"}, p.cli.Tools...)
	return []string{"--tools", strings.Join(tools, ",")}
}

func restrictedShellReady(p gatePass) error {
	if !p.restrictedShell || !p.lookupTool || p.noTools {
		return nil
	}
	if b, err := os.ReadFile(p.cli.Ready); err != nil || string(b) != "lookup-only\n" {
		return fmt.Errorf("restricted-shell extension did not load or activate; refusing pass results (shell was disabled at startup)")
	}
	return nil
}

func claudePassArgs(choice profile, p gatePass) []string {
	args := claudeArgs(choice, p.noTools)
	if !p.noTools && len(p.cli.Tools) > 0 {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--tools" {
				args[i+1] += ",Bash"
			}
			if args[i] == "--allowedTools" {
				args[i+1] += "," + strings.Join(p.cli.Tools, ",")
			}
		}
	}
	// Exclude repository memories without --bare, which disables subscription auth.
	settings := map[string]any{"claudeMdExcludes": []string{"**"}}
	if !p.noTools && p.cli.Hook != "" {
		// Prefix allow rules cannot refuse rg --pre or fd -x, so every Bash call goes
		// through the same check pi's shell uses. It adds to the user's own hooks.
		settings["hooks"] = map[string]any{"PreToolUse": []any{map[string]any{
			"matcher": "Bash",
			"hooks":   []any{map[string]any{"type": "command", "command": p.cli.Hook}},
		}}}
	}
	b, _ := json.Marshal(settings)
	return append(args, "--settings", string(b))
}

// paneLaunchScript is the sh script a pane pass runs: it exports env and
// execs argv. The pane's
// shell only types `sh <script>`. Typing the exports and the long agent
// command instead overflowed the terminal's input queue (about 1 KB on macOS)
// while a fresh pane's shell was still starting, which dropped bytes and
// merged lines. The exports live only in the agent's process, never in the
// pane's shell. PATH is not touched: the pane's own PATH must find orch.
func paneLaunchScript(header string, env []string, argv []string) string {
	var b strings.Builder
	b.WriteString("# orch gate pass ")
	b.WriteString(strings.ReplaceAll(header, "\n", " "))
	b.WriteString("\n")
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if key == "PATH" || key == "" {
			continue
		}
		// Env names come from LookupCLIFor; values remain literal shell strings.
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellQuote(value))
		b.WriteString("\n")
	}
	b.WriteString("exec")
	for _, a := range argv {
		b.WriteString(" ")
		b.WriteString(shellQuote(a))
	}
	b.WriteString("\n")
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func treeWarning(rec *gateRecord) string {
	if len(rec.TreeChanged) > 0 {
		return "UNVERIFIED: checkout changed: " + strings.Join(rec.TreeChanged, ", ") + "; in flight: " + strings.Join(rec.TreeChangedPasses, ", ") + ". Findings retained for inspection; approval withheld. Files were NOT reverted."
	}
	if rec.TreeCheckError != "" {
		return "UNVERIFIED: checkout could not be checked: " + rec.TreeCheckError + ". Findings retained for inspection; approval withheld."
	}
	return ""
}

// A coverage repair may exhaust the run after scope/questions or some items
// already finished. Ask the existing reducer which checks remain open; never
// duplicate its support rules or turn completed checks into execution failures.
// The diagnostic key is not a required actor, so the legacy execution adapter
// does not manufacture a blanket failure for this partially completed pass.
func recordPartialBudgetFailure(rec *gateRecord, actor string) string {
	if rec.Review == nil {
		return actor
	}
	g := rec.Review
	result := ReduceReview(rec.Subject, g.Checks, g.Answers, g.Findings, g.Policy)
	pending := map[string]bool{}
	for _, reason := range result.Reasons {
		if reason.Actor == actor {
			pending[reason.Check] = true
		}
	}
	completed := false
	for _, c := range g.Checks {
		if c.Actor == actor && !pending[c.key()] {
			completed = true
		}
	}
	if !completed {
		return actor
	}
	for _, c := range g.Checks {
		if c.Actor != actor || !pending[c.key()] {
			continue
		}
		g.Answers = append(g.Answers, reviewAnswer{SchemaVersion: evidenceSchemaVersion,
			ID: "budget-" + subjectJSONDigest([]string{actor, c.key()}), ItemID: c.Item.ID, ItemRevision: c.Item.Revision,
			CheckID: c.ID, AnswerState: reviewAnswerBudget, Rationale: "budget_exhausted",
			NextAction: "Resume " + actor + " for " + c.key() + "; raise the user-level gate budget or ask the recorded owner"})
	}
	return actor + "#budget"
}

// User-only, optional overrides. Pointers distinguish an omitted limit from
// an explicit zero repairs/fallbacks. A repository cannot decode this table.
type gateBudgetConfig struct {
	Requests            *int     `toml:"requests"`
	InputTokens         *int64   `toml:"input_tokens"`
	OutputTokens        *int64   `toml:"output_tokens"`
	Seconds             *int     `toml:"seconds"`
	Repairs             *int     `toml:"repairs"`
	Fallbacks           *int     `toml:"fallbacks"`
	OutputReserve       *int64   `toml:"output_reserve"`
	MaxUSD              *float64 `toml:"max_usd"`
	EstimatedRequestUSD *float64 `toml:"estimated_request_usd"`
	ReliableEstimate    *bool    `toml:"reliable_estimate"`
}

func budgetValue[T any](value *T, fallback T) T {
	if value != nil {
		return *value
	}
	return fallback
}

// Four route invocations per pass cover the existing first/repair-or-stall
// attempt and one provider fallback on each. Each gets the existing hard turn
// allowance plus a final answer, 150k input tokens and 32k output per turn.
// Serial 45-minute caps are deliberately more generous than the worker pool.
func gateRunLimits(passes []gatePass, cfg *gateBudgetConfig) adjudicationLimits {
	requests := 2 // leave room for the optional adjudicator's default two calls
	for _, p := range passes {
		requests += 4 * (int(math.Ceil(float64(max(1, p.budget))*gateBudgetHard)) + 2)
	}
	l := adjudicationLimits{Requests: requests, InputTokens: int64(requests) * int64(max(gateContextSoft, 150000)) * 4,
		OutputTokens: int64(requests) * 32768, Seconds: max(1, int((time.Duration(len(passes))*gateTimeout + 180*time.Second).Seconds())),
		Repairs: max(1, 2*len(passes)+1), Fallbacks: max(1, 3*len(passes)+1), OutputReserve: 4096}
	if cfg != nil {
		l.Requests = budgetValue(cfg.Requests, l.Requests)
		l.InputTokens = budgetValue(cfg.InputTokens, l.InputTokens)
		l.OutputTokens = budgetValue(cfg.OutputTokens, l.OutputTokens)
		l.Seconds = budgetValue(cfg.Seconds, l.Seconds)
		l.Repairs = budgetValue(cfg.Repairs, l.Repairs)
		l.Fallbacks = budgetValue(cfg.Fallbacks, l.Fallbacks)
		l.OutputReserve = budgetValue(cfg.OutputReserve, l.OutputReserve)
		l.MaxUSD = budgetValue(cfg.MaxUSD, l.MaxUSD)
		l.EstimatedRequestUSD = budgetValue(cfg.EstimatedRequestUSD, l.EstimatedRequestUSD)
		l.ReliableEstimate = budgetValue(cfg.ReliableEstimate, l.ReliableEstimate)
	}
	return l
}

// Admission belongs at the execution boundary inside roleRun.run's callback:
// every provider invocation, including a refused fallback, sees the
// same run total. Prepared prompt bytes include both shared prefix classes.
func runBudgetedGatePass(b *RunBudget, p gatePass, repair, fallback bool, run func(gatePass) (string, string, error)) (string, string, error) {
	ticket, err := b.begin(int64(len(p.prompt)), repair, fallback)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(passContext(p), b.remaining())
	defer cancel()
	p.ctx = ctx
	p.admission = ticket
	out, hit, err := run(p)
	report := gateInvocationReport(p.sessionDir)
	requests := report.requests
	if requests > 0 && requests < ticket.continuations+1 {
		report.unknown.Requests = 1
	}
	// Additional pane prompts were already admitted separately. Reported
	// sessions can include still more tool-loop requests; charge those once.
	if requests > 0 {
		requests = max(1, requests-ticket.continuations)
	}
	// Finalize tokens and additional requests atomically before releasing
	// this invocation's reservations to another worker.
	usageErr := ticket.finishUsage(report.usage, report.unknown, requests)
	if report.overflow {
		usageErr = errAdjudicationBudget
	}
	if b.remaining() <= 0 || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		usageErr = errAdjudicationBudget
	}
	if usageErr != nil {
		return out, hit, usageErr
	}
	return out, hit, err
}

// Read only this invocation's files, not earlier attempts in subdirectories.
// Pi reports message usage in JSONL; Claude's stream-json result supplies its
// aggregate split. Missing fields stay nil, even when their sibling is known.
type gateUsageReport struct {
	usage    executionUsage
	requests int
	unknown  budgetUnknown
	overflow bool
}

func gateInvocationUsage(dir string) (executionUsage, int) {
	r := gateInvocationReport(dir)
	return r.usage, r.requests
}

func gateInvocationReport(dir string) gateUsageReport {
	u := executionUsage{Availability: "unknown"}
	requests := 0
	var input, output int64
	var cost float64
	inputKnown, outputKnown, costKnown := false, false, false
	unknown := budgetUnknown{}
	// Claude can persist several content blocks with the same native message
	// id. Count one request and the largest cumulative usage, not each block.
	type sample struct {
		input, output int64
		cost          float64
	}
	messages := map[string]sample{}
	overflow := false
	add := func(total *int64, values ...int64) {
		for _, n := range values {
			if n > math.MaxInt64-*total {
				*total = math.MaxInt64
				overflow = true
			} else {
				*total += n
			}
		}
	}
	files, _ := os.ReadDir(dir)
	for _, file := range files {
		if file.IsDir() || (filepath.Ext(file.Name()) != ".jsonl" && filepath.Ext(file.Name()) != ".json") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, file.Name()))
		if err != nil {
			continue
		}
		r := bufio.NewReader(f)
		for {
			line, readErr := r.ReadBytes('\n')
			var event struct {
				Type     string                     `json:"type"`
				NumTurns int                        `json:"num_turns"`
				Cost     *float64                   `json:"total_cost_usd"`
				Usage    map[string]json.RawMessage `json:"usage"`
				Message  struct {
					ID    string                     `json:"id"`
					Role  string                     `json:"role"`
					Usage map[string]json.RawMessage `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &event) == nil {
				usage := event.Message.Usage
				isResult := event.Type == "result" && event.Usage != nil
				if isResult {
					usage = event.Usage
				}
				if event.Message.Role == "assistant" && usage == nil {
					// A text-only record can be the final rendering of an already
					// counted request. Do not invent another measured request.
					unknown = budgetUnknown{1, 1, 1, 1}
				}
				if isResult || event.Message.Role == "assistant" && usage != nil {
					// Claude result usage supersedes its streamed assistant totals.
					if isResult {
						input, output, cost, requests = 0, 0, 0, 0
						inputKnown, outputKnown, costKnown = false, false, false
						unknown = budgetUnknown{}
						messages = map[string]sample{}
					}
					previous, duplicate := messages[event.Message.ID]
					if event.Message.ID == "" {
						duplicate = false
						previous = sample{}
					}
					if duplicate {
						requests--
					}
					requests++
					if isResult {
						requests = event.NumTurns
						// Harness turns are a lower bound, not API telemetry.
						unknown.Requests = 1
					}
					read := func(keys ...string) (int64, bool) {
						for _, key := range keys {
							if raw, ok := usage[key]; ok {
								var n int64
								if json.Unmarshal(raw, &n) == nil && n >= 0 {
									return n, true
								}
							}
						}
						return 0, false
					}
					in, haveIn := read("input", "input_tokens")
					out, haveOut := read("output", "output_tokens")
					cached, haveCache := read("cacheRead", "cache_read_input_tokens")
					written, haveWrite := read("cacheWrite", "cache_creation_input_tokens")
					var requestInput int64
					add(&requestInput, in, cached, written)
					current := sample{input: max(previous.input, requestInput), output: max(previous.output, out), cost: previous.cost}
					add(&input, current.input-previous.input)
					add(&output, current.output-previous.output)
					inputKnown = inputKnown || haveIn || haveCache || haveWrite
					outputKnown = outputKnown || haveOut
					if !haveIn {
						unknown.Input = 1
					}
					if !haveOut {
						unknown.Output = 1
					}
					var c struct {
						Total *float64 `json:"total"`
					}
					_ = json.Unmarshal(usage["cost"], &c)
					if isResult {
						c.Total = event.Cost
					}
					if c.Total == nil {
						unknown.Cost = 1
					} else {
						costKnown = true
						if *c.Total < 0 {
							overflow = true
						}
						current.cost = max(previous.cost, *c.Total)
						cost += current.cost - previous.cost
					}
					if event.Message.ID != "" {
						messages[event.Message.ID] = current
					}
				}
			}
			if readErr != nil {
				if readErr != io.EOF {
					unknown = budgetUnknown{1, 1, 1, 1}
				}
				break
			}
		}
		f.Close()
	}
	if inputKnown || outputKnown || costKnown {
		u.Availability = "partial"
		if inputKnown {
			u.InputTokens = &input
		}
		if outputKnown {
			u.OutputTokens = &output
		}
		if costKnown {
			u.CostUSD = &cost
		}
		if inputKnown && outputKnown && costKnown && unknown == (budgetUnknown{}) {
			u.Availability = "available"
		}
	}
	if requests == 0 {
		unknown.Requests = 0
	}
	return gateUsageReport{usage: u, requests: requests, unknown: unknown, overflow: overflow}
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
