// Read-only gate passes: the shell runs `orch ctx <find|sym|text|batch|refs> ...` and
// nothing else. No pipes, redirects, command substitution, globs or other commands.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { writeFileSync } from "node:fs";

const allowed = /^orch ctx (find|sym|text|batch|refs)(\s|$)/;

// Why a command line is not one plain orch ctx call, or "" when it is.
function refusal(command: string): string {
	const cmd = command.trim();
	if (!allowed.test(cmd)) {
		return "only orch ctx find, sym, text, batch and refs run in this shell";
	}
	let quote = "";
	for (const ch of cmd) {
		if (quote === "'") {
			if (ch === "'") quote = "";
			continue;
		}
		if (quote === '"') {
			if (ch === '"') quote = "";
			else if (ch === "$" || ch === "`" || ch === "\\") return "no $, backtick or backslash inside double quotes (use single quotes)";
			continue;
		}
		if (ch === "'" || ch === '"') quote = ch;
		else if (";&|<>(){}`$\\\n\r*?[]~!#".includes(ch)) return `no unquoted ${JSON.stringify(ch)}: no pipes, redirects, globs or substitutions`;
	}
	return quote ? "unterminated quote" : "";
}

export default function (pi: ExtensionAPI) {
	pi.on("tool_call", async (event) => {
		if (event.toolName !== "bash") return undefined;
		const why = refusal(String(event.input.command ?? ""));
		if (!why) return undefined;
		return {
			block: true,
			reason:
				`Blocked: ${why}. This shell only runs one orch ctx call per command, with no pipes. ` +
				`Put several lookups in ONE call: orch ctx batch -q "find:Code that ..." -q "symbol:Name refs=3" -q "text:pattern glob=dir/**". ` +
				`Shorten output with --compact or --limit N. Read files with the read tool (offset and limit); there is no cd, ls, grep, head, tail, sed or cat.`,
		};
	});
	pi.on("session_start", () => {
		const ready = process.env.ORCH_CTX_ONLY_READY;
		if (!ready) return;
		// The guard above is installed before bash becomes available. A failed
		// import leaves only read active; orch rejects an absent acknowledgement.
		pi.setActiveTools(["read", "bash"]);
		if (pi.getActiveTools().sort().join(",") !== "bash,read") {
			throw new Error("ctx-only tools could not be activated");
		}
		writeFileSync(ready, "ctx-only\n", { mode: 0o600 });
	});
}
