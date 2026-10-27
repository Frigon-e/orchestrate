#!/usr/bin/env python3
"""Runs every case in toolguard_cases.json through the real Claude hooks.

The hooks are shell scripts, so this drives them as Claude Code does: a
PreToolUse JSON event on stdin, exit 0 to allow, exit 2 with the reason on
stderr to block. PATH holds only stubs for the tools a situation names, so the
absence of rg and fd is real and not an accident of the machine.

Run: python3 hooks/claude/toolguard_test.py
"""

import contextlib
import json
import os
import shutil
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.abspath(__file__))
CASES = os.path.join(ROOT, "toolguard_cases.json")
GUARD_MARKER = "orch tool guard"

SCRIPTS = [
    "deny-find-chains.sh",
    "deny-sed-inplace.sh",
    "deny-cat-head-tail.sh",
    "deny-grep-rg.sh",
    "deny-big-read.sh",
]

# Which script owns a rule. Plain find shares deny-find-chains.sh.
HOOK_FOR_RULE = {
    "find-chains": "deny-find-chains.sh",
    "find": "deny-find-chains.sh",
    "sed-inplace": "deny-sed-inplace.sh",
    "cat-head-tail": "deny-cat-head-tail.sh",
    "grep-rg": "deny-grep-rg.sh",
}

BIG_READ_HOOK = "deny-big-read.sh"

# The replacement a block must name, given the tools on PATH and the rule. A
# plain find and a grep are only blocked when fd or rg is on PATH, so they
# always name it; a find chain with no fd names a plain find.
REPLACEMENT = {
    "fd-rg": {
        "find-chains": "use fd",
        "find": "use fd",
        "sed-inplace": "built-in Edit",
        "cat-head-tail": "built-in Read",
        "grep-rg": "use rg",
    },
    "rg": {
        "find-chains": "run a plain find",
        "sed-inplace": "built-in Edit",
        "cat-head-tail": "built-in Read",
        "grep-rg": "use rg",
    },
    "fd": {
        "find-chains": "use fd",
        "find": "use fd",
        "sed-inplace": "built-in Edit",
        "cat-head-tail": "built-in Read",
        "grep-rg": "use rg",
    },
    "none": {
        "find-chains": "run a plain find",
        "sed-inplace": "built-in Edit",
        "cat-head-tail": "built-in Read",
        "grep-rg": "use rg",
    },
}

# Every example the task requires, with the decision it must get. Presence is
# checked here; the shared case file is what both harness tests execute.
REQUIRED = [
    ("find . -name '*.log' -exec rm {} \\;", "fd-rg", "find-chains"),
    ("find . -name '*.ts' | grep -v node_modules", "fd-rg", "find-chains"),
    ("find . -type f | xargs grep -l TODO", "fd-rg", "find-chains"),
    ("sed -i 's/foo/bar/' file.txt", "fd-rg", "sed-inplace"),
    ("sed -i.bak 's/foo/bar/' file.txt", "fd-rg", "sed-inplace"),
    ("sed -i '' 's/foo/bar/' file.txt", "fd-rg", "sed-inplace"),
    ("gsed -i 's/foo/bar/' file.txt", "fd-rg", "sed-inplace"),
    ("sed --in-place 's/foo/bar/' file.txt", "fd-rg", "sed-inplace"),
    ("cat notes.txt", "fd-rg", "cat-head-tail"),
    ("head -n 20 notes.txt", "fd-rg", ""),
    ("tail -c 100 notes.txt", "fd-rg", ""),
    ("sudo cat /etc/hosts", "fd-rg", "cat-head-tail"),
    ("sed 's/foo/bar/' file.txt", "fd-rg", ""),
    ("git diff | sed 's/^/  /'", "fd-rg", ""),
    ("tail -f /var/log/app.log", "fd-rg", ""),
    ("cat file1 file2", "fd-rg", ""),
    ("cat notes.txt | grep TODO", "fd-rg", ""),
    ("head -n 5 file | wc -l", "fd-rg", ""),
    ("cat notes.txt > copy.txt", "fd-rg", ""),
    ('grep -rn "findUser" src', "fd-rg", "grep-rg"),
    ("ls my-find-exec-project/", "fd-rg", ""),
    ("grep -rn x src", "fd-rg", "grep-rg"),
    ("rg Foo internal/", "fd-rg", ""),
    ("go test ./... | grep FAIL", "fd-rg", ""),
    ("rg -n foo tool", "fd-rg", ""),
    ("grep -rn foo .", "fd-rg", "grep-rg"),
    ("grep -rn foo .", "none", ""),
]


