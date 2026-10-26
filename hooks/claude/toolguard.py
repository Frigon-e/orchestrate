#!/usr/bin/env python3
"""Shared guard for the Claude Code deny hooks in this folder.

This is a line for line port of the five rules in
tool/pi_ctx_reminder.ts, which stays the source of truth:
deny-find-chains, deny-find, deny-sed-inplace, deny-cat-head-tail and
deny-grep-rg. `deny-find-chains.sh` owns the first two, because a plain find is
a file search and a find chain is its sharper form; the other three scripts own
one rule each.

`deny-builtin-search.sh` is the sixth script and carries the two rules for
Claude's own Grep and Glob tools: deny-builtin-grep and deny-builtin-glob. They
reach the tree without a shell, so while `orch` answers they are refused like
the shell rules and point at the same ctx query; with no usable `orch` they are
the fallback. `orch ctx` is pinned to a git repository root, so a search whose
path is outside the session's repository has no replacement and is allowed.

`deny-big-read.sh` is the seventh script and carries one rule for Claude's own
Read tool: deny-big-read blocks a whole-file read of a regular file over
BIG_READ_BYTES, because every token read stays in context for the rest of the
session. A read with an offset or a limit always passes.

Each script passes its group name as the only argument. The hook JSON arrives
on stdin, and a block is exit 2 with the reason on stderr, which is how Claude
Code's PreToolUse hooks hand a refusal back to the model.

The replacement text names a tool in the same order as pi: `orch ctx` first
when `orch` answers `--version`, then fd for a file search and rg for a content
search, then the built-in Claude tools. An `orch` that is on PATH but cannot
answer is treated as absent, and a search block says it is broken.
"""

import json
import os
import re
import stat
import subprocess
import sys
import time

GUARD_MARKER = "orch tool guard"

# `orch --version` prints a fingerprint and starts nothing else, so a healthy
# wrapper answers at once and one that cannot find its tool directory fails at
# once. A second is plenty; three leaves room for a cold binary cache.
PROBE_TIMEOUT = 3

# The first line of a probe error is cut at this many characters so it fits in
# one line of a block.
DETAIL_MAX = 120

# Every hook call is a fresh process, so a probe would run on every Bash call.
# The result lives in a small cache for a minute instead.
HEALTH_TTL = 60
HEALTH_FILE = "toolguard-health.json"

# A whole-file Read of a regular file over this many bytes is blocked; a range
# (offset or limit) always passes.
BIG_READ_BYTES = 30720

# Files Claude Code's Read tool opens specially, so a size limit does not apply.
READ_EXEMPT_SUFFIXES = frozenset({".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf", ".ipynb"})

# The rules that name a search replacement, and so say when orch is broken. The
# two rules that always name a built-in tool (Edit, Read) never do, because
# orch does not back them and an edit or a read still works.
SEARCH_RULES = frozenset({"find-chains", "find", "grep-rg"})

# The rule order matters: a find chain is the sharper name for a plain find.
SHELL_SEARCHES = frozenset({"rg", "grep", "egrep", "fgrep", "ag", "fd", "find"})

SUDO_VALUE = re.compile(r"^-[ugpCDhTr]$")
VAR_PREFIX = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
SUDO_FLAG = re.compile(r"^-[A-Za-z]")
IN_PLACE = re.compile(r"^-[A-Za-z]*i")
FOLLOW = re.compile(r"^-[A-Za-z]*[fF]")
RECURSIVE = re.compile(r"^-[A-Za-z]*[rR]")
SED_PRINT = re.compile(r"^[\d$,; ]*p$")

FIND_CHAIN = frozenset({"-exec", "-execdir", "-ok", "-okdir", "-delete"})
SEARCH_SOURCE = frozenset({"find", "fd", "rg", "ag"})
SEARCH_SINK = frozenset({"grep", "rg", "ag"})
PLAIN_SEARCH = frozenset({"grep", "egrep", "fgrep", "rg", "ag"})
RECURSIVE_BY_DEFAULT = frozenset({"rg", "ag"})
GREP_VALUE = frozenset(
    {
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
    }
)


