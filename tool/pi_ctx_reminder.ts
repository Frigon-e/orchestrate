// Guards the model against five kinds of raw shell work and points it at the
// right tool instead. Each rule is a table entry with a name, a matcher and a
// reason: deny-find-chains (find -exec / find -delete / a search tool feeding a
// grep), deny-find (a find over a tree), deny-sed-inplace (sed -i, perl -pi),
// deny-cat-head-tail (a bare cat, head, tail or sed -n on one file) and
// deny-grep-rg (a plain grep, egrep, fgrep, ag or git grep over files or a
// directory). A grep is blocked only while rg is on PATH and rg itself is never
// blocked; a find rule names fd, or the built-in tool when fd is missing. Every
// search block ends with the lookup ladder (jbcontext search, orch lsp, rg, fd).
// pi's own grep and find reach the tree without a shell, so two more rules
// refuse them while rg (for grep) or fd (for find) is on PATH and point at the
// shell tool; ls only lists a directory and stays. A built-in search that names
// a path outside the session's git repository is allowed. A grep filtering
// another command's output and plain fd are not blocked; the ladder is appended
// to their result instead, and to a built-in search that does run, while `orch`
// is on PATH.
// deny-big-read refuses pi's read of a file over 30 KB when it has no offset and
// no limit, whatever is installed; it names a ranged read and how to get the lines.
import type { ExtensionAPI, ToolCallEvent, ToolResultEvent } from "@earendil-works/pi-coding-agent";
import { accessSync, constants, realpathSync, statSync } from "node:fs";
import { delimiter, dirname, extname, isAbsolute, join, relative, resolve, sep } from "node:path";

const marker = "Lookup order:";
const guardMarker = "orch tool guard";

// The line every search block ends with, and the note appended to a search
// result: where to look, in order.
const ladder =
	`${marker} jbcontext search "<one behavior>" if the location is unknown, ` +
	`orch lsp def|refs|callers <Name> for a known symbol, rg for exact text, ` +
	`fd for file names; send independent lookups in one message.`;
const reminder = ladder;

const shellSearches = new Set(["rg", "grep", "egrep", "fgrep", "ag", "fd", "find"]);
const builtinSearches = new Set(["grep", "find", "ls"]);

type word = { text: string; quoted: boolean };
type segment = {
	words: word[];
	// stdin comes from an earlier command's pipe, so this only formats it.
	stdinPipe: boolean;
	// stdout feeds a later command, so this is one step of a pipeline.
	stdoutPipe: boolean;
	// A redirect writes a file, so the command is not a plain read.
	redirect: boolean;
};

// The tools a block may name: orch for the result note, fd and rg as the
// replacements for a file search and a content search.
type tools = { orch: boolean; fd: boolean; rg: boolean };

type rule = {
	// The full name, and the short name ORCH_TOOL_GUARD_SKIP takes.
	name: string;
	id: string;
	match: (line: string, tools: tools) => boolean;
	reason: (tools: tools) => string;
};

// A built-in tool call: pi's own tools reach the tree without a shell, so a
// rule for one matches the tool name and the arguments it carries.
type builtinCall = { name: string; input: Record<string, unknown> };

type builtinRule = {
	// The full name the reason shows, and the short name ORCH_TOOL_GUARD_SKIP takes.
	name: string;
	id: string;
	// The one ORCH_TOOL_GUARD_SKIP name that disables every built-in rule.
	group: string;
	match: (call: builtinCall, tools: tools) => boolean;
	// The reason is fixed: the rule only matches while its replacement is on PATH.
	reason: string;
};

// True when an executable of this name is on PATH, so the guard is harmless in
// repos and sessions that never installed the skill or the search tools.
function onPath(name: string): boolean {
	for (const dir of (process.env.PATH ?? "").split(delimiter)) {
		if (!dir) continue;
		const path = join(dir, name);
		try {
			if (!statSync(path).isFile()) continue;
			accessSync(path, constants.X_OK);
			return true;
		} catch {
			// Not this directory; keep looking.
		}
	}
	return false;
}

