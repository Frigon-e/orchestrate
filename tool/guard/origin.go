package guard

import "strings"

// Call origin: a call a Code Mode script makes (Claude Code's REPL tool, pi's
// codemode tool) is "script"; a call the model makes itself is "direct". A
// script filters what it reads, so its calls never reach the model's context
// as such and the context-saving rules and reminders do not apply to them.
const (
	originDirect = "direct"
	originScript = "script"
)

// callOrigin is the origin of one hook event. Claude Code gives a script's calls
// a tool_use_id that starts with repl_ (a direct call's starts with toolu_); the
// pi extension sends the parent call's id as parent for a nested call.
func callOrigin(ev *Event) string {
	if strings.HasPrefix(ev.Str("tool_use_id"), "repl_") || ev.Str("parent") != "" {
		return originScript
	}
	return originDirect
}
