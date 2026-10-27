// Command orch-guard is the tool guard of both harnesses.
//
// For Claude Code, `pre` handles a PreToolUse event (the deny rules and
// deny-big-read) and `coach` handles PostToolUse, PostToolUseFailure and
// UserPromptSubmit (the lookup log and reminders). A block is exit 2 with the
// reason on stderr; anything else exits 0.
//
// With --harness pi the same two modes take the JSON request of the pi
// extension (tool/pi_ctx_reminder.ts) and answer with one JSON object on
// stdout, always with exit 0; the protocol is described in tool/guard/pi.go.
package main

import (
	"encoding/json"
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
	pi := len(args) == 3 && args[1] == "--harness" && args[2] == "pi"
	if (len(args) != 1 && !pi) || (args[0] != "pre" && args[0] != "coach") {
		fmt.Fprintln(os.Stderr, "usage: orch-guard pre|coach [--harness pi] < event.json, or orch-guard --version")
		os.Exit(1)
	}
	code := 0
	if data, err := io.ReadAll(os.Stdin); err == nil {
		if ev, ok := guard.ParseEvent(data); ok {
			switch {
			case pi:
				var res guard.PiResponse
				if args[0] == "pre" {
					res = guard.PiPre(ev)
				} else {
					res = guard.PiCoach(ev)
				}
				json.NewEncoder(os.Stdout).Encode(res)
			case args[0] == "pre":
				code = guard.Pre(ev, os.Stderr)
			default:
				guard.Coach(ev, os.Stdout) // fail open: a coach error never disturbs a session
			}
		} else if pi {
			json.NewEncoder(os.Stdout).Encode(guard.PiResponse{}) // unreadable event: allow
		}
	} else if pi {
		json.NewEncoder(os.Stdout).Encode(guard.PiResponse{})
	}
	guard.CheckStale(fingerprint, sourceDir)
	os.Exit(code)
}