// Computed once: PATH does not change under a running session.
function installed(): tools {
	return { orch: onPath("orch"), fd: onPath("fd"), rg: onPath("rg") };
}

// Split a command line on unquoted separators, dropping quoted text, so an
// argument that merely contains the word grep is not a search. Each word also
// records whether it came from a quote, so a quoted word is never a command.
function segments(command: string): segment[] {
	const out: segment[] = [];
	let words: word[] = [];
	let text = "";
	let started = false;
	let quoted = false;
	let quote = "";
	let escaped = false;
	let stdinPipe = false;
	let redirect = false;

	const endWord = () => {
		if (!started) return;
		words.push({ text, quoted });
		text = "";
		started = false;
		quoted = false;
	};
	const endSegment = (pipeOut: boolean) => {
		endWord();
		if (words.length) out.push({ words, stdinPipe, stdoutPipe: pipeOut, redirect });
		words = [];
		stdinPipe = pipeOut;
		redirect = false;
	};

	for (const ch of command) {
		if (quote) {
			started = true;
			quoted = true;
			if (escaped) {
				escaped = false;
				text += ch;
			} else if (quote === '"' && ch === "\\") escaped = true;
			else if (ch === quote) quote = "";
			else text += ch;
			continue;
		}
		if (ch === "'" || ch === '"') {
			quote = ch;
			started = true;
			quoted = true;
			continue;
		}
		if (ch === ">" || ch === "<") {
			endWord();
			redirect = true;
			continue;
		}
		if (ch === "|") {
			endSegment(true);
			continue;
		}
		if (";&\n\r()".includes(ch)) {
			endSegment(false);
			continue;
		}
		if (ch === " " || ch === "\t") {
			endWord();
			continue;
		}
		text += ch;
		started = true;
	}
	endSegment(false);
	return out;
}

function base(word: string): string {
	return word.replaceAll("\\", "").split("/").pop() ?? "";
}

// sudo flags that take a separate value, so `sudo -u root cat f` still reads.
const sudoValue = /^-[ugpCDhTr]$/;

// The command a segment runs, with its arguments. Leading VAR=value prefixes,
// `sudo` and a path to the tool do not hide the command from a rule.
function command(segment: segment): { name: string; args: string[] } {
	const words = segment.words;
	let at = 0;
	while (at < words.length && !words[at].quoted && /^[A-Za-z_][A-Za-z0-9_]*=/.test(words[at].text)) at += 1;
	while (at < words.length && !words[at].quoted && base(words[at].text) === "sudo") {
		at += 1;
		while (at < words.length && !words[at].quoted && /^-[A-Za-z]/.test(words[at].text)) {
			const flag = words[at].text;
			at += 1;
			if (sudoValue.test(flag)) at += 1;
		}
		while (at < words.length && !words[at].quoted && /^[A-Za-z_][A-Za-z0-9_]*=/.test(words[at].text)) at += 1;
	}
	const first = words[at];
	if (!first || first.quoted) return { name: "", args: [] };
	return { name: base(first.text), args: words.slice(at + 1).map((w) => w.text) };
}

function segmentSearch(segment: segment): boolean {
	const { name, args } = command(segment);
	if (!name) return false;
	if (name === "git") return args[0] === "grep";
	if (shellSearches.has(name)) return true;
	if (name === "xargs") return args.some((arg) => arg === "git" || shellSearches.has(base(arg)));
	return false;
}

function commandSearch(command: string): boolean {
	return segments(command).some(segmentSearch);
}

function isSearch(toolName: string, input: Record<string, unknown>): boolean {
	if (builtinSearches.has(toolName)) return true;
	if (toolName !== "bash") return false;
	return commandSearch(String(input.command ?? ""));
}

// -i and its combined forms: -i, -i.bak, -pi, -ni, and --in-place.
const inPlace = /^-[A-Za-z]*i/;

function inPlaceEdit(args: string[]): boolean {
	return args.some((arg) => arg.startsWith("--in-place") || inPlace.test(arg));
}