def on_path(name):
    """True when an executable of this name is on PATH."""
    for directory in (os.environ.get("PATH") or "").split(os.pathsep):
        if not directory:
            continue
        path = os.path.join(directory, name)
        try:
            if os.path.isfile(path) and os.access(path, os.X_OK):
                return True
        except OSError:
            continue
    return False


def installed():
    """The replacements a block may name, computed once per hook run.

    `orch` counts only when it answers `--version`; `broken` carries the probe's
    first error line so a block can say why it stopped naming it.
    """
    present = on_path("orch")
    healthy = False
    broken = ""
    if present:
        health = orch_health()
        healthy = health["ok"]
        if not healthy:
            broken = health["detail"]
    return {"orch": healthy, "fd": on_path("fd"), "rg": on_path("rg"), "broken": broken}


def first_line(text):
    """The first non-empty line of a probe's error, cut at DETAIL_MAX."""
    for line in str(text).splitlines():
        line = line.strip()
        if line:
            return line[:DETAIL_MAX]
    return ""


def probe_orch():
    """Run `orch --version` once and report whether it answered.

    A wrapper on PATH that fails is not a replacement, and a block that points
    at it teaches the model to go around the guard instead.
    """
    try:
        done = subprocess.run(
            ["orch", "--version"],
            capture_output=True,
            text=True,
            timeout=PROBE_TIMEOUT,
        )
    except (OSError, subprocess.SubprocessError) as error:
        return {"ok": False, "detail": first_line(error) or "probe failed"}
    if done.returncode == 0:
        return {"ok": True, "detail": ""}
    detail = (
        first_line(done.stderr)
        or first_line(done.stdout)
        or f"orch --version exited {done.returncode}"
    )
    return {"ok": False, "detail": detail}


def health_path():
    """The probe cache, under ORCH_TOOL_GUARD_STATE_DIR when it is set."""
    directory = os.environ.get("ORCH_TOOL_GUARD_STATE_DIR")
    if not directory:
        directory = os.path.join(os.path.expanduser("~"), ".local", "state", "orchestrate")
    return os.path.join(directory, HEALTH_FILE)


def read_health(path):
    """The cached probe when it is fresh and well formed, else None."""
    try:
        with open(path, encoding="utf-8") as handle:
            cached = json.load(handle)
    except (OSError, ValueError):
        return None
    if not isinstance(cached, dict) or not isinstance(cached.get("ok"), bool):
        return None
    try:
        age = time.time() - float(cached.get("ts", 0))
    except (TypeError, ValueError):
        return None
    if not 0 <= age < HEALTH_TTL:
        return None
    return {"ok": cached["ok"], "detail": str(cached.get("detail") or "")[:DETAIL_MAX]}


def write_health(path, health):
    """Best effort: a guard never fails a call because it could not cache."""
    try:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as handle:
            json.dump({"ts": time.time(), "ok": health["ok"], "detail": health["detail"]}, handle)
    except OSError:
        pass


def orch_health():
    """The probe result, cached for HEALTH_TTL seconds across hook calls."""
    path = health_path()
    cached = read_health(path)
    if cached is not None:
        return cached
    health = probe_orch()
    write_health(path, health)
    return health


def broken_line(detail):
    """The one line a search block adds when orch is installed but broken."""
    return (
        f"orch ctx is installed but broken here ({detail}); "
        "use the tools above and tell the user."
    )


def segments(command):
    """Split a command line on unquoted separators, keeping quote awareness.

    A word records whether it came from a quote, so a quoted word is never a
    command. A segment records a pipe on either side and a redirect, because a
    pipeline formats or forwards input and a redirect writes a file.
    """
    out = []
    words = []
    text = ""
    started = False
    quoted = False
    quote = ""
    escaped = False
    stdin_pipe = False
    redirect = False

    def end_word():
        nonlocal text, started, quoted
        if not started:
            return
        words.append((text, quoted))
        text = ""
        started = False
        quoted = False

    def end_segment(pipe_out):
        nonlocal words, stdin_pipe, redirect
        end_word()
        if words:
            out.append(
                {
                    "words": words,
                    "stdin": stdin_pipe,
                    "stdout": pipe_out,
                    "redirect": redirect,
                }
            )
        words = []
        stdin_pipe = pipe_out
        redirect = False

    for ch in command:
        if quote:
            started = True
            quoted = True
            if escaped:
                escaped = False
                text += ch
            elif quote == '"' and ch == "\\":
                escaped = True
            elif ch == quote:
                quote = ""
            else:
                text += ch
            continue
        if ch in "'\"":
            quote = ch
            started = True
            quoted = True
            continue
        if ch in "><":
            end_word()
            redirect = True
            continue
        if ch == "|":
            end_segment(True)
            continue
        if ch in ";&\n\r()":
            end_segment(False)
            continue
        if ch in " \t":
            end_word()
            continue
        text += ch
        started = True
    end_segment(False)
    return out