LADDER = (
    'Lookup order: jbcontext search "<one behavior>" if the location is unknown, '
    "orch lsp def|refs|callers <Name> for a known symbol, rg for exact text, "
    "fd for file names; send independent lookups in one message."
)

# The search reasons, byte for byte.
GREP_REASON = (
    "orch tool guard (deny-grep-rg): grep, egrep, fgrep, ag and git grep over files are blocked; "
    "use rg (rg -n PATTERN path, -g 'GLOB', -F literal, -w word, -i).\n" + LADDER
)
FIND_REASON = (
    "orch tool guard (deny-find): find over a tree is blocked; "
    "use fd (fd PATTERN dir, fd -e go, fd -g '*.ts').\n" + LADDER
)
FIND_CHAINS_REASON = (
    "orch tool guard (deny-find-chains): find -exec, -delete and find | grep chains are blocked; "
    "use fd, or rg -g 'GLOB' PATTERN for content.\n" + LADDER
)
BIG_READ_LOOKUP = 'Get the line numbers first: rg -n PATTERN file (a doc: rg -n "^#{1,3} " file).'


def hook_run(checker, script, command, situation, extra=None):
    """One Bash hook run, the way Claude Code drives it."""
    payload = json.dumps(
        {
            "hook_event_name": "PreToolUse",
            "tool_name": "Bash",
            "tool_input": {"command": command},
        }
    )
    return subprocess.run(
        [os.path.join(ROOT, script)],
        input=payload,
        text=True,
        capture_output=True,
        env=checker.env(situation, extra),
    )


def check_generic_reasons(checker):
    """The search reasons, byte for byte, and no mention of orch ctx anywhere."""
    failures = []
    for script, command, want in (
        ("deny-grep-rg.sh", "grep -rn Normalize src", GREP_REASON),
        ("deny-find-chains.sh", "find . -name '*.ts'", FIND_REASON),
        ("deny-find-chains.sh", "find . -name x -exec ls {} \\;", FIND_CHAINS_REASON),
    ):
        done = hook_run(checker, script, command, "fd-rg")
        if done.returncode != 2 or done.stderr.strip() != want:
            failures.append(f"{command}: the reason is not the fixed text byte for byte: {done.stderr.strip()!r}")
    # With no rg a grep has no replacement and passes; with no fd a plain find
    # passes the same way (Claude's Glob is not loaded by default on macOS).
    done = hook_run(checker, "deny-grep-rg.sh", "grep -rn Normalize src", "none")
    if done.returncode != 0 or done.stderr.strip():
        failures.append(f"grep without rg was not allowed: {done.stderr.strip()!r}")
    done = hook_run(checker, "deny-find-chains.sh", "find . -name '*.ts'", "none")
    if done.returncode != 0 or done.stderr.strip():
        failures.append(f"find without fd was not allowed: {done.stderr.strip()!r}")
    return failures


def read_hook(checker, payload, situation="fd-rg", extra=None):
    """One deny-big-read hook run, the way Claude Code drives it."""
    return subprocess.run(
        [os.path.join(ROOT, BIG_READ_HOOK)],
        input=payload if isinstance(payload, str) else json.dumps(payload),
        text=True,
        capture_output=True,
        env=checker.env(situation, extra),
    )