// find runs a command on every match, or deletes: that is a chain.
const findChain = new Set(["-exec", "-execdir", "-ok", "-okdir", "-delete"]);
// A search tool feeding one of these is a search chain.
const searchSource = new Set(["find", "fd", "rg", "ag"]);
// fd piped into rg filters file names, so only grep and ag are pipe sinks; xargs
// also counts rg.
const pipeSink = new Set(["grep", "ag"]);
const xargsSink = new Set(["grep", "rg", "ag"]);

function denyFindChains(line: string): boolean {
	const list = segments(line);
	for (const segment of list) {
		const { name, args } = command(segment);
		if (name === "find" && args.some((arg) => findChain.has(arg))) return true;
	}
	for (let at = 0; at < list.length; at += 1) {
		if (!list[at].stdoutPipe || !searchSource.has(command(list[at]).name)) continue;
		for (let next = at + 1; next < list.length && list[next].stdinPipe; next += 1) {
			const { name, args } = command(list[next]);
			const sink = name === "xargs" ? args.some((arg) => xargsSink.has(base(arg)) || arg === "find") : pipeSink.has(name);
			if (sink) return true;
			if (!list[next].stdoutPipe) break;
		}
	}
	return false;
}

// find over a tree is a file search by name, path or type, which fd answers. A find reading a pipe is formatting stdin, so it stays.
function denyFind(line: string): boolean {
	return segments(line).some((segment) => !segment.stdinPipe && command(segment).name === "find");
}

function denySedInplace(line: string): boolean {
	return segments(line).some((segment) => {
		const { name, args } = command(segment);
		if (name === "sed" || name === "gsed") return inPlaceEdit(args);
		if (name === "perl") return args.some((arg) => inPlace.test(arg));
		return false;
	});
}

// flags of head and tail: -f, -F and --follow keep watching, so they are not a
// plain read of one file.
const follow = /^-[A-Za-z]*[fF]/;

// cat takes no counted flags, so every word that is not a flag names a file.
function plainFiles(args: string[]): string[] {
	return args.filter((arg) => arg !== "-" && !arg.startsWith("-"));
}

function countFiles(args: string[]): string[] {
	const out: string[] = [];
	for (let at = 0; at < args.length; at += 1) {
		const arg = args[at];
		if (arg === "-n" || arg === "-c" || arg === "-s" || arg === "--lines" || arg === "--bytes") {
			at += 1;
			continue;
		}
		if (arg.startsWith("-") || arg === "-") continue;
		out.push(arg);
	}
	return out;
}

// sed prints a line range to stdout when it has -n or a script that only
// prints, and names the file as the first word after the script.
function sedFiles(args: string[]): string[] {
	const out: string[] = [];
	let scripted = false;
	for (let at = 0; at < args.length; at += 1) {
		const arg = args[at];
		if (arg.startsWith("-") && arg !== "-") {
			if (arg === "-e" || arg === "-f" || arg === "--expression" || arg === "--file") {
				scripted = true;
				at += 1;
			}
			continue;
		}
		if (!scripted) {
			scripted = true;
			continue;
		}
		out.push(arg);
	}
	return out;
}

function sedPrints(args: string[]): boolean {
	if (args.some((arg) => arg === "-n" || /^-[A-Za-z]*n$/.test(arg))) return true;
	return args.some((arg) => /^[\d$,; ]*p$/.test(arg));
}

const plainSearch = new Set(["grep", "egrep", "fgrep", "rg", "ag"]);
// rg and ag search the tree under the current directory without a flag.
const recursiveByDefault = new Set(["rg", "ag"]);
const recursive = /^-[A-Za-z]*[rR]/;
// grep flags whose value is the pattern, a type, a glob or a count, not a path.
const grepValue = new Set([
	"-e",
	"-f",
	"-A",
	"-B",
	"-C",
	"-m",
	"-t",
	"-g",
	"-j",
	"--regexp",
	"--file",
	"--type",
	"--glob",
	"--max-count",
	"--after-context",
	"--before-context",
	"--context",
]);