def base(word):
    return word.replace("\\", "").split("/")[-1]


def command(segment):
    """The command a segment runs, with its arguments.

    Leading VAR=value prefixes, `sudo` and a path to the tool do not hide the
    command from a rule.
    """
    words = segment["words"]
    at = 0
    total = len(words)
    while at < total and not words[at][1] and VAR_PREFIX.match(words[at][0]):
        at += 1
    while at < total and not words[at][1] and base(words[at][0]) == "sudo":
        at += 1
        while at < total and not words[at][1] and SUDO_FLAG.match(words[at][0]):
            flag = words[at][0]
            at += 1
            if SUDO_VALUE.match(flag):
                at += 1
        while at < total and not words[at][1] and VAR_PREFIX.match(words[at][0]):
            at += 1
    if at >= total:
        return "", []
    first = words[at]
    if first[1]:
        return "", []
    return base(first[0]), [word[0] for word in words[at + 1 :]]


def in_place_edit(args):
    return any(arg.startswith("--in-place") or IN_PLACE.match(arg) for arg in args)


def deny_find_chains(line):
    """find running a command, or deleting, and a search tool feeding a grep."""
    listing = segments(line)
    for segment in listing:
        name, args = command(segment)
        if name == "find" and any(arg in FIND_CHAIN for arg in args):
            return True
    for at, segment in enumerate(listing):
        if not segment["stdout"] or command(segment)[0] not in SEARCH_SOURCE:
            continue
        nxt = at + 1
        while nxt < len(listing) and listing[nxt]["stdin"]:
            name, args = command(listing[nxt])
            if name == "xargs":
                sink = any(base(arg) in SEARCH_SINK or arg == "find" for arg in args)
            else:
                sink = name in SEARCH_SINK
            if sink:
                return True
            if not listing[nxt]["stdout"]:
                break
            nxt += 1
    return False


def deny_find(line):
    """find over a tree is a file search; a find reading a pipe formats stdin."""
    return any(not segment["stdin"] and command(segment)[0] == "find" for segment in segments(line))


def deny_sed_inplace(line):
    for segment in segments(line):
        name, args = command(segment)
        if name in ("sed", "gsed") and in_place_edit(args):
            return True
        if name == "perl" and any(IN_PLACE.match(arg) for arg in args):
            return True
    return False


def plain_files(args):
    """cat takes no counted flags, so every other word names a file."""
    return [arg for arg in args if arg != "-" and not arg.startswith("-")]


def count_files(args):
    out = []
    at = 0
    while at < len(args):
        arg = args[at]
        if arg in ("-n", "-c", "-s", "--lines", "--bytes"):
            at += 2
            continue
        if arg.startswith("-") or arg == "-":
            at += 1
            continue
        out.append(arg)
        at += 1
    return out


def sed_files(args):
    """The file words of a sed, which follow its script."""
    out = []
    scripted = False
    at = 0
    while at < len(args):
        arg = args[at]
        if arg.startswith("-") and arg != "-":
            if arg in ("-e", "-f", "--expression", "--file"):
                scripted = True
                at += 1
            at += 1
            continue
        if not scripted:
            scripted = True
            at += 1
            continue
        out.append(arg)
        at += 1
    return out


def sed_prints(args):
    if any(arg == "-n" or re.match(r"^-[A-Za-z]*n$", arg) for arg in args):
        return True
    return any(SED_PRINT.match(arg) for arg in args)


