// Command orch runs orchestrators with one Herdr pane per lead. Several
// orchestrators can share a repo: each lead records the orchestrator pane
// that owns it, and each orchestrator runs its own watcher.
//
// State for a repo lives in <herdr worktrees dir>/<repo>/.orchestrate/<task-id>/
// (task.md, lead.json, status.log, inbox/, report.md). ORCH_HOME overrides the
// base directory. Per-repo settings come from <repo>/.agents/orchestrate.toml.
// Run it through the skill's scripts/orch wrapper, which builds this binary and
// sets ORCH_SELF and ORCH_SKILL_DIR.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// The launcher sets this to the source/toolchain fingerprint at build time.
// Direct go builds still have a useful, dependency-free version response.
var orchVersion = "devel"

const usage = `orch - orchestrators, one Herdr pane per lead

Orchestrator:
  orch version | --version | -v | -V   (bare build fingerprint; no repo needed)
  orch new <id> --kind ship|scout --harness claude|pi --model M --effort E [--base B] [--depends a,b]
  orch plan <spec> [--dry-run]
  orch check <id>
  orch verify <id> [--no-merge]
  orch integration remove <slug>
  orch gate <spec>
  orch bench record|import|list|show|compare|key|spec|report|verify|migrate|export|import-archive|readme
  orch spec queries <spec> [--package id | --item id#n] [--json] [--generate [--dry-run] [--all]]
  orch test-changed [--base main] [--print] [--run-extra <regex>]   (build, vet and only the tests the branch changed)
  orch ctx status|refresh|watch|find|sym|text|batch|brief   (code facts for any git repo; see references/context.md)
  orch quick <id> --owns <file>[,<file>] <request...>
  orch models
  orch triage <id>... [--force] | orch triage report
  orch doctor
  orch sweep <topic-dir> [--roots name=dir] [--min-passes 2] [--final settle]
  orch audit [<id>] [--system S] [--since 6h] [--problems]
  orch hold [on <reason> | off]
  orch unfreeze <id>
  orch health [set claude|pi|pi:provider|pi:openrouter:anthropic ok|limited [--for 3h] [--reason R]]
  orch spawn <id> [--auto [--tier settled|standard|planning] [--apply]]
  orch spawn <id> --harness claude|pi --model M [--effort E]
  orch send <id> [--resolve KEY] <text...|->
  orch status [--json] [--all]
  orch ask [--cards DIR] [--out DIR]
  orch dispatch
  orch watch [--interval S] [--timeout S] [--once]
  orch peek <id> [--lines N]
  orch focus <id>
  orch close <id> [--force]
  orch compact <id> [focus...]
  orch respawn <id> [--harness claude|pi [--model M] [--effort E] [--degraded]]
  orch adopt <id>
  orch become [--off]
  orch config-check <file>
  orch lock list | lock release <name> --force
Lead:
  orch report [--dir D] [--key K] <state> <text...>
  orch inbox [--dir D]
  orch lock acquire|release <name> [--dir D] [--wait S]
Hooks:
  orch hook pretool|stop|session-start
`

type app struct {
	overrideTier bool // spawn --override-tier: skip the triage refusal
	skipTriage   bool // an explicit --tier does not call the Decider
	cwd          string
	stdin        io.Reader
	out          io.Writer
	errw         io.Writer
	now          func() time.Time
}

// exitError carries a message for stderr and the process exit code.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func fail(format string, a ...any) error {
	return &exitError{code: 1, msg: fmt.Sprintf(format, a...)}
}

var commands = map[string]func(*app, []string) error{
	"new":          cmdNew,
	"plan":         cmdPlan,
	"spec":         cmdSpec,
	"check":        cmdCheck,
	"verify":       cmdVerify,
	"integration":  cmdIntegration,
	"gate":         cmdGate,
	"bench":        cmdBench,
	"ctx":          cmdCtx,
	"test-changed": cmdTestChanged,
	"models":       cmdModels,
	"triage":       cmdTriage,
	"quick":        cmdQuick,
	"audit":        cmdAudit,
	"sweep":        cmdSweep,
	"health":       cmdHealth,
	"hold":         cmdHold,
	"unfreeze":     cmdUnfreeze,
	"doctor":       cmdDoctor,
	"spawn":        cmdSpawn,
	"send":         cmdSend,
	"status":       cmdStatus,
	"ask":          cmdAsk,
	"dispatch":     cmdDispatch,
	"watch":        cmdWatch,
	"adopt":        cmdAdopt,
	"peek":         cmdPeek,
	"focus":        cmdFocus,
	"close":        cmdClose,
	"compact":      cmdCompact,
	"respawn":      cmdRespawn,
	"become":       cmdBecome,
	"config-check": cmdConfigCheck,
	"lock":         cmdLock,
	"report":       cmdReport,
	"inbox":        cmdInbox,
	"hook":         cmdHook,
}

func main() {
	cwd, _ := os.Getwd()
	a := &app{cwd: cwd, stdin: os.Stdin, out: os.Stdout, errw: os.Stderr, now: time.Now}
	os.Exit(a.run(os.Args[1:]))
}

func (a *app) run(args []string) int {
	// Answer probes before command dispatch, repo/config lookup or backends.
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version" || args[0] == "-v" || args[0] == "-V") {
		if len(args) != 1 {
			fmt.Fprintln(a.errw, "orch: usage: orch version | --version | -v | -V")
			return 2
		}
		fmt.Fprintln(a.out, orchVersion)
		return 0
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(a.out, usage)
		return 0
	}
	fn, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(a.errw, "orch: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	err := fn(a, args[1:])
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintf(a.errw, "orch: %s\n", ee.msg)
		}
		return ee.code
	}
	fmt.Fprintf(a.errw, "orch: %v\n", err)
	return 1
}

func (a *app) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("orch "+name, flag.ContinueOnError)
	fs.SetOutput(a.errw)
	return fs
}

// parseArgs parses flags anywhere among the positional arguments; everything
// after a literal "--" is positional.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, s := range args {
		if s == "--" {
			tail, args = args[i+1:], args[:i]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &exitError{code: 2}
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	return append(pos, tail...), nil
}

func exactArgs(pos []string, n int, form string) error {
	if len(pos) != n {
		return &exitError{code: 2, msg: "usage: orch " + form}
	}
	return nil
}
