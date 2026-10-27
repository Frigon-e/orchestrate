// Guards the model against five kinds of raw shell work and points it at the
// right tool instead. Each rule is a table entry with a name, a matcher and a
// reason: deny-find-chains (find -exec / find -delete / a search tool feeding a
// grep), deny-find (a find over a tree), deny-sed-inplace (sed -i, perl -pi),
// deny-cat-head-tail (a bare cat or sed -n on one file) and
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
import { accessSync, closeSync, constants, mkdirSync, openSync, realpathSync, statSync, writeSync } from "node:fs";
import { homedir } from "node:os";
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
	reason: (tools: tools, line: string) => string;
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
function segments(command: string, dqRaw = false): segment[] {
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
				// bash keeps the backslash before any character but these three
				if (dqRaw && !'"\\$`'.includes(ch)) text += "\\";
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

// The name and args of the first segment deny-cat-head-tail blocks, or undefined.
function catHeadTailHit(line: string): { name: string; args: string[] } | undefined {
	for (const segment of segments(line)) {
		// A pipeline formats or takes its input elsewhere, and a redirect
		// writes a file, so neither is a bare read of one file.
		if (segment.stdinPipe || segment.stdoutPipe || segment.redirect) continue;
		const { name, args } = command(segment);
		if (name === "cat" && plainFiles(args).length === 1) return { name, args };
		if (name === "sed" && !inPlaceEdit(args) && sedPrints(args) && sedFiles(args).length === 1) return { name, args };
	}
	return undefined;
}

function denyCatHeadTail(line: string): boolean {
	return catHeadTailHit(line) !== undefined;
}

// The script word of a sed: the value of -e, else the first word that is not a flag.
function sedScript(args: string[]): string {
	for (let at = 0; at < args.length; at += 1) {
		const arg = args[at];
		if (arg === "-e" || arg === "--expression") return args[at + 1] ?? "";
		if (arg === "-f" || arg === "--file") return "";
		if (!(arg.startsWith("-") && arg !== "-")) return arg;
	}
	return "";
}

const readTail = "as a parallel call in the same message as your other commands.";

// The block text: what the output is not, then the exact read call to send. pi's
// read tool takes path, offset and limit and accepts a relative path.
function catHeadTailReason(line: string): string {
	const head =
		`${guardMarker} (deny-cat-head-tail): cat and sed -n output is not a Read, so a later Edit of the file fails.\n`;
	const hit = catHeadTailHit(line);
	if (!hit) return head + `Send read with offset and limit instead, ${readTail}`;
	const path = (hit.name === "cat" ? plainFiles(hit.args) : sedFiles(hit.args))[0];
	const call = `read path=${path}`;
	const found = hit.name === "sed" ? /^\s*(\d+)(?:\s*,\s*(\d+))?\s*p\s*$/.exec(sedScript(hit.args)) : null;
	if (found) {
		const start = Number(found[1]);
		const end = Number(found[2] ?? found[1]);
		if (end >= start) return head + `Send ${call} offset=${start} limit=${end - start + 1} instead, ${readTail}`;
	}
	return head + `Send ${call} instead (add offset and limit for a section), ${readTail}`;
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
			`Use the built-in edit tool. If the mtime must stay the same, run touch -r FILE REF before the edit and touch -r REF FILE after. ` +
			`Do not rewrite files with python, awk or tee.`,
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
		reason: (_tools, line) => catHeadTailReason(line),
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

// ---------------------------------------------------------------------------
// Lookup coach: a non-blocking log of every lookup (jbcontext, orch lsp, rg, fd,
// read) and four reminders. hooks/claude/toolguard.py is the Claude side; both
// follow hooks/claude/coach_cases.json.
// ---------------------------------------------------------------------------
const coachPrefix = "orch lookup coach: ";
const coachText: Record<string, string> = {
	"concept-first":
		'Location unknown? Ask jbcontext first: jbcontext search "<one behavior>", one concept per query, several queries in one message.',
	"symbol-via-lsp": "For a definition, references or callers use orch lsp def|refs|callers <Name>; rg finds text, not uses.",
	"batch-lookups": "Send independent lookups together as parallel calls in one message; each one alone costs a turn.",
	"read-batch": "Read the ranges you already know as parallel reads in one message.",
	"lookup-ladder":
		'Before searching: location unknown -> jbcontext search "<one behavior>"; known symbol -> orch lsp def|refs|callers|impls <Name>; exact text -> rg. Send independent lookups as parallel calls in one message. Read known files or ranges with Read, not cat or sed -n.',
};
// Kinds that fire at most once per prompt window; the others once per session.
const perPrompt = new Set(["concept-first", "symbol-via-lsp"]);
const queryMax = 200;
const docExtensions = new Set(["md", "txt", "json", "yaml", "yml", "toml"]);
const docTypes = new Set([...docExtensions, "markdown"]);
const rgValue = new Set([...grepValue, "--iglob", "-T", "--type-not", "-r", "--replace", "-d", "--max-depth", "-M", "--max-columns"]);
const fdValue = new Set([
	"-e", "-E", "-t", "-d", "-x", "-X", "-S", "-c", "-j", "--extension", "--exclude", "--type", "--max-depth",
	"--size", "--changed-within", "--changed-before", "--owner", "--color", "--threads", "--base-directory",
]);
const jbValue = new Set(["--project-path", "--git-remote-url", "--limit", "--revision", "-p", "--path-filter", "--branch"]);
const lspValue = new Set(["--limit"]);
const identifier = /^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$/;

type lookup = { tool: string; query: string; symbol: boolean };

// The words of an argument list that are neither flags nor flag values.
function positionals(args: string[], valueFlags: Set<string>): string[] {
	const out: string[] = [];
	for (let at = 0; at < args.length; at += 1) {
		const arg = args[at];
		if (arg === "--") {
			out.push(...args.slice(at + 1));
			break;
		}
		if (arg.startsWith("-") && arg !== "-") {
			if (valueFlags.has(arg)) at += 1;
			continue;
		}
		out.push(arg);
	}
	return out;
}

function rgParts(args: string[]) {
	let pattern: string | undefined;
	const globs: string[] = [];
	const types: string[] = [];
	let word = false;
	let free: string[] = [];
	for (let at = 0; at < args.length; at += 1) {
		const arg = args[at];
		const next = args[at + 1] ?? "";
		if (arg === "--") {
			free = free.concat(args.slice(at + 1));
			break;
		}
		if (arg.startsWith("-") && arg !== "-") {
			if (arg === "-e" || arg === "--regexp") {
				pattern ??= next;
				at += 1;
				continue;
			}
			if (arg === "-g" || arg === "--glob" || arg === "--iglob") globs.push(next);
			else if (arg === "-t" || arg === "--type") types.push(next);
			else if (arg.startsWith("--glob=") || arg.startsWith("--iglob=")) globs.push(arg.slice(arg.indexOf("=") + 1));
			else if (arg.startsWith("--type=")) types.push(arg.slice(arg.indexOf("=") + 1));
			else if (arg === "--word-regexp" || (/^-[A-Za-z]+$/.test(arg) && arg.slice(1).includes("w"))) word = true;
			if (rgValue.has(arg)) at += 1;
			continue;
		}
		free.push(arg);
	}
	if (pattern === undefined && free.length) {
		pattern = free[0];
		free = free.slice(1);
	}
	return { pattern: pattern ?? "", paths: free, globs, types, word };
}

function docTarget(kind: string, value: string): boolean {
	value = value.trim();
	if (kind === "type") return docTypes.has(value);
	const name = value.split("/").pop() ?? "";
	if (!name.includes(".")) return false;
	const ext = value.slice(value.lastIndexOf(".") + 1).replace(/^\{|\}$/g, "");
	return ext !== "" && ext.split(",").every((part) => docExtensions.has(part));
}

// An rg whose pattern names a symbol, so orch lsp would answer it better.
function symbolRg(args: string[]): boolean {
	const { pattern, paths, globs, types, word } = rgParts(args);
	const targets: [string, string][] = [
		...globs.map((g): [string, string] => ["glob", g]),
		...types.map((t): [string, string] => ["type", t]),
		...paths.map((p): [string, string] => ["path", p]),
	];
	if (targets.length && targets.every(([kind, value]) => docTarget(kind, value))) return false;
	let text = pattern.replaceAll("\\b", "").replaceAll("\\<", "").replaceAll("\\>", "");
	text = text.replace(/^(func|def|class|type|interface) /, "").replace(/\\?\($/, "");
	if (!identifier.test(text)) return false;
	return word || text.includes(".") || text.includes("_") || /[A-Z]/.test(text.slice(1));
}

// The lookup the first command of a shell line is, or undefined.
function lookupShell(line: string): lookup | undefined {
	let list = segments(line, true);
	if (list.length && command(list[0]).name === "cd") list = list.slice(1);
	if (!list.length) return undefined;
	const { name, args } = command(list[0]);
	if (name === "jbcontext" && args[0] === "search")
		return { tool: "jbcontext", query: positionals(args.slice(1), jbValue)[0] ?? "", symbol: false };
	if (name === "orch" && args.length >= 2 && args[0] === "lsp" && !args[1].startsWith("-"))
		return { tool: `lsp:${args[1]}`, query: positionals(args.slice(2), lspValue).join(" "), symbol: false };
	if (name === "orch" && args.length >= 2 && args[0] === "ctx" && !args[1].startsWith("-"))
		return { tool: `ctx:${args[1]}`, query: args.slice(2).filter((w) => !w.startsWith("-")).join(" "), symbol: false };
	if (name === "rg") return { tool: "rg", query: rgParts(args).pattern, symbol: symbolRg(args) };
	if (name === "fd") return { tool: "fd", query: positionals(args, fdValue)[0] ?? "", symbol: false };
	return undefined;
}

function readQuery(path: string, offset: unknown, limit: unknown): string {
	const blank = (v: unknown) => v === undefined || v === null || v === "";
	if (blank(offset) && blank(limit)) return path;
	return `${path}:${blank(offset) ? "" : offset}-${blank(limit) ? "" : limit}`;
}

function lookupOf(toolName: string, input: Record<string, unknown>): lookup | undefined {
	if (toolName === "bash") return lookupShell(String(input.command ?? ""));
	if (toolName === "read") {
		const path = input.path;
		if (typeof path !== "string" || !path) return undefined;
		return { tool: "read", query: readQuery(path, input.offset, input.limit), symbol: false };
	}
	return undefined;
}

type coachState = {
	fired: Set<string>;
	firstSearchDone: boolean;
	conceptSeen: boolean;
	pending: string;
	turns: { id: number; nr: number; rd: number }[];
	prompt: number;
	windowFirst: "" | "good" | "bad";
};

// Open a new prompt window; true when the lookup ladder is due: on the session's
// first prompt, and after a window whose first search was rg or fd.
function promptStep(state: coachState, reminders: boolean): boolean {
	const due = reminders && (state.prompt === 0 || state.windowFirst === "bad");
	state.prompt += 1;
	state.windowFirst = "";
	state.firstSearchDone = false;
	state.conceptSeen = false;
	state.fired = new Set([...state.fired].filter((key) => !key.includes("@")));
	if (due) state.pending = "lookup-ladder";
	return due;
}

// Advance the session state for one lookup; returns the reminder fired on this
// call (or "") and whether it followed the previous reminder (null when none).
function coachStep(state: coachState, found: lookup, turn: number | null, reminders: boolean) {
	let followed: boolean | null = null;
	if (reminders && state.pending) {
		if (state.pending === "concept-first") followed = found.tool === "jbcontext";
		else if (state.pending === "lookup-ladder") followed = found.tool === "jbcontext" || found.tool.startsWith("lsp:");
		else followed = found.tool.startsWith("lsp:");
		state.pending = "";
	}
	if (!state.windowFirst) {
		if (found.tool === "rg" || found.tool === "fd") state.windowFirst = "bad";
		else if (found.tool === "jbcontext" || found.tool.startsWith("lsp:")) state.windowFirst = "good";
	}
	const last = state.turns[state.turns.length - 1];
	const newTurn = turn !== null && !(last && last.id === turn);
	const candidates: string[] = [];
	if ((found.tool === "rg" || found.tool === "fd") && !state.firstSearchDone) {
		state.firstSearchDone = true;
		if (!state.conceptSeen) candidates.push("concept-first");
	}
	if (found.tool === "jbcontext" || found.tool.startsWith("lsp:")) state.conceptSeen = true;
	if (found.tool === "rg" && found.symbol) candidates.push("symbol-via-lsp");
	if (newTurn && turn !== null && state.turns.length >= 2) {
		const [a, b] = state.turns.slice(-2);
		if (b.id === a.id + 1 && turn === b.id + 1) {
			if (a.nr === 1 && b.nr === 1) candidates.push("batch-lookups");
			if (a.rd === 1 && b.rd === 1 && a.nr === 0 && b.nr === 0) candidates.push("read-batch");
		}
	}
	let reminder = "";
	if (reminders) {
		const keyOf = (kind: string) => (perPrompt.has(kind) ? `${kind}@${state.prompt}` : kind);
		reminder = candidates.find((kind) => !state.fired.has(keyOf(kind))) ?? "";
		if (reminder) state.fired.add(keyOf(reminder));
		if (reminder === "concept-first" || reminder === "symbol-via-lsp") state.pending = reminder;
	}
	if (turn !== null) {
		if (newTurn) {
			state.turns.push({ id: turn, nr: 0, rd: 0 });
			state.turns = state.turns.slice(-3);
		}
		state.turns[state.turns.length - 1][found.tool === "read" ? "rd" : "nr"] += 1;
	}
	return { reminder, followed };
}

// rg, fd and orch lsp exit 1 with no output when nothing matched; pi reports that
// as an error result ending in "exited with code 1".
function lookupOutcome(found: lookup, text: string, isError: boolean) {
	if (isError) {
		const body = text.replace(/\n*(Command )?exited with code 1\s*$/, "");
		const clean = body !== text && body.trim() === "";
		const quiet = found.tool === "rg" || found.tool === "fd" || found.tool.startsWith("lsp:");
		return clean && quiet ? { ok: true, empty: true } : { ok: false, empty: false };
	}
	return { ok: true, empty: found.tool === "jbcontext" && text.includes("No relevant results found") };
}

type lookupRecord = {
	session: string;
	turn: number | null;
	found: lookup;
	ok: boolean;
	empty: boolean;
	bytes: number;
	reminder: string;
	followed: boolean | null;
};

// One write call per line, so concurrent sessions never interleave a record.
function appendLookup(record: lookupRecord, durationMs: number | null) {
	const dir = process.env.ORCH_TOOL_GUARD_STATE_DIR || join(homedir(), ".local", "state", "orchestrate");
	mkdirSync(dir, { recursive: true });
	const line = {
		ts: new Date().toISOString(),
		harness: "pi",
		session: record.session,
		repo: repositoryRoot() || process.cwd(),
		turn: record.turn,
		tool: record.found.tool,
		query: record.found.query.slice(0, queryMax),
		ok: record.ok,
		empty: record.empty,
		bytes: record.bytes,
		duration_ms: durationMs,
		reminder: record.reminder || null,
		followed: record.followed,
	};
	const fd = openSync(join(dir, "lookups.jsonl"), "a");
	try {
		writeSync(fd, JSON.stringify(line) + "\n");
	} finally {
		closeSync(fd);
	}
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

	// The lookup coach: ORCH_LOOKUP_COACH=off silences its reminders, ORCH_LOOKUP_LOG=off
	// its log, and ORCH_TOOL_GUARD=off both. pi runs one process per session, so the
	// state lives here. A record is written once both the result (which knows the
	// reminder) and tool_execution_end (which knows the duration) have been seen.
	const coachOn = guard && process.env.ORCH_LOOKUP_COACH !== "off";
	const logOn = guard && process.env.ORCH_LOOKUP_LOG !== "off";
	const coaching = coachOn || logOn;
	const state: coachState = { fired: new Set(), firstSearchDone: false, conceptSeen: false, pending: "", turns: [], prompt: 0, windowFirst: "" };
	let turn: number | null = null;
	const open = new Map<string, { record?: lookupRecord; ended?: boolean; durationMs?: number | null }>();
	const settle = (id: string, part: { record?: lookupRecord; ended?: boolean; durationMs?: number | null }) => {
		try {
			const entry = { ...open.get(id), ...part };
			if (entry.record && entry.ended) {
				open.delete(id);
				appendLookup(entry.record, entry.durationMs ?? null);
				return;
			}
			open.set(id, entry);
			if (open.size > 200) open.delete(open.keys().next().value as string);
		} catch {
			// The log must never disturb a session.
		}
	};
	const coachResult = (event: ToolResultEvent, ctx: unknown): string => {
		if (!coaching) return "";
		try {
			const found = lookupOf(event.toolName, event.input);
			if (!found) return "";
			const text = (event.content ?? []).map((part) => (part.type === "text" ? part.text : "")).join("");
			const { reminder: fired, followed } = coachStep(state, found, turn, coachOn);
			const manager = (ctx as { sessionManager?: { getSessionId?: () => string } } | undefined)?.sessionManager;
			const session = manager?.getSessionId?.() || `pi-${process.pid}`;
			const record = { session, turn, found, ...lookupOutcome(found, text, event.isError), bytes: Buffer.byteLength(text), reminder: fired, followed };
			if (logOn) settle(event.toolCallId, { record });
			return fired;
		} catch {
			return "";
		}
	};
	if (coaching) {
		try {
			pi.on("turn_start", async (event: { turnIndex: number }) => {
				turn = event.turnIndex;
			});
			// A prompt opens a new window; the ladder rides in as a message before the turn.
			if (coachOn) {
				pi.on("before_agent_start", async (event: { messageType?: string }) => {
					if (event.messageType !== "user") return undefined;
					try {
						if (!promptStep(state, true)) return undefined;
						return { message: { customType: "orch-lookup-coach", content: coachPrefix + coachText["lookup-ladder"], display: false } };
					} catch {
						return undefined;
					}
				});
			}
			if (logOn) {
				pi.on("tool_execution_end", async (event: { toolCallId: string; durationMs?: number }) => {
					settle(event.toolCallId, { ended: true, durationMs: event.durationMs ?? null });
				});
			}
		} catch {
			// An older pi without these events: lookups still log, with a null turn.
		}
	}

	let remindersByResult = false;
	if (ready.orch || coaching) {
		try {
			pi.on("tool_result", async (event: ToolResultEvent, ctx?: unknown) => {
				const content = event.content ?? [];
				const extra: { type: "text"; text: string }[] = [];
				// A search outside the repository is not guarded, so it gets no note.
				if (
					ready.orch &&
					isSearch(event.toolName, event.input) &&
					insideRepository(String(event.input.path ?? "")) &&
					!content.some((part) => part.type === "text" && part.text.includes(marker))
				) {
					extra.push({ type: "text", text: reminder });
				}
				const coached = coachResult(event, ctx);
				if (coached) extra.push({ type: "text", text: coachPrefix + coachText[coached] });
				if (!extra.length) return undefined;
				const next = [...content, ...extra];
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
			return { block: true, reason: blockReason(rule.reason(ready, line)) };
		}
		blocksInRow = 0;
		if (!ready.orch || !guard || remindersByResult || fallbackReminded) return undefined;
		if (event.toolName !== "bash" || !commandSearch(line)) return undefined;
		fallbackReminded = true;
		return { block: true, reason: reminder };
	});
}
