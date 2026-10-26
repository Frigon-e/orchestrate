#!/usr/bin/env python3
"""Register the tool-guard hooks in a Claude Code settings.json.

The hooks to register are listed in settings-fragment.json next to this file.
Every hook whose command points into hooks/claude of this skill is owned by this
script; every other key and hook in the settings file is left exactly as found.

    install_settings.py                  show what would change (default, writes nothing)
    install_settings.py --apply          merge the fragment into the settings file
    install_settings.py --apply --remove remove the owned hooks again
    install_settings.py --check          exit 1 when an owned hook is missing
    install_settings.py --stdin          read settings JSON on stdin, write the result on stdout

A settings file that is not valid JSON is never touched (exit 2).
"""
import argparse
import json
import os
import re
import shutil
import sys
import tempfile
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
FRAGMENT = HERE / "settings-fragment.json"
OWNED_MARK = "/skills/orchestrate/hooks/claude/"
DEFAULT_SETTINGS = "~/.claude/settings.json"


def is_owned(hook):
    return OWNED_MARK in hook.get("command", "")


def commands(settings, event):
    return [h.get("command") for g in settings.get("hooks", {}).get(event, []) for h in g.get("hooks", [])]


def merge(settings, fragment):
    """Add every fragment hook that is missing. Returns the commands added."""
    added = []
    hooks = settings.setdefault("hooks", {})
    for event, groups in fragment["hooks"].items():
        have = set(commands(settings, event))
        for group in groups:
            for hook in group["hooks"]:
                if hook["command"] in have:
                    continue
                event_groups = hooks.setdefault(event, [])
                target = next(
                    (g for g in event_groups
                     if g.get("matcher") == group.get("matcher") and any(is_owned(h) for h in g.get("hooks", []))),
                    None,
                )
                if target is None:
                    target = {k: v for k, v in group.items() if k != "hooks"}
                    target["hooks"] = []
                    event_groups.append(target)
                target["hooks"].append(dict(hook))
                have.add(hook["command"])
                added.append(hook["command"])
    return added


def remove(settings):
    """Drop every owned hook and any group or event left empty. Returns the commands removed."""
    removed = []
    hooks = settings.get("hooks", {})
    for event in list(hooks):
        kept_groups = []
        for group in hooks[event]:
            kept = [h for h in group.get("hooks", []) if not is_owned(h)]
            removed += [h["command"] for h in group.get("hooks", []) if is_owned(h)]
            if kept:
                group["hooks"] = kept
                kept_groups.append(group)
        if kept_groups:
            hooks[event] = kept_groups
        else:
            del hooks[event]
    if "hooks" in settings and not settings["hooks"]:
        del settings["hooks"]
    return removed


def missing(settings, fragment):
    return [
        h["command"]
        for event, groups in fragment["hooks"].items()
        for g in groups
        for h in g["hooks"]
        if h["command"] not in commands(settings, event)
    ]


def parse(text, label):
    try:
        value = json.loads(text) if text.strip() else {}
    except json.JSONDecodeError as err:
        sys.exit(f"install_settings: {label} is not valid JSON ({err}); leaving it alone")
    if not isinstance(value, dict):
        sys.exit(f"install_settings: {label} is not a JSON object; leaving it alone")
    return value


def indent_of(text):
    match = re.search(r"^( +)\S", text, re.M)
    return len(match.group(1)) if match else 2


def render(settings, indent):
    return json.dumps(settings, indent=indent, ensure_ascii=False) + "\n"


def write_atomic(path, text):
    path = path.resolve()
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        shutil.copy2(path, f"{path}.bak-{time.strftime('%Y%m%d-%H%M%S')}")
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=path.name + ".")
    try:
        with os.fdopen(fd, "w") as handle:
            handle.write(text)
        if path.exists():
            shutil.copymode(path, tmp)
        os.replace(tmp, path)
    except BaseException:
        os.unlink(tmp)
        raise


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--settings", default=DEFAULT_SETTINGS, help="settings file (default %(default)s)")
    ap.add_argument("--apply", action="store_true", help="write the result")
    ap.add_argument("--remove", action="store_true", help="remove the owned hooks instead of adding them")
    ap.add_argument("--check", action="store_true", help="exit 1 when an owned hook is missing")
    ap.add_argument("--stdin", action="store_true", help="filter mode: settings on stdin, result on stdout")
    args = ap.parse_args(argv)

    fragment = json.loads(FRAGMENT.read_text())

    if args.stdin:
        text = sys.stdin.read()
        settings = parse(text, "stdin")
        remove(settings) if args.remove else merge(settings, fragment)
        sys.stdout.write(render(settings, indent_of(text)))
        return 0

    path = Path(args.settings).expanduser()
    text = path.read_text() if path.exists() else ""
    settings = parse(text, str(path))

    if args.check:
        gone = missing(settings, fragment)
        for command in gone:
            print(f"missing: {command}")
        return 1 if gone else 0

    changed = remove(settings) if args.remove else merge(settings, fragment)
    verb = "remove" if args.remove else "add"
    if not changed:
        print(f"nothing to {verb}: {path} is already up to date")
        return 0
    for command in changed:
        print(f"{verb}: {command}")
    if not args.apply:
        print("dry run; pass --apply to write")
        return 0
    write_atomic(path, render(settings, indent_of(text)))
    print(f"wrote {path.resolve()}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
