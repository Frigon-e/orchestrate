// The pi side of the orchestrate tool guard. It holds no rules: every decision
// is made by the orch-guard binary (tool/guard, built by scripts/build-guard),
// the same one Claude Code runs. This file only turns pi events into JSON
// requests and applies the JSON replies.
//
// What the guard does: it blocks raw shell work (find chains, find over a tree,
// sed -i, a bare cat or sed -n of one file, grep over files), pi's own grep and
// find tool while rg or fd answers, and a whole-file read of a file over 30 KB,
// each with a reason that names the replacement; it appends the lookup ladder to
// search results and logs every lookup (jbcontext, orch lsp, rg, fd, read) with
// the lookup coach's reminders. ORCH_TOOL_GUARD=off, ORCH_TOOL_GUARD_SKIP,
// ORCH_LOOKUP_COACH=off and ORCH_LOOKUP_LOG=off are read by the binary.
//
// Protocol, one process per call: `orch-guard pre --harness pi` for tool_call and
// `orch-guard coach --harness pi` for the other events. stdin is one JSON object,
// stdout one JSON object, exit 0. Both are documented in tool/guard/pi.go:
//
//   request  { event, session, turn, cwd, tool, input, toolCallId,
//              content?, isError?, durationMs?, resultHook? }
//   reply    { block?, reason?, appendText?, message? }
//
// The binary is $ORCH_GUARD_BIN (orch sets it), else
// ~/.agents/skills/orchestrate/hooks/bin/orch-guard. A missing binary, a timeout
// or a bad reply fails open: the call is allowed, nothing is added, and one
// warning per session goes to stderr.
import type { ExtensionAPI, ToolCallEvent, ToolResultEvent } from "@earendil-works/pi-coding-agent";
import { execFile } from "node:child_process";
import { accessSync, constants, statSync } from "node:fs";
import { homedir } from "node:os";
import { delimiter, join } from "node:path";

const timeoutMs = 5000;

type reply = { block?: boolean; reason?: string; appendText?: string[]; message?: string };

// True when an executable of this name is on PATH.
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

function guardBinary(): string {
	return process.env.ORCH_GUARD_BIN || join(homedir(), ".agents", "skills", "orchestrate", "hooks", "bin", "orch-guard");
}

export default function (pi: ExtensionAPI) {
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
	const base = (event: string, ctx: unknown, toolName: string, input: unknown, toolCallId: string) => ({
		event,
		session: sessionOf(ctx),
		turn,
		cwd: process.cwd(),
		tool: toolName,
		input,
		toolCallId,
	});

	if (coaching) {
		try {
			pi.on("turn_start", async (event: { turnIndex: number }) => {
				turn = event.turnIndex;
			});
			// A prompt opens a new window; the ladder rides in as a message before the turn.
			if (coachOn) {
				pi.on("before_agent_start", async (event: { messageType?: string }, ctx?: unknown) => {
					if (event.messageType !== "user") return undefined;
					const res = await ask("coach", { event: "before_agent_start", session: sessionOf(ctx), turn, cwd: process.cwd() });
					return res.message ? { message: { customType: "orch-lookup-coach", content: res.message, display: false } } : undefined;
				});
			}
			if (logOn) {
				pi.on(
					"tool_execution_end",
					async (event: { toolCallId: string; toolName: string; durationMs?: number }, ctx?: unknown) => {
						await ask("coach", {
							...base("tool_execution_end", ctx, event.toolName, undefined, event.toolCallId),
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
					...base("tool_result", ctx, event.toolName, event.input, event.toolCallId),
					content: content.map((part) => (part.type === "text" ? part.text : "")),
					isError: event.isError,
				});
				if (!res.appendText?.length) return undefined;
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
		const res = await ask("pre", { ...base("tool_call", ctx, event.toolName, event.input, event.toolCallId), resultHook });
		return res.block ? { block: true, reason: res.reason ?? "" } : undefined;
	});
}
