// Read-only gate passes: the shell runs one `jbcontext search`, `orch lsp`, `rg`,
// `fd`, `ls` or `wc` command per call and nothing else. No pipes, redirects,
// command substitution, unquoted globs or other commands; rg and fd flags that
// run another program are refused.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { writeFileSync } from "node:fs";

const allowed = /^(jbcontext search|orch lsp|rg|fd|ls|wc)(\s|$)/;

// The words of a command line that passed the quote checks, quotes removed.
function words(cmd: string): string[] {
	const out: string[] = [];
	let cur = "";
	let has = false;
	let quote = "";
	for (const ch of cmd) {
		if (quote) {
			if (ch === quote) quote = "";
			else cur += ch;
		} else if (ch === "'" || ch === '"') {
			quote = ch;
			has = true;
		} else if (/\s/.test(ch)) {
			if (has) out.push(cur);
			cur = "";
			has = false;
		} else {
			cur += ch;
			has = true;
		}
	}
	if (has) out.push(cur);
	return out;
}

// Why the flags of an rg or fd call are refused: they run another program.
// Every word is scanned, even after "--": a flag that takes a value (rg -g --)
// can swallow the "--", so it does not reliably end the flags. A pattern that
// looks like an exec flag is refused too.
function execFlag(cmd: string): string {
	const w = words(cmd);
	const flags = w.slice(1);
	if (w[0] === "rg") {
		for (const f of flags) {
			if (/^--(pre|pre-glob|hostname-bin)(=|$)/.test(f)) return `rg ${f.split("=")[0]} runs another program`;
		}
	}
	if (w[0] === "fd") {
		for (const f of flags) {
			if (/^--(exec|exec-batch)(=|$)/.test(f) || /^-[A-Za-z]*[xX]/.test(f)) return `fd ${f.split("=")[0]} runs another program`;
		}
	}
	return "";
}

// Why a command line is not one plain lookup call, or "" when it is.
function refusal(command: string): string {
	const cmd = command.trim();
	if (!allowed.test(cmd)) {
		return "only jbcontext search, orch lsp, rg, fd, ls and wc run in this shell";
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
	if (quote) return "unterminated quote";
	return execFlag(cmd);
}

export default function (pi: ExtensionAPI) {
	pi.on("tool_call", async (event) => {
		if (event.toolName !== "bash") return undefined;
		const why = refusal(String(event.input.command ?? ""));
		if (!why) return undefined;
		return {
			block: true,
			reason:
				`Blocked: ${why}. This shell runs one command per call: jbcontext search, orch lsp, rg, fd, ls or wc, with no pipes, redirects or unquoted globs. ` +
				`Put independent lookups in separate calls in one message. ` +
				`Read files with the read tool (offset and limit); there is no cd, grep, head, tail, sed or cat.`,
		};
	});
	pi.on("session_start", () => {
		const ready = process.env.ORCH_LOOKUP_READY;
		if (!ready) return;
		// The guard above is installed before bash becomes available. A failed
		// import leaves only read active; orch rejects an absent acknowledgement.
		pi.setActiveTools(["read", "bash"]);
		if (pi.getActiveTools().sort().join(",") !== "bash,read") {
			throw new Error("restricted-shell tools could not be activated");
		}
		writeFileSync(ready, "lookup-only\n", { mode: 0o600 });
	});
}