def search_targets(args):
    """The file and directory arguments of a grep, which follow its pattern."""
    out = []
    patterned = False
    at = 0
    while at < len(args):
        arg = args[at]
        if arg.startswith("-") and arg != "-":
            if arg in GREP_VALUE:
                at += 1
            at += 1
            continue
        if not patterned:
            patterned = True
            at += 1
            continue
        out.append(arg)
        at += 1
    return out


def repository_root(start=None):
    """The root of the git repository the hook runs in, or "" outside one.

    orch ctx is pinned to a git repository root, so a path outside it has no ctx
    answer. A worktree root has a .git file rather than a directory, so any entry
    counts. Resolve links so aliases share the same repository boundary.
    """
    directory = os.path.abspath(start or os.getcwd())
    while True:
        if os.path.exists(os.path.join(directory, ".git")):
            return os.path.realpath(directory)
        parent = os.path.dirname(directory)
        if parent == directory:
            return ""
        directory = parent


def inside_repository(path):
    """True when a built-in search's target path is inside the repository.

    Resolve relative and missing paths from the session directory. Searches
    outside its git repository have no orch ctx replacement.
    """
    root = repository_root()
    if not root:
        return False
    resolved = os.path.realpath((path or "").strip() or ".")
    return resolved == root or resolved.startswith(root.rstrip(os.sep) + os.sep)


def grep_searches(segment, tools):
    """One segment is a raw content search over files."""
    name, args = command(segment)
    if not name:
        return False
    if name == "git":
        return bool(args) and args[0] == "grep" and not segment["stdin"]
    if name == "rg" and not tools["orch"]:
        return False
    if name not in PLAIN_SEARCH:
        return False
    if search_targets(args):
        return True
    recurses = name in RECURSIVE_BY_DEFAULT or any(
        arg.startswith("--recursive") or RECURSIVE.match(arg) for arg in args
    )
    return recurses and not segment["stdin"]


def deny_grep_rg(line, tools):
    """A grep with a file or directory argument, or one that recurses, searches
    files. One reading stdin is a filter. With no orch, rg is the preferred grep
    rather than a raw search, so rg itself stays allowed."""
    return any(grep_searches(segment, tools) for segment in segments(line))


def deny_cat_head_tail(line):
    for segment in segments(line):
        if segment["stdin"] or segment["stdout"] or segment["redirect"]:
            continue
        name, args = command(segment)
        if name == "cat":
            if len(plain_files(args)) == 1:
                return True
            continue
        if name in ("head", "tail"):
            if any(FOLLOW.match(arg) or arg.startswith("--follow") for arg in args):
                continue
            if len(count_files(args)) == 1:
                return True
            continue
        if name == "sed":
            if in_place_edit(args):
                continue
            if sed_prints(args) and len(sed_files(args)) == 1:
                return True
    return False


def file_search_reason(tools, with_symbol, none_text):
    """The line a find rule names: orch ctx, then fd, then the built-in Glob."""
    if tools["orch"]:
        queries = 'orch ctx batch -q "find:Code that ..."'
        if with_symbol:
            queries += ' -q "symbol:Name refs=3"'
        queries += ' -q "text:pattern glob=dir/**"'
        return f"One orch ctx batch replaces it: {queries} (--compact shortens it)."
    if tools["fd"]:
        return "Use fd instead (e.g. fd -e ts pattern dir, and fd -x cmd replaces find -exec)."
    return none_text


