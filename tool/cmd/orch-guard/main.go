// Command orch-guard is the Claude Code tool guard: `pre` handles a PreToolUse
// event (the deny rules and deny-big-read) and `coach` handles PostToolUse,
// PostToolUseFailure and UserPromptSubmit (the lookup log and reminders).
// A block is exit 2 with the reason on stderr; anything else exits 0.
package main

import (
	"fmt"
	"io"
	"os"

	"orchestrate/orch/guard"
)

// Set at build time by scripts/build-guard: the source fingerprint, and the
// repository the binary was built from, which the staleness check re-hashes.
var (
	fingerprint = "dev"
	sourceDir   = ""
)

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println(fingerprint)
		return
	}
	if len(args) != 1 || (args[0] != "pre" && args[0] != "coach") {
		fmt.Fprintln(os.Stderr, "usage: orch-guard pre|coach < hook-event.json, or orch-guard --version")
		os.Exit(1)
	}
	code := 0
	if data, err := io.ReadAll(os.Stdin); err == nil {
		if ev, ok := guard.ParseEvent(data); ok {
			if args[0] == "pre" {
				code = guard.Pre(ev, os.Stderr)
			} else {
				guard.Coach(ev, os.Stdout) // fail open: a coach error never disturbs a session
			}
		}
	}
	guard.CheckStale(fingerprint, sourceDir)
	os.Exit(code)
}