// The file and directory arguments of a grep, which follow its pattern.
function searchTargets(args: string[]): string[] {
	const out: string[] = [];
	let patterned = false;
	for (let at = 0; at < args.length; at += 1) {
		const arg = args[at];
		if (arg.startsWith("-") && arg !== "-") {
			if (grepValue.has(arg)) at += 1;
			continue;
		}
		if (!patterned) {
			patterned = true;
			continue;
		}
		out.push(arg);
	}
	return out;
}

// The root of the git repository the session runs in, found by walking up from
// the working directory (a worktree root has a .git file rather than a
// directory, so any entry counts), or "" outside one. Resolved once: the working
// directory does not change under a session.
let repoRoot: string | undefined;
function repositoryRoot(): string {
	if (repoRoot !== undefined) return repoRoot;
	repoRoot = "";
	let dir = process.cwd();
	for (;;) {
		try {
			statSync(join(dir, ".git"));
			repoRoot = realpathSync(dir);
			break;
		} catch {
			// No repository here; keep walking up.
		}
		const parent = dirname(dir);
		if (parent === dir) break;
		dir = parent;
	}
	return repoRoot;
}

// Resolve relative and missing paths from the session directory. A search
// outside the session's git repository is allowed.
function insideRepository(path: string): boolean {
	const root = repositoryRoot();
	if (!root) return false;
	let resolved = resolve(path.trim() || ".");
	try {
		resolved = realpathSync(resolved);
	} catch {
		// Missing targets keep their lexical scope.
	}
	const rel = relative(root, resolved);
	return rel !== ".." && !rel.startsWith(`..${sep}`) && !isAbsolute(rel);
}

// One segment is a raw content search over files.
function grepSearches(segment: segment, tools: tools): boolean {
	const { name, args } = command(segment);
	if (!name || !tools.rg) return false;
	if (name === "git") return args[0] === "grep" && !segment.stdinPipe;
	if (name === "rg" || !plainSearch.has(name)) return false;
	if (searchTargets(args).length > 0) return true;
	const recurses =
		recursiveByDefault.has(name) || args.some((arg) => arg.startsWith("--recursive") || recursive.test(arg));
	return recurses && !segment.stdinPipe;
}

// A grep with a file or directory argument, or one that recurses, searches
// files. One reading stdin is a filter, so it stays allowed. rg is the
// replacement, so it is never blocked, and with no rg on PATH there is nothing
// to name, so nothing is.
function denyGrepRg(line: string, tools: tools): boolean {
	return segments(line).some((segment) => grepSearches(segment, tools));
}

function denyCatHeadTail(line: string): boolean {
	return segments(line).some((segment) => {
		// A pipeline formats or takes its input elsewhere, and a redirect
		// writes a file, so neither is a bare read of one file.
		if (segment.stdinPipe || segment.stdoutPipe || segment.redirect) return false;
		const { name, args } = command(segment);
		if (name === "cat") return plainFiles(args).length === 1;
		if (name === "head" || name === "tail") {
			if (args.some((arg) => follow.test(arg) || arg.startsWith("--follow"))) return false;
			return countFiles(args).length === 1;
		}
		if (name === "sed") {
			if (inPlaceEdit(args)) return false;
			return sedPrints(args) && sedFiles(args).length === 1;
		}
		return false;
	});
}

// Two or three short lines: what is blocked, and the tool that replaces it. The
// 3rd block in a row adds one more.
const findReason = (
	{ fd }: tools,
	name: string,
	first: string,
	fallback: string,
) => `${guardMarker} (${name}): ` + (fd ? `${first}\n${ladder}` : fallback);

