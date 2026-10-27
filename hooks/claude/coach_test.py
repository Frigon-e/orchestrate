#!/usr/bin/env python3
"""Runs coach_cases.json through the real Claude lookup coach (toolguard.py coach).

Every call is a PostToolUse (or PostToolUseFailure) JSON event on stdin, with a
temp state dir and a synthetic transcript, exactly as Claude Code sends it.
The pi extension runs the same table in tool/ctx_reminder_test.go.

Run: python3 hooks/claude/coach_test.py
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest
from concurrent.futures import ThreadPoolExecutor

ROOT = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(ROOT, "lookup-coach.sh")
with open(os.path.join(ROOT, "coach_cases.json")) as handle:
    CASES = json.load(handle)

PREFIX = "orch lookup coach: "
LADDER = (
    'Before searching: location unknown -> jbcontext search "<one behavior>"; known symbol -> '
    "orch lsp def|refs|callers|impls <Name>; exact text -> rg. "
    "Send independent lookups as parallel calls in one message."
)


def claude_call(spec, ident):
    """The Claude tool_use block for a neutral call spec."""
    if "bash" in spec:
        return {"type": "tool_use", "id": ident, "name": "Bash", "input": {"command": spec["bash"]}}
    if "read" in spec:
        args = dict(spec["read"])
        args["file_path"] = args.pop("path")
        return {"type": "tool_use", "id": ident, "name": "Read", "input": args}
    if "lsp" in spec:
        return {"type": "tool_use", "id": ident, "name": "LSP", "input": spec["lsp"]}
    return {"type": "tool_use", "id": ident, "name": "mcp__jbcontext__search", "input": spec["mcp"]}


class Sandbox:
    def __init__(self, test):
        self.dir = tempfile.mkdtemp()
        test.addCleanup(lambda: __import__("shutil").rmtree(self.dir, ignore_errors=True))
        self.state = os.path.join(self.dir, "state")
        self.transcript = os.path.join(self.dir, "transcript.jsonl")

    def write_transcript(self, turns):
        """One assistant message per turn, holding a tool_use per call."""
        with open(self.transcript, "w") as out:
            out.write(json.dumps({"type": "user", "message": {"role": "user", "content": "go"}}) + "\n")
            for t, calls in enumerate(turns):
                blocks = [claude_call(spec, f"tu_{t}_{j}") for j, spec in enumerate(calls)]
                for block in blocks:  # Claude writes one entry per content block
                    out.write(json.dumps({"type": "assistant", "message": {"id": f"msg_{t}", "role": "assistant", "content": [block]}}) + "\n")

    def run(self, spec, ident, session="s1", failure=None, env=None, response=None, extra=None):
        block = claude_call(spec, ident)
        event = {
            "hook_event_name": "PostToolUseFailure" if failure is not None else "PostToolUse",
            "session_id": session,
            "transcript_path": self.transcript,
            "cwd": self.dir,
            "tool_name": block["name"],
            "tool_input": block["input"],
            "tool_use_id": ident,
        }
        if failure is not None:
            event["error"] = failure
        else:
            event["tool_response"] = response if response is not None else {"stdout": "ok", "stderr": ""}
        event.update(extra or {})
        merged = dict(os.environ)
        merged.update({"ORCH_TOOL_GUARD_STATE_DIR": self.state})
        merged.update(env or {})
        proc = subprocess.run([SCRIPT], input=json.dumps(event), capture_output=True, text=True, env=merged)
        return proc

    def log(self):
        path = os.path.join(self.state, "lookups.jsonl")
        if not os.path.exists(path):
            return []
        with open(path) as handle:
            return [json.loads(line) for line in handle if line.strip()]


class ClassifyTests(unittest.TestCase):
    def test_every_classification_case(self):
        for case in CASES["classify"]:
            if case.get("harness") == "pi" or case["name"].startswith("symbol "):
                continue
            with self.subTest(case["name"]):
                box = Sandbox(self)
                box.write_transcript([[case]])
                proc = box.run(case, "tu_0_0")
                self.assertEqual(proc.returncode, 0)
                lines = box.log()
                if case["lookup"] is None:
                    self.assertEqual(lines, [])
                    self.assertEqual(proc.stdout, "")
                    continue
                self.assertEqual(len(lines), 1)
                self.assertEqual((lines[0]["tool"], lines[0]["query"]), (case["lookup"]["tool"], case["lookup"]["query"]))

    def test_symbol_rule_cases(self):
        for case in CASES["classify"]:
            if not case["name"].startswith("symbol "):
                continue
            with self.subTest(case["name"]):
                box = Sandbox(self)
                prime = {"bash": 'jbcontext search "x"'}
                box.write_transcript([[prime], [case]])
                box.run(prime, "tu_0_0")
                box.run(case, "tu_1_0")
                want = "symbol-via-lsp" if case["symbol"] else None
                self.assertEqual(box.log()[1]["reminder"], want)


class SequenceTests(unittest.TestCase):
    def test_every_reminder_sequence(self):
        for seq in CASES["sequences"]:
            with self.subTest(seq["name"]):
                box = Sandbox(self)
                box.write_transcript(seq["turns"])
                fired = []
                followed = []
                for t, calls in enumerate(seq["turns"]):
                    for j, spec in enumerate(calls):
                        before = len(box.log())
                        proc = box.run(spec, f"tu_{t}_{j}")
                        self.assertEqual(proc.returncode, 0)
                        lines = box.log()[before:]
                        if not lines:
                            self.assertEqual(proc.stdout, "")
                            continue
                        line = lines[0]
                        self.assertEqual(line["turn"], f"msg_{t}")
                        if line["reminder"]:
                            fired.append([t, j, line["reminder"]])
                            out = json.loads(proc.stdout)["hookSpecificOutput"]
                            self.assertEqual(out["hookEventName"], "PostToolUse")
                            self.assertTrue(out["additionalContext"].startswith(PREFIX))
                        else:
                            self.assertEqual(proc.stdout, "")
                        if line["followed"] is not None:
                            followed.append([t, j, line["followed"]])
                self.assertEqual(fired, seq["expect"])
                self.assertEqual(followed, seq.get("followed", []))


class PromptTests(unittest.TestCase):
    """UserPromptSubmit: the lookup ladder and the per-prompt re-arm."""

    def prompt(self, box, session="s1", env=None):
        event = {"hook_event_name": "UserPromptSubmit", "session_id": session, "prompt": "go", "cwd": box.dir}
        merged = dict(os.environ)
        merged.update({"ORCH_TOOL_GUARD_STATE_DIR": box.state})
        merged.update(env or {})
        return subprocess.run([SCRIPT], input=json.dumps(event), capture_output=True, text=True, env=merged)

    def test_every_prompt_sequence(self):
        for seq in CASES["prompts"]:
            with self.subTest(seq["name"]):
                box = Sandbox(self)
                turns = [step for step in seq["steps"] if isinstance(step, list)]
                box.write_transcript(turns)
                fired, followed, t = [], [], 0
                for i, step in enumerate(seq["steps"]):
                    if isinstance(step, dict):
                        proc = self.prompt(box)
                        self.assertEqual(proc.returncode, 0)
                        if proc.stdout:
                            out = json.loads(proc.stdout)["hookSpecificOutput"]
                            self.assertEqual(out["hookEventName"], "UserPromptSubmit")
                            self.assertEqual(out["additionalContext"], PREFIX + LADDER)
                            fired.append([i, 0, "lookup-ladder"])
                        continue
                    for j, spec in enumerate(step):
                        before = len(box.log())
                        box.run(spec, f"tu_{t}_{j}")
                        line = box.log()[before]
                        if line["reminder"]:
                            fired.append([i, j, line["reminder"]])
                        if line["followed"] is not None:
                            followed.append([i, j, line["followed"]])
                    t += 1
                self.assertEqual(fired, seq["expect"])
                self.assertEqual(followed, seq["followed"])

    def test_prompt_writes_no_lookup_row(self):
        box = Sandbox(self)
        self.prompt(box)
        self.assertEqual(box.log(), [])

    def test_coach_off_and_guard_off_silence_the_ladder(self):
        for env in ({"ORCH_LOOKUP_COACH": "off"}, {"ORCH_TOOL_GUARD": "off"}):
            with self.subTest(env):
                box = Sandbox(self)
                proc = self.prompt(box, env=env)
                self.assertEqual((proc.returncode, proc.stdout), (0, ""))

    def test_broken_state_dir_fails_open(self):
        box = Sandbox(self)
        blocker = os.path.join(box.dir, "file")
        open(blocker, "w").close()
        proc = self.prompt(box, env={"ORCH_TOOL_GUARD_STATE_DIR": os.path.join(blocker, "x")})
        self.assertEqual((proc.returncode, proc.stdout), (0, ""))


class BehaviorTests(unittest.TestCase):
    RG = {"bash": "rg -n gateway src"}

    def test_log_line_fields(self):
        box = Sandbox(self)
        os.makedirs(os.path.join(box.dir, ".git"))
        box.write_transcript([[self.RG]])
        box.run(self.RG, "tu_0_0", response={"stdout": "ab", "stderr": "é"}, extra={"duration_ms": 12})
        line = box.log()[0]
        self.assertEqual(line["harness"], "claude")
        self.assertEqual(line["session"], "s1")
        self.assertEqual(os.path.realpath(line["repo"]), os.path.realpath(box.dir))
        self.assertEqual((line["bytes"], line["duration_ms"], line["ok"], line["empty"]), (4, 12, True, False))
        self.assertTrue(line["ts"].endswith("Z"))

    def test_clean_exit_1_is_an_empty_result_not_an_error(self):
        box = Sandbox(self)
        box.write_transcript([[self.RG]])
        box.run(self.RG, "tu_0_0", failure="Exit code 1")
        line = box.log()[0]
        self.assertEqual((line["ok"], line["empty"]), (True, True))

    def test_exit_1_with_stderr_and_exit_2_are_errors(self):
        for error in ("Exit code 1\nrg: bad regex", "Exit code 2\n", "boom"):
            with self.subTest(error):
                box = Sandbox(self)
                box.write_transcript([[self.RG]])
                proc = box.run(self.RG, "tu_0_0", failure=error)
                line = box.log()[0]
                self.assertEqual((line["ok"], line["empty"]), (False, False))
                self.assertEqual(proc.returncode, 0)

    def test_jbcontext_no_result_text_is_empty(self):
        spec = {"bash": 'jbcontext search "zzqq"'}
        box = Sandbox(self)
        box.write_transcript([[spec]])
        out = "Searched revision: 4cd05cebea6c\n\nNo relevant results found. Try rephrasing your query.\n"
        box.run(spec, "tu_0_0", response={"stdout": out, "stderr": ""})
        self.assertEqual((box.log()[0]["ok"], box.log()[0]["empty"]), (True, True))

    def test_failure_event_names_itself_in_the_reminder(self):
        box = Sandbox(self)
        box.write_transcript([[self.RG]])
        proc = box.run(self.RG, "tu_0_0", failure="Exit code 1")
        out = json.loads(proc.stdout)["hookSpecificOutput"]
        self.assertEqual(out["hookEventName"], "PostToolUseFailure")

    def test_unknown_tool_use_id_logs_a_null_turn(self):
        box = Sandbox(self)
        box.write_transcript([[self.RG]])
        box.run(self.RG, "tu_missing")
        self.assertIsNone(box.log()[0]["turn"])

    def test_coach_off_logs_without_reminders(self):
        box = Sandbox(self)
        box.write_transcript([[self.RG]])
        proc = box.run(self.RG, "tu_0_0", env={"ORCH_LOOKUP_COACH": "off"})
        self.assertEqual(proc.stdout, "")
        self.assertIsNone(box.log()[0]["reminder"])

    def test_log_off_still_reminds(self):
        box = Sandbox(self)
        box.write_transcript([[self.RG]])
        proc = box.run(self.RG, "tu_0_0", env={"ORCH_LOOKUP_LOG": "off"})
        self.assertTrue(json.loads(proc.stdout)["hookSpecificOutput"]["additionalContext"].startswith(PREFIX))
        self.assertEqual(box.log(), [])

    def test_guard_off_disables_both(self):
        box = Sandbox(self)
        box.write_transcript([[self.RG]])
        proc = box.run(self.RG, "tu_0_0", env={"ORCH_TOOL_GUARD": "off"})
        self.assertEqual(proc.stdout, "")
        self.assertEqual(box.log(), [])

    def test_broken_input_exits_0(self):
        proc = subprocess.run([SCRIPT], input="not json", capture_output=True, text=True)
        self.assertEqual((proc.returncode, proc.stdout), (0, ""))

    def test_unwritable_state_dir_fails_open(self):
        box = Sandbox(self)
        box.write_transcript([[self.RG]])
        blocker = os.path.join(box.dir, "file")
        open(blocker, "w").close()
        proc = box.run(self.RG, "tu_0_0", env={"ORCH_TOOL_GUARD_STATE_DIR": os.path.join(blocker, "x")})
        self.assertEqual((proc.returncode, proc.stdout), (0, ""))

    def test_parallel_hooks_keep_one_line_each_and_one_reminder(self):
        calls = [{"bash": f"rg -n gateway{i} src"} for i in range(8)]
        box = Sandbox(self)
        box.write_transcript([calls])
        with ThreadPoolExecutor(8) as pool:
            procs = list(pool.map(lambda j: box.run(calls[j], f"tu_0_{j}"), range(8)))
        self.assertEqual(len(box.log()), 8)
        self.assertEqual(sum(1 for p in procs if p.stdout), 1)
        self.assertEqual(sum(1 for line in box.log() if line["reminder"]), 1)

    def test_sessions_do_not_share_reminders(self):
        box = Sandbox(self)
        box.write_transcript([[self.RG]])
        box.run(self.RG, "tu_0_0", session="a")
        proc = box.run(self.RG, "tu_0_0", session="b")
        self.assertTrue(proc.stdout)


if __name__ == "__main__":
    unittest.main()
