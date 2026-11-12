// Read-only gate passes: the shell runs `jbcontext search`, `orch lsp`, `rg`,
// `fd`, `ls` or `wc` commands, one per call or joined with ; or &&, and nothing else. No pipes, redirects,
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

// What a character that is refused outside quotes does.
function charWhy(ch: string): string {
	switch (ch) {
		case "|": return "a pipe sends output to another command";
		case "<": case ">": return "a redirect reads or writes a file";
		case "*": case "?": case "[": case "]": return "an unquoted glob expands to file names; quote the pattern";
		case "$": case "`": return "a substitution runs another command or expands a variable; quote it with single quotes";
		case "(": case ")": case "{": case "}": return "a subshell or group";
		case "~": return "home-directory expansion";
		case "!": return "history expansion";
		case "#": return "a comment";
		case "\\": return "an escape; quote instead";
		case "\r": return "a carriage return";
	}
	return "";
}

// Cut a command line at the unquoted ;, && and newline that join lookups, and at
// the unquoted || and lone & that do not. bad lists the refused connectors found.
// A lone | stays in its part, where the character check refuses it.
function split(cmd: string): { parts: string[]; bad: string[] } {
	const r = Array.from(cmd);
	const parts: string[] = [];
	const bad: string[] = [];
	let cur = "";
	let quote = "";
	for (let i = 0; i < r.length; i++) {
		const ch = r[i];
		if (quote) {
			if (ch === quote) quote = "";
			cur += ch;
			continue;
		}
		const next = r[i + 1] ?? "";
		if (ch === "'" || ch === '"') {
			quote = ch;
			cur += ch;
			continue;
		}
		if (ch === ";" || ch === "\n") {
			// a separator
		} else if (ch === "&" && next === "&") {
			i++;
		} else if (ch === "&") {
			bad.push("&");
		} else if (ch === "|" && next === "|") {
			i++;
			bad.push("||");
		} else {
			cur += ch;
			continue;
		}
		parts.push(cur.trim());
		cur = "";
	}
	parts.push(cur.trim());
	return { parts, bad };
}

// Why one part of a command line is not a plain lookup call, or "" when it is.
function partRefusal(cmd: string): string {
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
		else if (charWhy(ch)) return `unquoted ${JSON.stringify(ch)}: ${charWhy(ch)}`;
	}
	if (quote) return "unterminated quote";
	return execFlag(cmd);
}

// Why a command line is not plain lookup calls joined by ; or &&, or "" when it is.
function refusal(command: string): string {
	const { parts, bad } = split(command.trim());
	for (let i = 0; i < parts.length; i++) {
		const p = parts[i];
		if (p === "" && parts.length > 1) return "empty command between separators";
		const why = partRefusal(p);
		if (!why) continue;
		if (parts.length > 1) return `part ${i + 1} of ${parts.length} (${Array.from(p).slice(0, 60).join("")}): ${why}`;
		return why;
	}
	if (bad.length > 0) {
		const why = bad[0] === "&" ? `unquoted "&": & runs a command in the background` : `unquoted "||": || chains commands; only ; and && can join lookups`;
		return `${why}\nSend these as separate calls in one message:\n- ${parts.join("\n- ")}`;
	}
	return "";
}

const denyTail =
	"This shell runs one lookup per call, or several joined with ; or &&: jbcontext search, orch lsp, rg, fd, ls or wc, with no pipes, redirects or unquoted globs. " +
	"Independent lookups are better as separate calls in one message: they run in parallel and their output stays separate. " +
	"Read files with the read tool (offset and limit); there is no cd, grep, head, tail, sed, cat or find.";

export default function (pi: ExtensionAPI) {
	pi.on("tool_call", async (event) => {
		if (event.toolName !== "bash") return undefined;
		const why = refusal(String(event.input.command ?? ""));
		if (!why) return undefined;
		return {
			block: true,
			reason: `Blocked: ${why}\n${denyTail}`,
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
