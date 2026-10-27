#!/usr/bin/env python3
"""Shared guard for the Claude Code deny hooks in this folder.

This is a line for line port of the five rules in
tool/pi_ctx_reminder.ts, which stays the source of truth:
deny-find-chains, deny-find, deny-sed-inplace, deny-cat-head-tail and
deny-grep-rg. `deny-find-chains.sh` owns the first two, because a plain find is
a file search and a find chain is its sharper form; the other three scripts own
one rule each.

`deny-big-read.sh` is the sixth script and carries one rule for Claude's own
Read tool: deny-big-read blocks a whole-file read of a regular file over
BIG_READ_BYTES, because every token read stays in context for the rest of the
session. A read with an offset or a limit always passes.

Each script passes its group name as the only argument. The hook JSON arrives
on stdin, and a block is exit 2 with the reason on stderr, which is how Claude
Code's PreToolUse hooks hand a refusal back to the model.

A block names rg for a content search and fd for a file search, then the lookup
ladder. A grep is blocked only while rg is on PATH, because with no rg there is
no replacement to name.
"""

import datetime
import json
import os
import re
import stat
import sys

GUARD_MARKER = "orch tool guard"

# A whole-file Read of a regular file over this many bytes is blocked; a range
# (offset or limit) always passes.
BIG_READ_BYTES = 30720

# Files Claude Code's Read tool opens specially, so a size limit does not apply.
READ_EXEMPT_SUFFIXES = frozenset({".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf", ".ipynb"})

# One line after the first line of a search block: where to look, in order.
LADDER = (
    'Lookup order: jbcontext search "<one behavior>" if the location is unknown, '
    "orch lsp def|refs|callers <Name> for a known symbol, rg for exact text, "
    "fd for file names; send independent lookups in one message."
)

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
PIPE_SINK = frozenset({"grep", "ag"})
XARGS_SINK = frozenset({"grep", "rg", "ag"})
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
    """The replacements a block may name, computed once per hook run."""
    return {"fd": on_path("fd"), "rg": on_path("rg")}


def segments(command, dq_raw=False):
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
                if dq_raw and ch not in '"\\$`':
                    text += "\\"  # bash keeps the backslash before any other character
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
                sink = any(base(arg) in XARGS_SINK or arg == "find" for arg in args)
            else:
                sink = name in PIPE_SINK
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


def grep_searches(segment, tools):
    """One segment is a raw content search over files."""
    name, args = command(segment)
    if not name:
        return False
    if not tools["rg"]:
        return False
    if name == "git":
        return bool(args) and args[0] == "grep" and not segment["stdin"]
    if name == "rg" or name not in PLAIN_SEARCH:
        return False
    if search_targets(args):
        return True
    recurses = name in RECURSIVE_BY_DEFAULT or any(
        arg.startswith("--recursive") or RECURSIVE.match(arg) for arg in args
    )
    return recurses and not segment["stdin"]


def deny_grep_rg(line, tools):
    """A grep with a file or directory argument, or one that recurses, searches
    files. One reading stdin is a filter. rg is the replacement, so it is never
    blocked, and with no rg on PATH there is nothing to name, so nothing is."""
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
        if name == "sed":
            if in_place_edit(args):
                continue
            if sed_prints(args) and len(sed_files(args)) == 1:
                return True
    return False


def find_reason(tools, name, first, fallback):
    """A find rule: fd when it is installed, else the built-in tool."""
    if tools["fd"]:
        return f"{GUARD_MARKER} ({name}): {first}\n{LADDER}"
    return f"{GUARD_MARKER} ({name}): {fallback}"