def check_big_read(checker, workdir, cases):
    """The big-read hook: the shared table, the reason, the switches and the
    fail-open payloads. Returns the failures and the table's counts."""
    failures = []
    blocked = allowed = 0
    files = os.path.join(workdir, "big-read-files")
    os.makedirs(files)

    def event(path, extra=None):
        tool_input = {"file_path": path}
        tool_input.update(extra or {})
        return {"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_input": tool_input}

    big = os.path.join(files, "big.txt")
    for case in cases:
        path = os.path.join(files, case["file"])
        kind = case.get("kind", "file")
        if kind == "dir":
            os.makedirs(path, exist_ok=True)
        elif kind == "file" and not os.path.exists(path):
            with open(path, "wb") as handle:
                handle.write(b"x" * case["bytes"])
        payload = event(path, case["input"])
        if not case["block"]:
            allowed += 1
            done = read_hook(checker, payload, case["situation"])
            if done.returncode != 0 or done.stderr.strip():
                failures.append(f"{case['name']}: exit {done.returncode}, want 0: {done.stderr.strip()}")
            continue
        blocked += 1
        for attempt in ("first", "retry"):
            label = case["name"] + (" (retry)" if attempt == "retry" else "")
            done = read_hook(checker, payload, case["situation"])
            if done.returncode != 2:
                failures.append(f"{label}: exit {done.returncode}, want 2: {done.stderr.strip()}")
                continue
            reason = done.stderr.strip()
            lines = [entry for entry in reason.splitlines() if entry.strip()]
            if "(deny-big-read)" not in reason or GUARD_MARKER not in reason:
                failures.append(f"{label}: reason does not name the rule and the guard: {reason}")
            if not 2 <= len(lines) <= 5:
                failures.append(f"{label}: reason is {len(lines)} lines, want 2 to 5: {reason}")
            if f"{os.path.getsize(path) / 1024:.0f} KB" not in reason:
                failures.append(f"{label}: reason does not name the size in KB: {reason}")
            if "offset and limit" not in reason:
                failures.append(f"{label}: reason does not name offset and limit: {reason}")
            if BIG_READ_LOOKUP not in reason:
                failures.append(f"{label}: reason must carry the rg -n lookup: {reason}")
            if "orch ctx" in reason:
                failures.append(f"{label}: reason names orch ctx: {reason}")
            if done.stdout:
                failures.append(f"{label}: block wrote to stdout: {done.stdout.strip()}")

    # A retry with a limit passes where the bare read was blocked.
    if read_hook(checker, event(big)).returncode != 2 or read_hook(checker, event(big, {"limit": 100})).returncode != 0:
        failures.append("the bare read was not blocked, or the retry with limit was not allowed")

    # The switches: ORCH_TOOL_GUARD=off, the short and the full rule name.
    for extra, label in (
        ({"ORCH_TOOL_GUARD": "off"}, "ORCH_TOOL_GUARD=off"),
        ({"ORCH_TOOL_GUARD_SKIP": "big-read"}, "the big-read skip"),
        ({"ORCH_TOOL_GUARD_SKIP": "deny-big-read"}, "the deny-big-read skip"),
        ({"ORCH_TOOL_GUARD_SKIP": "grep-rg, big-read"}, "a skip list"),
    ):
        done = read_hook(checker, event(big), "fd-rg", extra)
        if done.returncode != 0 or done.stderr.strip():
            failures.append(f"{label} did not disable the rule: {done.stderr.strip()}")
    done = read_hook(checker, event(big), "fd-rg", {"ORCH_TOOL_GUARD_SKIP": "grep-rg,find"})
    if done.returncode != 2:
        failures.append("another rule's skip also disabled big-read")

    # Fail open on anything but a Read of a path.
    payloads = [
        ("broken JSON", "not json"),
        ("a JSON list", "[]"),
        ("another tool", {"tool_name": "Grep", "tool_input": {"file_path": big}}),
        ("no tool name", {"tool_input": {"file_path": big}}),
        ("no tool input", {"tool_name": "Read"}),
        ("a broken tool_input", {"tool_name": "Read", "tool_input": big}),
        ("no file_path", {"tool_name": "Read", "tool_input": {}}),
        ("an empty file_path", {"tool_name": "Read", "tool_input": {"file_path": " "}}),
        ("a number as file_path", {"tool_name": "Read", "tool_input": {"file_path": 5}}),
        ("a NUL in file_path", {"tool_name": "Read", "tool_input": {"file_path": big + "\0"}}),
    ]
    for name, payload in payloads:
        done = read_hook(checker, payload)
        if done.returncode != 0 or done.stderr.strip():
            failures.append(f"{name}: did not allow cleanly: {done.stderr.strip()}")
    return failures, blocked, allowed