def rules():
    return [
        {
            "script": "find-chains",
            "name": "deny-find-chains",
            "id": "find-chains",
            "match": lambda line, tools: deny_find_chains(line),
            "reason": lambda tools: (
                f"{GUARD_MARKER} (deny-find-chains): find -exec, -delete and find | grep chains are blocked.\n"
                + file_search_reason(
                    tools,
                    True,
                    "Use the built-in Glob and Grep tools; orch ctx and fd are not installed.",
                )
            ),
        },
        {
            "script": "find-chains",
            "name": "deny-find",
            "id": "find",
            "match": lambda line, tools: deny_find(line),
            "reason": lambda tools: (
                f"{GUARD_MARKER} (deny-find): find over a tree is blocked.\n"
                + file_search_reason(tools, False, "Use the built-in Glob tool; orch ctx and fd are not installed.")
            ),
        },
        {
            "script": "sed-inplace",
            "name": "deny-sed-inplace",
            "id": "sed-inplace",
            "match": lambda line, tools: deny_sed_inplace(line),
            "reason": lambda tools: (
                f"{GUARD_MARKER} (deny-sed-inplace): sed -i and perl -pi rewrite a file in place.\n"
                "Use the built-in Edit tool; orch ctx is read-only and never replaces an edit."
            ),
        },
        {
            "script": "grep-rg",
            "name": "deny-grep-rg",
            "id": "grep-rg",
            "match": deny_grep_rg,
            "reason": lambda tools: (
                f"{GUARD_MARKER} (deny-grep-rg): a plain grep"
                + (", rg" if tools["orch"] else "")
                + ", ag or git grep over files is blocked.\n"
                + (
                    'One orch ctx batch replaces it: orch ctx batch -q "text:pattern glob=dir/**" '
                    '-q "symbol:Name refs=3" (a concept: find:...).'
                    if tools["orch"]
                    else "Use rg instead (e.g. rg -n pattern dir)."
                    if tools["rg"]
                    else "Use the built-in Grep tool; orch ctx and rg are not installed."
                )
            ),
        },
        {
            "script": "cat-head-tail",
            "name": "deny-cat-head-tail",
            "id": "cat-head-tail",
            "match": lambda line, tools: deny_cat_head_tail(line),
            "reason": lambda tools: (
                f"{GUARD_MARKER} (deny-cat-head-tail): a bare cat, head, tail or sed -n floods the context.\n"
                "Use the built-in Read tool with offset and limit (a section or the whole file)."
            ),
        },
    ]


# The built-in tools reach the tree without a shell, so their rules match a tool
# name and the arguments it carries rather than a command line.
def builtin_rules():
    """The two rules for Claude's own search tools, which a shell rule alone
    leaves as the way around the guard."""
    return [
        {
            "script": "builtin-search",
            "name": "deny-builtin-grep",
            "id": "builtin-grep",
            "tool": "Grep",
            "reason": (
                f"{GUARD_MARKER} (deny-builtin-grep): the built-in Grep tool is blocked; "
                "orch ctx answers searches here.\n"
                'One orch ctx batch replaces it: orch ctx batch -q "text:pattern glob=dir/**" '
                '-q "symbol:Name refs=3" (a concept: find:...).'
            ),
        },
        {
            "script": "builtin-search",
            "name": "deny-builtin-glob",
            "id": "builtin-glob",
            "tool": "Glob",
            "reason": (
                f"{GUARD_MARKER} (deny-builtin-glob): the built-in Glob tool is blocked; "
                "orch ctx answers file searches here.\n"
                'One orch ctx batch replaces it: orch ctx batch -q "find:Code that ..." '
                '-q "text:pattern glob=dir/**" (--compact shortens it).'
            ),
        },
    ]


def active_builtin_rules():
    """ORCH_TOOL_GUARD=off disables every rule; ORCH_TOOL_GUARD_SKIP takes the
    group name `builtin-search`, or one rule's short or full name."""
    if os.environ.get("ORCH_TOOL_GUARD") == "off":
        return []
    skip = {name.strip() for name in (os.environ.get("ORCH_TOOL_GUARD_SKIP") or "").split(",") if name.strip()}
    return [
        rule
        for rule in builtin_rules()
        if rule["id"] not in skip and rule["name"] not in skip and rule["script"] not in skip
    ]


def builtin_search(event):
    """A Grep or Glob call, decided against the same policy as the pi guard.

    Fail open: a payload without a tool name, a tool input or a pattern is not a
    search this guard can name a replacement for.
    """
    active = active_builtin_rules()
    if not active:
        return 0
    tool = event.get("tool_name")
    tool_input = event.get("tool_input")
    if not isinstance(tool_input, dict):
        return 0
    pattern = tool_input.get("pattern")
    if not isinstance(pattern, str) or not pattern.strip():
        return 0
    rule = next((rule for rule in active if rule["tool"] == tool), None)
    if rule is None:
        return 0
    path = tool_input.get("path")
    if not inside_repository(path if isinstance(path, str) else ""):
        return 0
    tools = installed()
    if not tools["orch"]:
        return 0
    sys.stderr.write(rule["reason"] + "\n")
    return 2