def rules():
    return [
        {
            "script": "find-chains",
            "name": "deny-find-chains",
            "id": "find-chains",
            "match": lambda line, tools: deny_find_chains(line),
            "reason": lambda tools: find_reason(
                tools,
                "deny-find-chains",
                "find -exec, -delete and find | grep chains are blocked; use fd, or rg -g 'GLOB' PATTERN for content.",
                "find -exec, -delete and find | grep chains are blocked; run a plain find and act on its output, or rg for content (fd is not installed).",
            ),
        },
        {
            "script": "find-chains",
            "name": "deny-find",
            "id": "find",
            "match": lambda line, tools: tools["fd"] and deny_find(line),
            "reason": lambda tools: find_reason(
                tools,
                "deny-find",
                "find over a tree is blocked; use fd (fd PATTERN dir, fd -e go, fd -g '*.ts').",
                "find over a tree is blocked; use fd.",
            ),
        },
        {
            "script": "sed-inplace",
            "name": "deny-sed-inplace",
            "id": "sed-inplace",
            "match": lambda line, tools: deny_sed_inplace(line),
            "reason": lambda tools: (
                f"{GUARD_MARKER} (deny-sed-inplace): sed -i and perl -pi rewrite a file in place.\n"
                "Use the built-in Edit tool."
            ),
        },
        {
            "script": "grep-rg",
            "name": "deny-grep-rg",
            "id": "grep-rg",
            "match": deny_grep_rg,
            "reason": lambda tools: (
                f"{GUARD_MARKER} (deny-grep-rg): grep, egrep, fgrep, ag and git grep over files are blocked; "
                "use rg (rg -n PATTERN path, -g 'GLOB', -F literal, -w word, -i).\n" + LADDER
            ),
        },
        {
            "script": "cat-head-tail",
            "name": "deny-cat-head-tail",
            "id": "cat-head-tail",
            "match": lambda line, tools: deny_cat_head_tail(line),
            "reason": lambda tools: (
                f"{GUARD_MARKER} (deny-cat-head-tail): a bare cat or sed -n over one file floods the context.\n"
                "Use the built-in Read tool with offset and limit (a section or the whole file)."
            ),
        },
    ]