class Checker:
    def __init__(self, cases, situations, paths):
        self.cases = cases
        self.situations = situations
        self.paths = paths
        self.blocked = 0
        self.allowed = 0
        self.failures = []

    def fail(self, case, message):
        self.failures.append(f"{case}: {message}")

    def run_case(self, case, hook):
        payload = json.dumps(
            {
                "hook_event_name": "PreToolUse",
                "tool_name": "Bash",
                "tool_input": {"command": case["command"]},
            }
        )
        return subprocess.run(
            [os.path.join(ROOT, hook)],
            input=payload,
            text=True,
            capture_output=True,
            env=self.env(case["situation"]),
        )

    def env(self, situation, extra=None):
        env = {
            "PATH": self.paths[situation],
            "HOME": os.environ.get("HOME", ""),
        }
        env.update(extra or {})
        return env

    def test(self):
        for case in self.cases:
            rule = case["rule"]
            if rule:
                self.blocked += 1
                self.expect_block(case, rule)
            else:
                self.allowed += 1
                self.expect_allow(case)

    def expect_block(self, case, rule):
        hook = HOOK_FOR_RULE.get(rule)
        if hook is None:
            self.fail(case["name"], f"unknown rule {rule!r}")
            return
        first = self.run_case(case, hook)
        # No retry escape: the same command is blocked every time.
        second = self.run_case(case, hook)
        for attempt in (first, second):
            label = case["name"] + " (retry)" if attempt is second else case["name"]
            if attempt.returncode != 2:
                self.fail(label, f"exit {attempt.returncode}, want 2: {attempt.stderr.strip()}")
                continue
            reason = attempt.stderr.strip()
            if f"(deny-{rule})" not in reason:
                self.fail(label, f"reason does not name deny-{rule}: {reason}")
            if GUARD_MARKER not in reason:
                self.fail(label, f"reason does not name the guard: {reason}")
            lines = [line for line in reason.splitlines() if line.strip()]
            if not 1 <= len(lines) <= 3:
                self.fail(label, f"reason is {len(lines)} lines, want 1 to 3: {reason}")
            if "orch ctx" in reason:
                self.fail(label, f"reason names orch ctx: {reason}")
            want = REPLACEMENT[case["situation"]][rule]
            if want not in reason:
                self.fail(label, f"reason must name {want!r}: {reason}")
            if attempt.stdout:
                self.fail(label, f"block wrote to stdout: {attempt.stdout.strip()}")

    def expect_allow(self, case):
        for hook in SCRIPTS:
            done = self.run_case(case, hook)
            if done.returncode != 0:
                self.fail(
                    case["name"],
                    f"{hook} exited {done.returncode}: {done.stderr.strip()}",
                )
            elif done.stderr.strip():
                self.fail(case["name"], f"{hook} wrote to stderr: {done.stderr.strip()}")


def stubs(directory, names):
    """A stand-in executable per name, so PATH holds exactly the tools a
    situation names."""
    os.makedirs(directory, exist_ok=True)
    for name in names:
        path = os.path.join(directory, name)
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("#!/bin/sh\nexit 0\n")
        os.chmod(path, 0o700)


def support_bin(directory):
    """A PATH with only what the shell wrappers themselves need."""
    os.makedirs(directory, exist_ok=True)
    for name in ("bash", "python3", "dirname"):
        found = shutil.which(name)
        if not found:
            raise SystemExit(f"toolguard_test: {name} is not installed")
        link = os.path.join(directory, name)
        with contextlib.suppress(FileExistsError):
            os.symlink(found, link)


def shell_checks():
    failures = []
    bash = shutil.which("bash")
    shellcheck = shutil.which("shellcheck")
    for script in SCRIPTS:
        path = os.path.join(ROOT, script)
        if not os.access(path, os.X_OK):
            failures.append(f"{script}: not executable")
        if bash:
            done = subprocess.run([bash, "-n", path], text=True, capture_output=True)
            if done.returncode != 0:
                failures.append(f"{script}: bash -n: {done.stderr.strip()}")
        if shellcheck:
            done = subprocess.run(
                [shellcheck, "-x", path], text=True, capture_output=True
            )
            if done.returncode != 0:
                failures.append(f"{script}: shellcheck: {done.stdout.strip()}")
    return failures


def required_cases(cases):
    found = {(case["command"], case["situation"]): case["rule"] for case in cases}
    failures = []
    for command, situation, rule in REQUIRED:
        key = (command, situation)
        if key not in found:
            failures.append(f"required case {command!r} ({situation}) is missing")
        elif found[key] != rule:
            failures.append(
                f"required case {command!r} ({situation}) has rule {found[key]!r}, want {rule!r}"
            )
    return failures