const rules: rule[] = [
	{
		name: "deny-find-chains",
		id: "find-chains",
		match: denyFindChains,
		reason: (tools) =>
			findReason(
				tools,
				"deny-find-chains",
				`find -exec, -delete and find | grep chains are blocked; use fd, or rg -g 'GLOB' PATTERN for content.`,
				`find -exec, -delete and find | grep chains are blocked. Use the built-in grep and find tools; fd is not installed.`,
			),
	},
	{
		name: "deny-find",
		id: "find",
		match: (line, tools) => tools.fd && denyFind(line),
		reason: (tools) =>
			findReason(
				tools,
				"deny-find",
				`find over a tree is blocked; use fd (fd PATTERN dir, fd -e go, fd -g '*.ts').`,
				`find over a tree is blocked; use fd.`,
			),
	},
	{
		name: "deny-sed-inplace",
		id: "sed-inplace",
		match: denySedInplace,
		reason: () =>
			`${guardMarker} (deny-sed-inplace): sed -i and perl -pi rewrite a file in place.\n` +
			`Use the built-in edit tool.`,
	},
	{
		name: "deny-grep-rg",
		id: "grep-rg",
		match: denyGrepRg,
		reason: () =>
			`${guardMarker} (deny-grep-rg): grep, egrep, fgrep, ag and git grep over files are blocked; ` +
			`use rg (rg -n PATTERN path, -g 'GLOB', -F literal, -w word, -i).\n${ladder}`,
	},
	{
		name: "deny-cat-head-tail",
		id: "cat-head-tail",
		match: denyCatHeadTail,
		reason: () =>
			`${guardMarker} (deny-cat-head-tail): a bare cat, head, tail or sed -n floods the context.\n` +
			`Use the built-in read tool with offset and limit (a section or the whole file).`,
	},
];

// pi's built-in grep and find walk the tree without a shell, so a shell rule
// alone leaves them as the way around the guard. They are refused while the
// shell replacement (rg for grep, fd for find) is on PATH; with it missing they
// are the fallback and stay allowed. `ls` lists a directory instead of
// searching, so it has no rule and stays allowed.
const builtinRules: builtinRule[] = [
	{
		name: "deny-builtin-grep",
		id: "builtin-grep",
		group: "builtin-search",
		match: (call, tools) => call.name === "grep" && tools.rg,
		reason:
			`${guardMarker} (deny-builtin-grep): the built-in grep tool is blocked; ` +
			`use rg in bash (rg -n PATTERN path, -g 'GLOB', -F literal, -w word, -i).\n${ladder}`,
	},
	{
		name: "deny-builtin-find",
		id: "builtin-find",
		group: "builtin-search",
		match: (call, tools) => call.name === "find" && tools.fd,
		reason:
			`${guardMarker} (deny-builtin-find): the built-in find tool is blocked; ` +
			`use fd in bash (fd PATTERN dir, fd -e go, fd -g '*.ts').\n${ladder}`,
	},
];

// deny-big-read: a whole-file read of a regular file over this many bytes floods
// the context for the rest of the session, so it is refused; a range (offset or
// limit) always passes. Images, PDFs and notebooks are read specially and stay.
// This is the pi side of hooks/claude/deny-big-read.sh.
const bigReadBytes = 30720;
const readExempt = new Set([".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf", ".ipynb"]);

// The block reason for a big read, or "" when the call is not one. Any path it
// cannot stat is allowed: a missing file or a directory is not a big read.
function bigReadReason(input: Record<string, unknown>): string {
	const path = input.path;
	if (typeof path !== "string" || !path.trim()) return "";
	const ranged = (key: string) => input[key] !== undefined && input[key] !== null && input[key] !== "";
	if (ranged("offset") || ranged("limit") || ranged("pages")) return "";
	if (readExempt.has(extname(path).toLowerCase())) return "";
	let size: number;
	try {
		const info = statSync(path);
		if (!info.isFile()) return "";
		size = info.size;
	} catch {
		return "";
	}
	if (size <= bigReadBytes) return "";
	const lookup = `Get the line numbers first: rg -n PATTERN file (a doc: rg -n "^#{1,3} " file).`;
	return (
		`${guardMarker} (deny-big-read): ${path} is ${Math.max(1, Math.round(size / 1024))} KB; reading it whole floods the context.\n` +
		`Use read with offset and limit and take only the section you need.\n` +
		lookup
	);
}