def big_read_reason(path, size):
    """The block text: the size, then how to read only the part needed."""
    kilobytes = max(1, round(size / 1024))
    return (
        f"{GUARD_MARKER} (deny-big-read): {path} is {kilobytes} KB; reading it whole floods the context.\n"
        "Use Read with offset and limit and take only the section you need.\n"
        'Get the line numbers first: rg -n PATTERN file (a doc: rg -n "^#{1,3} " file).'
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
    sys.stderr.write(big_read_reason(path, info.st_size) + "\n")
    return 2


# ---------------------------------------------------------------------------
# Lookup coach: a non-blocking log of every lookup and four reminders. The pi
# side is tool/pi_ctx_reminder.ts; both read hooks/claude/coach_cases.json.
# ---------------------------------------------------------------------------

COACH_PREFIX = "orch lookup coach: "
COACH_TEXT = {
    "concept-first": (
        'Location unknown? Ask jbcontext first: jbcontext search "<one behavior>", '
        "one concept per query, several queries in one message."
    ),
    "symbol-via-lsp": "For a definition, references or callers use orch lsp def|refs|callers <Name>; rg finds text, not uses.",
    "batch-lookups": "Send independent lookups together as parallel calls in one message; each one alone costs a turn.",
    "read-batch": "Read the ranges you already know as parallel reads in one message.",
}
QUERY_MAX = 200
TRANSCRIPT_TAIL = 512 * 1024
NO_RESULT = "No relevant results found"
DOC_EXTENSIONS = frozenset({"md", "txt", "json", "yaml", "yml", "toml"})
DOC_TYPES = DOC_EXTENSIONS | {"markdown"}
RG_VALUE = GREP_VALUE | {"--iglob", "-T", "--type-not", "-r", "--replace", "-d", "--max-depth", "-M", "--max-columns"}
FD_VALUE = frozenset(
    {"-e", "-E", "-t", "-d", "-x", "-X", "-S", "-c", "-j", "--extension", "--exclude", "--type", "--max-depth",
     "--size", "--changed-within", "--changed-before", "--owner", "--color", "--threads", "--base-directory"}
)
JB_VALUE = frozenset(
    {"--project-path", "--git-remote-url", "--limit", "--revision", "-p", "--path-filter", "--branch"}
)
LSP_VALUE = frozenset({"--limit"})
IDENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$")


def positionals(args, value_flags):
    """The words of an argument list that are neither flags nor flag values."""
    out = []
    at = 0
    while at < len(args):
        arg = args[at]
        if arg == "--":
            out.extend(args[at + 1 :])
            break
        if arg.startswith("-") and arg != "-":
            if arg in value_flags:
                at += 1
            at += 1
            continue
        out.append(arg)
        at += 1
    return out


def rg_parts(args):
    """The pattern, paths, globs, types and -w flag of an rg call."""
    pattern = None
    paths = []
    globs = []
    types = []
    word = False
    free = []
    at = 0
    while at < len(args):
        arg = args[at]
        nxt = args[at + 1] if at + 1 < len(args) else ""
        if arg == "--":
            free.extend(args[at + 1 :])
            break
        if arg.startswith("-") and arg != "-":
            if arg in ("-e", "--regexp"):
                pattern = nxt if pattern is None else pattern
                at += 2
                continue
            if arg in ("-g", "--glob", "--iglob"):
                globs.append(nxt)
            elif arg in ("-t", "--type"):
                types.append(nxt)
            elif arg.startswith(("--glob=", "--iglob=")):
                globs.append(arg.split("=", 1)[1])
            elif arg.startswith("--type="):
                types.append(arg.split("=", 1)[1])
            elif arg == "--word-regexp" or (re.match(r"^-[A-Za-z]+$", arg) and "w" in arg[1:]):
                word = True
            if arg in RG_VALUE:
                at += 1
            at += 1
            continue
        free.append(arg)
        at += 1
    if pattern is None and free:
        pattern, free = free[0], free[1:]
    paths = free
    return pattern or "", paths, globs, types, word


def doc_target(kind, value):
    value = value.strip()
    if kind == "type":
        return value in DOC_TYPES
    if "." not in value.rsplit("/", 1)[-1]:
        return False
    ext = value.rsplit(".", 1)[-1].strip("{}")
    return bool(ext) and all(part in DOC_EXTENSIONS for part in ext.split(","))


def symbol_rg(args):
    """An rg whose pattern names a symbol, so orch lsp would answer it better."""
    pattern, paths, globs, types, word = rg_parts(args)
    targets = [("glob", g) for g in globs] + [("type", t) for t in types] + [("path", p) for p in paths]
    if targets and all(doc_target(kind, value) for kind, value in targets):
        return False
    text = pattern.replace("\\b", "").replace("\\<", "").replace("\\>", "")
    text = re.sub(r"^(func|def|class|type|interface) ", "", text)
    text = re.sub(r"\\?\($", "", text)
    if not IDENT.match(text):
        return False
    return word or "." in text or "_" in text or bool(re.search(r"[A-Z]", text[1:]))


def lookup_shell(line):
    """(tool, query, symbol) for the first command of a line, or None."""
    listing = segments(line, True)
    if listing and command(listing[0])[0] == "cd":
        listing = listing[1:]
    if not listing:
        return None
    name, args = command(listing[0])
    if name == "jbcontext" and args and args[0] == "search":
        return "jbcontext", (positionals(args[1:], JB_VALUE) or [""])[0], False
    if name == "orch" and len(args) >= 2 and args[0] == "lsp" and not args[1].startswith("-"):
        return "lsp:" + args[1], " ".join(positionals(args[2:], LSP_VALUE)), False
    if name == "orch" and len(args) >= 2 and args[0] == "ctx" and not args[1].startswith("-"):
        return "ctx:" + args[1], " ".join(w for w in args[2:] if not w.startswith("-")), False
    if name == "rg":
        return "rg", rg_parts(args)[0], symbol_rg(args)
    if name == "fd":
        return "fd", (positionals(args, FD_VALUE) or [""])[0], False
    return None


def read_query(path, offset, limit):
    if offset in (None, "") and limit in (None, ""):
        return path
    return f"{path}:{'' if offset in (None, '') else offset}-{'' if limit in (None, '') else limit}"


def lookup_claude(event):
    """(tool, query, symbol) for a Claude tool call, or None when it is no lookup."""
    name = event.get("tool_name") or ""
    tool_input = event.get("tool_input")
    if not isinstance(tool_input, dict):
        return None
    if name == "Bash":
        line = tool_input.get("command")
        return lookup_shell(line) if isinstance(line, str) else None
    if name == "Read":
        path = tool_input.get("file_path")
        if not isinstance(path, str) or not path:
            return None
        return "read", read_query(path, tool_input.get("offset"), tool_input.get("limit")), False
    if name == "LSP":
        op = tool_input.get("operation")
        if not isinstance(op, str) or not op:
            return None
        where = f"{tool_input.get('filePath', '')}:{tool_input.get('line', '')}"
        return "lsp:" + op, where, False
    if name.startswith("mcp__jbcontext__"):
        return "jbcontext", str(tool_input.get("query") or tool_input.get("q") or ""), False
    return None


def coach_step(state, tool, symbol, turn, adjacent, reminders=True):
    """Advance the session state for one lookup; returns (reminder, followed)."""
    followed = None
    if reminders and state.get("pending"):
        kind = state["pending"]
        followed = tool == "jbcontext" if kind == "concept-first" else tool.startswith("lsp:")
        state["pending"] = None
    turns = state.setdefault("turns", [])
    new_turn = turn is not None and not (turns and turns[-1]["id"] == turn)
    candidates = []
    if tool in ("rg", "fd") and not state.get("first_search_done"):
        state["first_search_done"] = True
        if not state.get("concept_seen"):
            candidates.append("concept-first")
    if tool == "jbcontext" or tool.startswith("lsp:"):
        state["concept_seen"] = True
    if tool == "rg" and symbol:
        candidates.append("symbol-via-lsp")
    if new_turn and len(turns) >= 2 and adjacent(turns[-2]["id"], turns[-1]["id"], turn):
        a, b = turns[-2], turns[-1]
        if a["nr"] == 1 and b["nr"] == 1:
            candidates.append("batch-lookups")
        if a["rd"] == 1 and b["rd"] == 1 and a["nr"] == 0 and b["nr"] == 0:
            candidates.append("read-batch")
    reminder = None
    fired = state.setdefault("fired", [])
    if reminders:
        for kind in candidates:
            if kind not in fired:
                reminder = kind
                fired.append(kind)
                break
        if reminder in ("concept-first", "symbol-via-lsp"):
            state["pending"] = reminder
    if turn is not None:
        if new_turn:
            turns.append({"id": turn, "nr": 0, "rd": 0})
            del turns[:-3]
        turns[-1]["rd" if tool == "read" else "nr"] += 1
    return reminder, followed


def transcript_turn(path, tool_use_id):
    """(turn id, ordered distinct assistant message ids) from the transcript tail."""
    with open(path, "rb") as handle:
        handle.seek(0, os.SEEK_END)
        size = handle.tell()
        handle.seek(max(0, size - TRANSCRIPT_TAIL))
        data = handle.read()
    entries = []
    for raw in data.split(b"\n"):
        if b'"message"' not in raw:
            continue
        try:
            entry = json.loads(raw)
        except ValueError:
            continue
        message = entry.get("message") if isinstance(entry, dict) else None
        if isinstance(message, dict) and message.get("role", "assistant") == "assistant" and message.get("id"):
            entries.append((bool(entry.get("isSidechain")), message))
    match = None
    for side, message in entries:
        content = message.get("content")
        if isinstance(content, list) and any(
            isinstance(b, dict) and b.get("type") == "tool_use" and b.get("id") == tool_use_id for b in content
        ):
            match = (side, message["id"])
    if match is None:
        return None, []
    order = []
    for side, message in entries:
        if side == match[0] and message["id"] not in order:
            order.append(message["id"])
    return match[1], order


def state_dir():
    return os.environ.get("ORCH_TOOL_GUARD_STATE_DIR") or os.path.join(
        os.path.expanduser("~"), ".local", "state", "orchestrate"
    )


def repo_of(cwd):
    here = os.path.abspath(cwd or os.getcwd())
    probe = here
    while True:
        if os.path.exists(os.path.join(probe, ".git")):
            return probe
        parent = os.path.dirname(probe)
        if parent == probe:
            return here
        probe = parent


def result_text(event):
    response = event.get("tool_response")
    if isinstance(response, str):
        return response
    if isinstance(response, dict) and ("stdout" in response or "stderr" in response):
        return f"{response.get('stdout') or ''}{response.get('stderr') or ''}"
    if response is None:
        return str(event.get("error") or "")
    return json.dumps(response)


def outcome(event, tool, text):
    """(ok, empty) of a lookup; a failure event is an error unless it is a clean exit 1."""
    failed = event.get("hook_event_name") == "PostToolUseFailure"
    if failed:
        error = str(event.get("error") or "")
        body = re.sub(r"^Exit code 1\s*", "", error)
        if body != error and not body.strip() and (tool in ("rg", "fd") or tool.startswith("lsp:")):
            return True, True
        return False, False
    return True, tool == "jbcontext" and NO_RESULT in text


def coach(event):
    if os.environ.get("ORCH_TOOL_GUARD") == "off":
        return 0
    found = lookup_claude(event)
    if found is None:
        return 0
    tool, query, symbol = found
    session = str(event.get("session_id") or "unknown")
    base_dir = state_dir()
    os.makedirs(os.path.join(base_dir, "coach"), exist_ok=True)
    turn, order = None, []
    path = event.get("transcript_path")
    use_id = event.get("tool_use_id")
    if isinstance(path, str) and isinstance(use_id, str):
        try:
            turn, order = transcript_turn(path, use_id)
        except OSError:
            pass
    position = {mid: i for i, mid in enumerate(order)}

    def adjacent(a, b, c):
        return all(x in position for x in (a, b, c)) and position[b] == position[a] + 1 and position[c] == position[b] + 1

    coaching = os.environ.get("ORCH_LOOKUP_COACH") != "off"
    name = re.sub(r"[^A-Za-z0-9_.-]", "_", session)
    import fcntl

    with open(os.path.join(base_dir, "coach", name + ".json"), "a+") as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        handle.seek(0)
        try:
            state = json.loads(handle.read() or "{}")
        except ValueError:
            state = {}
        reminder, followed = coach_step(state, tool, symbol, turn, adjacent, coaching)
        handle.seek(0)
        handle.truncate()
        handle.write(json.dumps(state))
        if os.environ.get("ORCH_LOOKUP_LOG") != "off":
            text = result_text(event)
            ok, empty = outcome(event, tool, text)
            duration = event.get("duration_ms")
            line = {
                "ts": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ"),
                "harness": "claude",
                "session": session,
                "repo": repo_of(event.get("cwd")),
                "turn": turn,
                "tool": tool,
                "query": query[:QUERY_MAX],
                "ok": ok,
                "empty": empty,
                "bytes": len(text.encode("utf-8", "replace")),
                "duration_ms": duration if isinstance(duration, (int, float)) else None,
                "reminder": reminder,
                "followed": followed,
            }
            fd = os.open(os.path.join(base_dir, "lookups.jsonl"), os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o644)
            try:
                os.write(fd, (json.dumps(line) + "\n").encode("utf-8"))
            finally:
                os.close(fd)
    if reminder:
        out = {
            "hookSpecificOutput": {
                "hookEventName": event.get("hook_event_name") or "PostToolUse",
                "additionalContext": COACH_PREFIX + COACH_TEXT[reminder],
            }
        }
        sys.stdout.write(json.dumps(out))
    return 0


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
    if script == "big-read":
        return big_read(event)
    if script == "coach":
        try:
            return coach(event)
        except Exception:  # fail open: a coach bug must never disturb a session
            return 0
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
            sys.stderr.write(rule["reason"](tools) + "\n")
            return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