def check_switches(checker):
    """ORCH_TOOL_GUARD=off and ORCH_TOOL_GUARD_SKIP are the only escapes."""
    failures = []
    sample = {
        "find-chains": ("deny-find-chains.sh", "fd -e go | grep Gateway"),
        "find": ("deny-find-chains.sh", "find . -name '*.ts'"),
        "sed-inplace": ("deny-sed-inplace.sh", "sed -i 's/a/b/' file.txt"),
        "cat-head-tail": ("deny-cat-head-tail.sh", "cat notes.txt"),
        "grep-rg": ("deny-grep-rg.sh", "grep -rn x src"),
    }

    def run(rule, situation, extra):
        hook, command = sample[rule]
        payload = json.dumps(
            {
                "hook_event_name": "PreToolUse",
                "tool_name": "Bash",
                "tool_input": {"command": command},
            }
        )
        return subprocess.run(
            [os.path.join(ROOT, hook)],
            input=payload,
            text=True,
            capture_output=True,
            env=checker.env(situation, extra),
        )

    for rule in sample:
        done = run(rule, "fd-rg", {"ORCH_TOOL_GUARD": "off"})
        if done.returncode != 0:
            failures.append(f"ORCH_TOOL_GUARD=off did not disable {rule}: {done.stderr.strip()}")

    skip_four = "find-chains,sed-inplace,cat-head-tail,grep-rg"
    for rule in skip_four.split(","):
        done = run(rule, "fd-rg", {"ORCH_TOOL_GUARD_SKIP": skip_four})
        if done.returncode != 0:
            failures.append(f"the four-name skip did not disable {rule}: {done.stderr.strip()}")
    # Plain find is a separate rule id, so the four-name skip leaves it on.
    done = run("find", "fd-rg", {"ORCH_TOOL_GUARD_SKIP": skip_four})
    if done.returncode != 2 or "(deny-find)" not in done.stderr:
        failures.append("the four-name skip disabled plain find, which is its own rule")

    done = run("find", "fd-rg", {"ORCH_TOOL_GUARD_SKIP": "find-chains"})
    if done.returncode != 2 or "(deny-find)" not in done.stderr:
        failures.append("skipping find-chains also disabled plain find")
    done = run("find-chains", "fd-rg", {"ORCH_TOOL_GUARD_SKIP": "find-chains"})
    if done.returncode != 0:
        failures.append("skipping find-chains did not disable it")
    done = run("find", "fd-rg", {"ORCH_TOOL_GUARD_SKIP": "find"})
    if done.returncode != 0:
        failures.append("skipping find did not disable plain find")
    done = run("cat-head-tail", "fd-rg", {"ORCH_TOOL_GUARD_SKIP": "deny-cat-head-tail"})
    if done.returncode != 0:
        failures.append("skipping deny-cat-head-tail by full name did not work")
    return failures


def check_non_bash(checker):
    """A non-Bash event, a missing command and broken JSON must all allow."""
    failures = []
    payloads = [
        ("non-Bash tool", {"tool_name": "Read", "tool_input": {"command": "grep -rn x src"}}),
        ("missing command", {"tool_name": "Bash", "tool_input": {}}),
        ("empty command", {"tool_name": "Bash", "tool_input": {"command": "  "}}),
    ]
    for script in SCRIPTS:
        for name, payload in payloads:
            done = subprocess.run(
                [os.path.join(ROOT, script)],
                input=json.dumps(payload),
                text=True,
                capture_output=True,
                env=checker.env("fd-rg"),
            )
            if done.returncode != 0 or done.stderr.strip():
                failures.append(f"{script}: {name} did not allow cleanly: {done.stderr.strip()}")
        done = subprocess.run(
            [os.path.join(ROOT, script)],
            input="not json",
            text=True,
            capture_output=True,
            env=checker.env("fd-rg"),
        )
        if done.returncode != 0 or done.stderr.strip():
            failures.append(f"{script}: broken stdin did not allow cleanly: {done.stderr.strip()}")
    return failures


def main():
    with open(CASES, encoding="utf-8") as handle:
        data = json.load(handle)
    cases = data["cases"]
    situations = data["pathSituations"]

    workdir = tempfile.mkdtemp(prefix="toolguard-")
    try:
        support = os.path.join(workdir, "support")
        support_bin(support)
        paths = {}
        for situation, spec in situations.items():
            stubs(os.path.join(workdir, situation), spec["tools"])
            paths[situation] = os.path.join(workdir, situation) + os.pathsep + support

        checker = Checker(cases, situations, paths)
        failures = []
        failures.extend(shell_checks())
        failures.extend(required_cases(cases))
        checker.test()
        failures.extend(checker.failures)
        failures.extend(check_switches(checker))
        failures.extend(check_non_bash(checker))
        read_failures, read_blocked, read_allowed = check_big_read(checker, workdir, data["readCases"])
        failures.extend(read_failures)
        failures.extend(check_generic_reasons(checker))
    finally:
        shutil.rmtree(workdir, ignore_errors=True)

    for failure in failures:
        print(f"FAIL {failure}", file=sys.stderr)
    print(
        f"{len(cases)} cases: {checker.blocked} blocked, {checker.allowed} allowed; "
        f"{read_blocked + read_allowed} read cases: {read_blocked} blocked, {read_allowed} allowed; "
        f"{len(SCRIPTS)} scripts checked with bash -n"
        + (" and shellcheck" if shutil.which("shellcheck") else "")
        + f"; {len(failures)} failure(s)"
    )
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