def big_read_reason(path, size, tools):
    """The block text: the size, then how to read only the part needed."""
    kilobytes = max(1, round(size / 1024))
    if tools["orch"]:
        lookup = (
            'Get the line numbers first: orch ctx text "pattern" --glob <file> or orch ctx sym Name '
            '(a doc: its headings with orch ctx text "^#{1,3} " --glob <file>).'
        )
    else:
        lookup = 'Get the line numbers first: rg -n "pattern" <file> (a doc: rg -n "^#{1,3} " <file>).'
    return (
        f"{GUARD_MARKER} (deny-big-read): {path} is {kilobytes} KB; reading it whole floods the context.\n"
        "Use Read with offset and limit and take only the section you need.\n" + lookup
    )


def big_read_skipped():
    """ORCH_TOOL_GUARD=off disables the rule; ORCH_TOOL_GUARD_SKIP takes its short
    or full name."""
    if os.environ.get("ORCH_TOOL_GUARD") == "off":
        return True
    skip = {name.strip() for name in (os.environ.get("ORCH_TOOL_GUARD_SKIP") or "").split(",") if name.strip()}
    return "big-read" in skip or "deny-big-read" in skip


def big_read(event):
    """A Read of a big file with no range, so the whole file lands in context.

    Fail open: a payload that is not a Read of a path, a range of any kind, a
    PDF page selection, a file Read opens specially, and a path that is not a
    regular file over the limit (missing, a directory, unreadable) all pass.
    """
    if big_read_skipped() or event.get("tool_name") != "Read":
        return 0
    tool_input = event.get("tool_input")
    if not isinstance(tool_input, dict):
        return 0
    path = tool_input.get("file_path")
    if not isinstance(path, str) or not path.strip():
        return 0
    if any(tool_input.get(key) not in (None, "") for key in ("offset", "limit", "pages")):
        return 0
    if os.path.splitext(path)[1].lower() in READ_EXEMPT_SUFFIXES:
        return 0
    try:
        info = os.stat(os.path.expanduser(path))
    except (OSError, ValueError):
        return 0
    if not stat.S_ISREG(info.st_mode) or info.st_size <= BIG_READ_BYTES:
        return 0
    sys.stderr.write(big_read_reason(path, info.st_size, installed()) + "\n")
    return 2


def active_rules(script):
    """ORCH_TOOL_GUARD=off disables every rule; ORCH_TOOL_GUARD_SKIP disables
    single rules by short name or by full name."""
    if os.environ.get("ORCH_TOOL_GUARD") == "off":
        return []
    skip = {name.strip() for name in (os.environ.get("ORCH_TOOL_GUARD_SKIP") or "").split(",") if name.strip()}
    return [
        rule
        for rule in rules()
        if rule["script"] == script and rule["id"] not in skip and rule["name"] not in skip
    ]


def main(argv):
    script = argv[1] if len(argv) > 1 else ""
    try:
        event = json.load(sys.stdin)
    except (json.JSONDecodeError, UnicodeDecodeError):
        return 0
    if not isinstance(event, dict):
        return 0
    if script == "builtin-search":
        return builtin_search(event)
    if script == "big-read":
        return big_read(event)
    tool = event.get("tool_name")
    if tool is not None and tool != "Bash":
        return 0
    tool_input = event.get("tool_input")
    if not isinstance(tool_input, dict):
        return 0
    raw = tool_input.get("command")
    if not isinstance(raw, str):
        return 0
    line = raw.strip()
    if not line:
        return 0
    active = active_rules(script)
    if not active:
        return 0
    tools = installed()
    for rule in active:
        if rule["match"](line, tools):
            reason = rule["reason"](tools)
            if tools["broken"] and rule["id"] in SEARCH_RULES:
                reason += "\n" + broken_line(tools["broken"])
            sys.stderr.write(reason + "\n")
            return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
