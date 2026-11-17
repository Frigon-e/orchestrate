// The pi side of the orchestrate tool guard. It holds no rules: every decision
// is made by the orch-guard binary (tool/guard, built by scripts/build-guard),
// the same one Claude Code runs. This file only turns pi events into JSON
// requests and applies the JSON replies.
//
// What the guard does: it blocks raw shell work (find chains, find over a tree,
// sed -i, a bare cat or sed -n of one file, grep over files), pi's own grep and
// find tool while rg or fd answers, and a whole-file read of a file over 30 KB (code: 2000 lines,
// 100 KB; a repeat read passes),
// each with a reason that names the replacement; it appends the lookup ladder to
// search results and logs every lookup (jbcontext, orch lsp, rg, fd, read) with
// the lookup coach's reminders. ORCH_TOOL_GUARD=off, ORCH_TOOL_GUARD_SKIP,
// ORCH_LOOKUP_COACH=off and ORCH_LOOKUP_LOG=off are read by the binary.
//
// Protocol, one process per call: `orch-guard pre --harness pi` for tool_call and
// `orch-guard coach --harness pi` for the other events. stdin is one JSON object,
// stdout one JSON object, exit 0. Both are documented in tool/guard/pi.go:
//
//   request  { event, session, turn, cwd, tool, input, toolCallId, parent,
//              content?, isError?, durationMs?, resultHook?, codemode? }
//   reply    { block?, reason?, appendText?, message? }
//
// The binary is $ORCH_GUARD_BIN (orch sets it), else
// ~/.agents/skills/orchestrate/hooks/bin/orch-guard. A missing binary, a timeout
// or a bad reply fails open: the call is allowed, nothing is added, and one
// warning per session goes to stderr.
//
// A call a Code Mode script makes (pi's codemode tool) carries parentToolCallId;
// it is sent as `parent`, and the binary treats such a call as a script's: no big
// read or range block, no reminder. Its result goes to the script, so nothing is
// ever appended to it here, whatever the binary replies.
import type { ExtensionAPI, ToolCallEvent, ToolResultEvent } from "@earendil-works/pi-coding-agent";
import { execFile } from "node:child_process";
import { accessSync, constants, readFileSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { delimiter, join } from "node:path";

const timeoutMs = 5000;

type reply = { block?: boolean; reason?: string; appendText?: string[]; message?: string };

// The first executable of this name on PATH, or "".
function findOnPath(name: string): string {
	for (const dir of (process.env.PATH ?? "").split(delimiter)) {
		if (!dir) continue;
		const path = join(dir, name);
		try {
			if (!statSync(path).isFile()) continue;
			accessSync(path, constants.X_OK);
			return path;
		} catch {
			// Not this directory; keep looking.
		}
	}
	return "";
}

// True when an executable of this name is on PATH.
function onPath(name: string): boolean {
	return findOnPath(name) !== "";
}

// The orch binary: $ORCH_BIN when it is an executable file, else orch on PATH, else "".
function orchBinary(): string {
	const env = process.env.ORCH_BIN;
	if (env) {
		try {
			if (statSync(env).isFile()) {
				accessSync(env, constants.X_OK);
				return env;
			}
		} catch {
			// Fall back to PATH.
		}
	}
	return findOnPath("orch");
}

// False when pi's built-in MCP support is off for this session: `-builtin:mcp` in the
// user or project settings (project wins), or --no-mcp / --no-extensions without an
// explicit `-e builtin:mcp`. pi reports a server registered without MCP support as an
// extension error in every session, so the orch server is not registered then.
function piMcpOn(cwd: string): boolean {
	const argv = process.argv.slice(2);
	if (argv.some((a, i) => (a === "-e" || a === "--extension") && argv[i + 1] === "builtin:mcp")) return true;
	if (argv.includes("--no-mcp") || argv.includes("--no-extensions") || argv.includes("-ne")) return false;
	let on = true;
	const agentDir = process.env.PI_CODING_AGENT_DIR || join(homedir(), ".pi", "agent");
	for (const file of [join(agentDir, "settings.json"), join(cwd, ".pi", "settings.json")]) {
		let list: unknown;
		try {
			list = JSON.parse(readFileSync(file, "utf8")).extensions;
		} catch {
			continue; // No settings file, or one pi itself will complain about.
		}
		if (!Array.isArray(list)) continue;
		for (const entry of list) {
			if (entry === "-builtin:mcp") on = false;
			else if (entry === "+builtin:mcp" || entry === "builtin:mcp") on = true;
		}
	}
	return on;
}

function guardBinary(): string {
	return process.env.ORCH_GUARD_BIN || join(homedir(), ".agents", "skills", "orchestrate", "hooks", "bin", "orch-guard");
}

export default function (pi: ExtensionAPI) {
	// The restricted-shell gate shell already refuses everything but one lookup call,
	// and has its own guard, so this one has to stay silent there.
	if (process.env.ORCH_LOOKUP_READY) return;

	// The orch MCP server (`orch mcp`: search and lsp), reached from codemode scripts
	// as mcp__orch__search and mcp__orch__lsp. A pi without the API or with its MCP
	// support off, or without an orch binary, registers nothing; ORCH_MCP=off skips it.
	const registerMcp = (pi as unknown as { registerMcpServer?: (name: string, config: Record<string, unknown>) => void })
		.registerMcpServer;
	if (typeof registerMcp === "function" && process.env.ORCH_MCP !== "off" && piMcpOn(process.cwd())) {
		try {
			const bin = orchBinary();
			if (bin) {
				registerMcp.call(pi, "orch", {
					command: bin,
					args: ["mcp"],
					exposure: "codemode",
					description: "orch code intelligence: search (concepts) and lsp (def, refs, callers, impls, symbols, hover, diagnostics)",
				});
			} else {
				console.warn("orch tool guard: orch is not on PATH and ORCH_BIN is not set; the orch MCP server is not registered");
			}
		} catch {
			// A pi that refuses the registration keeps the guard working.
		}
	}
	const guardOn = process.env.ORCH_TOOL_GUARD !== "off";
	const coachOn = guardOn && process.env.ORCH_LOOKUP_COACH !== "off";
	const logOn = guardOn && process.env.ORCH_LOOKUP_LOG !== "off";
	const coaching = coachOn || logOn;
	let turn: number | null = null;
	let warned = false;

	const warn = (why: string) => {
		if (warned) return;
		warned = true;
		console.error(`orch tool guard: ${why}; the guard is off for this session (${guardBinary()})`);
	};

	// One binary call. Any failure allows the tool call and adds nothing.
	const ask = (mode: "pre" | "coach", request: Record<string, unknown>): Promise<reply> =>
		new Promise((resolve) => {
			try {
				const child = execFile(
					guardBinary(),
					[mode, "--harness", "pi"],
					{ timeout: timeoutMs, maxBuffer: 1 << 20, env: process.env },
					(error, stdout) => {
						if (error) {
							warn(error.message.split("\n")[0]);
							return resolve({});
						}
						try {
							resolve(JSON.parse(stdout) as reply);
						} catch {
							warn("unreadable reply");
							resolve({});
						}
					},
				);
				// A missing binary closes stdin before the write; the callback reports it.
				child.stdin?.on("error", () => {});
				child.stdin?.end(JSON.stringify(request));
			} catch (error) {
				warn(String((error as Error).message ?? error));
				resolve({});
			}
		});

	const sessionOf = (ctx: unknown): string => {
		const manager = (ctx as { sessionManager?: { getSessionId?: () => string } } | undefined)?.sessionManager;
		return manager?.getSessionId?.() || `pi-${process.pid}`;
	};
	const parentOf = (event: unknown): string => (event as { parentToolCallId?: string }).parentToolCallId ?? "";
	const base = (event: string, ctx: unknown, toolName: string, input: unknown, toolCallId: string, parent: string) => ({
		event,
		session: sessionOf(ctx),
		turn,
		cwd: process.cwd(),
		tool: toolName,
		input,
		toolCallId,
		parent,
	});
	// True when pi's codemode tool is active, so lookups are calls inside a script.
	const codemodeActive = (): boolean => {
		try {
			const tools = (pi as unknown as { getActiveTools?: () => unknown[] }).getActiveTools?.() ?? [];
			return tools.some((tool) => (typeof tool === "string" ? tool : (tool as { name?: string })?.name) === "codemode");
		} catch {
			return false;
		}
	};

	if (coaching) {
		try {
			pi.on("turn_start", async (event: { turnIndex: number }) => {
				turn = event.turnIndex;
			});
			// A prompt opens a new window; the ladder rides in as a message before the turn.
			if (coachOn) {
				pi.on("before_agent_start", async (event: { messageType?: string }, ctx?: unknown) => {
					if (event.messageType !== "user") return undefined;
					const res = await ask("coach", {
							event: "before_agent_start",
							session: sessionOf(ctx),
							turn,
							cwd: process.cwd(),
							...(codemodeActive() ? { codemode: true } : {}),
						});
					return res.message ? { message: { customType: "orch-lookup-coach", content: res.message, display: false } } : undefined;
				});
			}
			if (logOn) {
				pi.on(
					"tool_execution_end",
					async (event: { toolCallId: string; toolName: string; durationMs?: number }, ctx?: unknown) => {
						await ask("coach", {
							...base("tool_execution_end", ctx, event.toolName, undefined, event.toolCallId, parentOf(event)),
							durationMs: event.durationMs ?? null,
						});
					},
				);
			}
		} catch {
			// An older pi without these events: lookups still log, with a null turn.
		}
	}

	let resultHook = false;
	if (onPath("orch") || coaching) {
		try {
			pi.on("tool_result", async (event: ToolResultEvent, ctx?: unknown) => {
				const content = event.content ?? [];
				const res = await ask("coach", {
					...base("tool_result", ctx, event.toolName, event.input, event.toolCallId, parentOf(event)),
					content: content.map((part) => (part.type === "text" ? part.text : "")),
					isError: event.isError,
				});
				if (parentOf(event) || !res.appendText?.length) return undefined;
				const next = [...content, ...res.appendText.map((text) => ({ type: "text" as const, text }))];
				return event.structuredContent === undefined
					? { content: next }
					: { content: next, structuredContent: event.structuredContent };
			});
			resultHook = true;
		} catch {
			resultHook = false;
		}
	}

	pi.on("tool_call", async (event: ToolCallEvent, ctx?: unknown) => {
		const res = await ask("pre", { ...base("tool_call", ctx, event.toolName, event.input, event.toolCallId, parentOf(event)), resultHook });
		return res.block ? { block: true, reason: res.reason ?? "" } : undefined;
	});
}