// The rules and the built-in rules ORCH_TOOL_GUARD_SKIP disables, by short name,
// by full name, or by group (`builtin-search` for both built-in rules at once).
function skipped(): Set<string> {
	const raw = process.env.ORCH_TOOL_GUARD_SKIP ?? "";
	return new Set(
		raw
			.split(",")
			.map((name) => name.trim())
			.filter(Boolean),
	);
}

// ORCH_TOOL_GUARD_SKIP=find-chains,find,sed-inplace,cat-head-tail disables
// single rules by short name or by full name.
function activeRules(): rule[] {
	const skip = skipped();
	return rules.filter((rule) => !skip.has(rule.id) && !skip.has(rule.name));
}

function activeBuiltinRules(): builtinRule[] {
	const skip = skipped();
	return builtinRules.filter((rule) => !skip.has(rule.id) && !skip.has(rule.name) && !skip.has(rule.group));
}

export default function (pi: ExtensionAPI) {
	// The ctx-only gate shell already refuses everything but one orch ctx call,
	// and has its own guard, so this one has to stay silent there.
	if (process.env.ORCH_CTX_ONLY_READY) return;
	const ready = installed();
	const guard = process.env.ORCH_TOOL_GUARD !== "off";
	const active = guard ? activeRules() : [];
	const activeBuiltins = guard ? activeBuiltinRules() : [];

	// A blocked command stays blocked, so the model learns not to use it. From
	// the third block in a row the reason also says the guard will not give way.
	// Any call the guard lets through ends the run.
	const giveWay = `\nBlocked 3 times: use the replacement above; this guard does not give way.`;
	let blocksInRow = 0;

	let remindersByResult = false;
	if (ready.orch) {
		try {
			pi.on("tool_result", async (event: ToolResultEvent) => {
				if (!isSearch(event.toolName, event.input)) return undefined;
				// A search outside the repository is not guarded, so it gets no note.
				if (!insideRepository(String(event.input.path ?? ""))) return undefined;
				const content = event.content ?? [];
				if (content.some((part) => part.type === "text" && part.text.includes(marker))) return undefined;
				const next = [...content, { type: "text" as const, text: reminder }];
				return event.structuredContent === undefined
					? { content: next }
					: { content: next, structuredContent: event.structuredContent };
			});
			remindersByResult = true;
		} catch {
			remindersByResult = false;
		}
	}

	// No result hook available: remind by blocking the first shell search only,
	// then let every later search through so the retry always runs.
	let fallbackReminded = false;

	// The reason a block carries: the rule's own text, and the give-way line from
	// the third block in a row.
	const blockReason = (reason: string) => reason + (blocksInRow >= 3 ? giveWay : "");

	pi.on("tool_call", async (event: ToolCallEvent) => {
		const input = event.input as Record<string, unknown>;
		// The built-in searches are the way around the shell rules, so they are
		// refused the same way while the shell replacement is on PATH.
		const builtin = activeBuiltins.find(
			(rule) => rule.match({ name: event.toolName, input }, ready) && insideRepository(String(input.path ?? "")),
		);
		if (builtin) {
			blocksInRow += 1;
			return { block: true, reason: blockReason(builtin.reason) };
		}
		const bigRead =
			guard && event.toolName === "read" && !skipped().has("big-read") && !skipped().has("deny-big-read")
				? bigReadReason(input)
				: "";
		if (bigRead) {
			blocksInRow += 1;
			return { block: true, reason: blockReason(bigRead) };
		}
		const line = event.toolName === "bash" ? String(input.command ?? "").trim() : "";
		if (!line) {
			blocksInRow = 0;
			return undefined;
		}
		for (const rule of active) {
			if (!rule.match(line, ready)) continue;
			blocksInRow += 1;
			return { block: true, reason: blockReason(rule.reason(ready)) };
		}
		blocksInRow = 0;
		if (!ready.orch || !guard || remindersByResult || fallbackReminded) return undefined;
		if (event.toolName !== "bash" || !commandSearch(line)) return undefined;
		fallbackReminded = true;
		return { block: true, reason: reminder };
	});
}
