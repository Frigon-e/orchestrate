#!/usr/bin/env python3
"""Tests for install_settings.py. Run: python3 hooks/claude/install_settings_test.py"""
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import install_settings as inst  # noqa: E402  # pyright: ignore[reportMissingImports]

SCRIPT = HERE / "install_settings.py"
FRAGMENT = json.loads((HERE / "settings-fragment.json").read_text())
FRAGMENT_COMMANDS = [h["command"] for gs in FRAGMENT["hooks"].values() for g in gs for h in g["hooks"]]

# What other tools put in settings.json: must survive every operation untouched.
FOREIGN = {
    "model": "opus",
    "permissions": {"allow": ["Bash(ls:*)"]},
    "hooks": {
        "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "jq -r .x"}]}],
        "SessionStart": [{"hooks": [{"type": "command", "command": "/Users/x/.jbcontext/bin/jbcontext_binary start"}]}],
    },
}


def deep(value):
    return json.loads(json.dumps(value))


GUARD_PRE = '"$HOME/.agents/skills/orchestrate/hooks/bin/orch-guard" pre'
GUARD_COACH = '"$HOME/.agents/skills/orchestrate/hooks/bin/orch-guard" coach'
ORCH_HOOK = '"$HOME/.agents/skills/orchestrate/hooks/claude/orch-hook.sh" '
# The `orch hook` entries a hand install left in settings.json, before this script owned them.
LEGACY_ORCH_PRETOOL = ('[ "$ORCH_ROLE" = orchestrator ] || [ -e "$HOME/.local/state/orchestrate/roles/${HERDR_PANE_ID:-none}" ] '
                       '|| exit 0; exec "$HOME/.agents/skills/orchestrate/scripts/orch" hook pretool')
LEGACY_ORCH_HOOK = ('[ -n "$ORCH_ROLE" ] || [ -e "$HOME/.local/state/orchestrate/roles/${HERDR_PANE_ID:-none}" ] '
                    '|| exit 0; exec "$HOME/.agents/skills/orchestrate/scripts/orch" hook ')
# What an install of the shell-script era left in settings.json: all of it retired.
OLD_SCRIPTS = ["deny-find-chains", "deny-sed-inplace", "deny-cat-head-tail", "deny-grep-rg", "deny-big-read", "lookup-coach"]


def old_command(name):
    return f'"$HOME/.agents/skills/orchestrate/hooks/claude/{name}.sh"'


def run(*args, stdin=None, env=None):
    return subprocess.run([sys.executable, str(SCRIPT), *args], input=stdin, capture_output=True, text=True, env=env)


class MergeTests(unittest.TestCase):
    def test_empty_settings_gets_every_fragment_hook(self):
        settings = {}
        added = inst.merge(settings, FRAGMENT)
        self.assertEqual(sorted(added), sorted(FRAGMENT_COMMANDS))
        self.assertEqual(settings["hooks"], FRAGMENT["hooks"])

    def test_foreign_hooks_and_keys_are_untouched(self):
        settings = deep(FOREIGN)
        inst.merge(settings, FRAGMENT)
        self.assertEqual(settings["model"], "opus")
        self.assertEqual(settings["permissions"], FOREIGN["permissions"])
        self.assertEqual(settings["hooks"]["SessionStart"], FOREIGN["hooks"]["SessionStart"] + FRAGMENT["hooks"]["SessionStart"])
        self.assertEqual(settings["hooks"]["PreToolUse"][0], FOREIGN["hooks"]["PreToolUse"][0])

    def test_owned_hooks_get_their_own_group_even_when_matcher_matches_a_foreign_group(self):
        settings = deep(FOREIGN)
        inst.merge(settings, FRAGMENT)
        bash_groups = [g for g in settings["hooks"]["PreToolUse"] if g.get("matcher") == "Bash"]
        self.assertEqual(len(bash_groups), 2)
        self.assertEqual([h["command"] for h in bash_groups[0]["hooks"]], ["jq -r .x"])
        self.assertTrue(all(inst.is_owned(h) for h in bash_groups[1]["hooks"]))

    def test_second_merge_adds_nothing(self):
        settings = deep(FOREIGN)
        inst.merge(settings, FRAGMENT)
        once = deep(settings)
        self.assertEqual(inst.merge(settings, FRAGMENT), [])
        self.assertEqual(settings, once)

    def test_merge_completes_a_partial_install(self):
        settings = {"hooks": {"PreToolUse": [{"matcher": "Read", "hooks": [dict(
            FRAGMENT["hooks"]["PreToolUse"][1]["hooks"][0])]}]}}
        added = inst.merge(settings, FRAGMENT)
        self.assertEqual(len(added), len(FRAGMENT_COMMANDS) - 1)
        self.assertEqual(inst.missing(settings, FRAGMENT), [])

    def test_lookup_coach_is_registered_after_every_result_event(self):
        settings = {}
        inst.merge(settings, FRAGMENT)
        for event in ("PostToolUse", "PostToolUseFailure"):
            groups = settings["hooks"][event]
            self.assertEqual([g["matcher"] for g in groups], ["Bash|Read|LSP|mcp__jbcontext__.*"])
            self.assertEqual(groups[0]["hooks"][0]["command"], GUARD_COACH)

    def test_lookup_coach_is_registered_on_user_prompt_submit_without_a_matcher(self):
        settings = deep(FOREIGN)
        settings["hooks"]["UserPromptSubmit"] = [{"hooks": [{"type": "command", "command": "echo mine"}]}]
        inst.merge(settings, FRAGMENT)
        commands = inst.commands(settings, "UserPromptSubmit")
        self.assertEqual(commands[0], "echo mine")
        self.assertEqual(commands.count(GUARD_COACH), 1)
        self.assertEqual(inst.merge(settings, FRAGMENT), [])

    def test_timeout_is_kept(self):
        settings = {}
        inst.merge(settings, FRAGMENT)
        read_hook = [h for g in settings["hooks"]["PreToolUse"] if g["matcher"] == "Read" for h in g["hooks"]][0]
        self.assertEqual(read_hook["timeout"], 5)


class PruneTests(unittest.TestCase):
    RETIRED = '"$HOME/.agents/skills/orchestrate/hooks/claude/deny-builtin-search.sh"'

    def with_retired(self):
        settings = deep(FOREIGN)
        inst.merge(settings, FRAGMENT)
        settings["hooks"]["PreToolUse"].append(
            {"matcher": "Grep|Glob", "hooks": [{"type": "command", "command": self.RETIRED}]}
        )
        return settings

    def test_prune_removes_an_owned_hook_the_fragment_dropped(self):
        settings = self.with_retired()
        self.assertEqual(inst.prune(settings, FRAGMENT), [self.RETIRED])
        self.assertEqual(inst.missing(settings, FRAGMENT), [])
        self.assertNotIn("Grep|Glob", [g.get("matcher") for g in settings["hooks"]["PreToolUse"]])

    def test_prune_leaves_foreign_hooks_and_current_hooks(self):
        settings = deep(FOREIGN)
        inst.merge(settings, FRAGMENT)
        once = deep(settings)
        self.assertEqual(inst.prune(settings, FRAGMENT), [])
        self.assertEqual(settings, once)

    def test_apply_prunes_the_retired_hook(self):
        tmp = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(tmp, ignore_errors=True))
        path = tmp / "settings.json"
        path.write_text(json.dumps(self.with_retired(), indent=2) + "\n")
        result = run("--settings", str(path), "--apply")
        self.assertEqual(result.returncode, 0)
        self.assertIn(f"remove: {self.RETIRED}", result.stdout)
        self.assertNotIn(self.RETIRED, path.read_text())
        self.assertEqual(inst.missing(json.loads(path.read_text()), FRAGMENT), [])

    def test_stdin_mode_prunes_too(self):
        result = run("--stdin", stdin=json.dumps(self.with_retired()))
        self.assertNotIn(self.RETIRED, result.stdout)


class RemoveTests(unittest.TestCase):
    def test_remove_restores_foreign_settings_exactly(self):
        settings = deep(FOREIGN)
        inst.merge(settings, FRAGMENT)
        removed = inst.remove(settings)
        self.assertEqual(sorted(removed), sorted(FRAGMENT_COMMANDS))
        self.assertEqual(settings, FOREIGN)

    def test_remove_from_owned_only_settings_drops_the_hooks_key(self):
        settings = {"model": "opus"}
        inst.merge(settings, FRAGMENT)
        inst.remove(settings)
        self.assertEqual(settings, {"model": "opus"})

    def test_remove_with_nothing_owned_changes_nothing(self):
        settings = deep(FOREIGN)
        self.assertEqual(inst.remove(settings), [])
        self.assertEqual(settings, FOREIGN)


class CliTests(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(self.tmp, ignore_errors=True))
        self.settings = self.tmp / "settings.json"

    def write(self, value, indent=2):
        self.settings.write_text(json.dumps(value, indent=indent) + "\n")

    def test_dry_run_is_the_default_and_writes_nothing(self):
        self.write(FOREIGN)
        before = self.settings.read_text()
        result = run("--settings", str(self.settings))
        self.assertEqual(result.returncode, 0)
        self.assertIn("dry run", result.stdout)
        self.assertEqual(self.settings.read_text(), before)
        self.assertEqual(list(self.tmp.glob("*.bak-*")), [])

    def test_apply_writes_a_backup_and_keeps_indent(self):
        self.write(FOREIGN, indent=4)
        original = self.settings.read_text()
        self.assertEqual(run("--settings", str(self.settings), "--apply").returncode, 0)
        result = json.loads(self.settings.read_text())
        self.assertEqual(inst.missing(result, FRAGMENT), [])
        self.assertTrue(self.settings.read_text().startswith('{\n    "'))
        backups = list(self.tmp.glob("settings.json.bak-*"))
        self.assertEqual(len(backups), 1)
        self.assertEqual(backups[0].read_text(), original)

    def test_apply_twice_is_a_no_op_and_makes_no_second_backup(self):
        self.write(FOREIGN)
        run("--settings", str(self.settings), "--apply")
        state = self.settings.read_text()
        result = run("--settings", str(self.settings), "--apply")
        self.assertIn("already up to date", result.stdout)
        self.assertEqual(self.settings.read_text(), state)
        self.assertEqual(len(list(self.tmp.glob("settings.json.bak-*"))), 1)

    def test_apply_creates_a_missing_settings_file(self):
        self.assertEqual(run("--settings", str(self.settings), "--apply").returncode, 0)
        self.assertEqual(inst.missing(json.loads(self.settings.read_text()), FRAGMENT), [])

    def test_check_exit_codes(self):
        self.write(FOREIGN)
        self.assertEqual(run("--settings", str(self.settings), "--check").returncode, 1)
        run("--settings", str(self.settings), "--apply")
        self.assertEqual(run("--settings", str(self.settings), "--check").returncode, 0)

    def test_remove_apply_round_trip(self):
        self.write(FOREIGN)
        run("--settings", str(self.settings), "--apply")
        run("--settings", str(self.settings), "--apply", "--remove")
        self.assertEqual(json.loads(self.settings.read_text()), FOREIGN)

    def test_invalid_json_is_refused_and_left_alone(self):
        self.settings.write_text("{ not json")
        result = run("--settings", str(self.settings), "--apply")
        self.assertEqual(result.returncode, 1)
        self.assertIn("not valid JSON", result.stderr)
        self.assertEqual(self.settings.read_text(), "{ not json")

    def test_non_object_json_is_refused(self):
        self.settings.write_text("[1, 2]")
        result = run("--settings", str(self.settings), "--apply")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.settings.read_text(), "[1, 2]")

    def test_apply_writes_through_a_symlink_and_keeps_it(self):
        real = self.tmp / "real.json"
        real.write_text(json.dumps(FOREIGN, indent=2) + "\n")
        os.symlink(real, self.settings)
        self.assertEqual(run("--settings", str(self.settings), "--apply").returncode, 0)
        self.assertTrue(self.settings.is_symlink())
        self.assertEqual(inst.missing(json.loads(real.read_text()), FRAGMENT), [])

    def test_stdin_mode_merges_without_touching_any_file(self):
        result = run("--stdin", stdin=json.dumps(FOREIGN))
        self.assertEqual(result.returncode, 0)
        self.assertEqual(inst.missing(json.loads(result.stdout), FRAGMENT), [])
        self.assertEqual(result.stderr, "")

    def test_stdin_mode_accepts_empty_input_for_a_first_install(self):
        result = run("--stdin", stdin="")
        self.assertEqual(result.returncode, 0)
        self.assertEqual(inst.missing(json.loads(result.stdout), FRAGMENT), [])

    def test_stdin_mode_rejects_invalid_json_without_output(self):
        result = run("--stdin", stdin="{ nope")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")

    def test_every_fragment_command_runs_the_guard_binary_or_orch_hook_with_its_event(self):
        """The binary is built, not checked in: the script that builds it must exist, and each
        event must pass the mode (pre or coach) the binary's command line knows. The orch hook
        entries run the checked-in gate script with the event `orch hook` knows."""
        build = HERE.parent.parent / "scripts" / "build-guard"
        self.assertTrue(os.access(build, os.X_OK), "scripts/build-guard is missing or not executable")
        self.assertTrue(os.access(HERE / "orch-hook.sh", os.X_OK), "orch-hook.sh is missing or not executable")
        orch_events = {"SessionStart": "session-start", "Stop": "stop", "PreToolUse": "pretool"}
        seen = set()
        for event, groups in FRAGMENT["hooks"].items():
            for group in groups:
                for hook in group["hooks"]:
                    if hook["command"].startswith(ORCH_HOOK):
                        self.assertEqual(hook["command"], ORCH_HOOK + orch_events[event], event)
                        self.assertEqual(hook["timeout"], 10, event)
                        seen.add(event)
                        continue
                    want = GUARD_PRE if event == "PreToolUse" else GUARD_COACH
                    self.assertEqual(hook["command"], want, event)
                    self.assertEqual(hook["timeout"], 5, event)
        self.assertEqual(seen, set(orch_events))

    def test_orch_session_hooks_fire_on_every_session_start_source_and_every_stop(self):
        # SessionStart sources per the Claude Code hooks docs: the orchestrator needs its
        # board back after /clear and compaction too, not only at startup.
        self.assertEqual([g.get("matcher") for g in FRAGMENT["hooks"]["SessionStart"]], ["^(startup|resume|clear|compact)$"])
        self.assertEqual([g.get("matcher") for g in FRAGMENT["hooks"]["Stop"]], [None])

    def test_pre_tool_use_registers_the_guard_for_bash_and_read_and_orch_for_the_orchestrator_tools(self):
        groups = FRAGMENT["hooks"]["PreToolUse"]
        self.assertEqual([(g["matcher"], [h["command"] for h in g["hooks"]]) for g in groups],
                         [("Bash", [GUARD_PRE]), ("Read", [GUARD_PRE]),
                          ("Edit|Write|MultiEdit|NotebookEdit|Agent|Task|Workflow|Bash", [ORCH_HOOK + "pretool"])])

    def test_apply_builds_the_guard_first_and_the_binary_runs(self):
        self.write(FOREIGN)
        result = run("--settings", str(self.settings), "--apply")
        self.assertEqual(result.returncode, 0, result.stderr)
        binary = HERE.parent / "bin" / "orch-guard"
        self.assertTrue(os.access(binary, os.X_OK))
        self.assertEqual(subprocess.run([str(binary), "--version"], capture_output=True, text=True).returncode, 0)

    def test_apply_without_go_fails_clearly_and_changes_nothing(self):
        self.write(FOREIGN)
        before = self.settings.read_text()
        result = run("--settings", str(self.settings), "--apply", env={"PATH": str(self.tmp), "HOME": str(self.tmp)})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("go is not on PATH", result.stderr)
        self.assertEqual(self.settings.read_text(), before)
        self.assertEqual(list(self.tmp.glob("*.bak-*")), [])

    def test_dry_run_and_check_do_not_need_go(self):
        self.write(FOREIGN)
        env = {"PATH": str(self.tmp), "HOME": str(self.tmp)}
        self.assertEqual(run("--settings", str(self.settings), env=env).returncode, 0)
        self.assertEqual(run("--settings", str(self.settings), "--check", env=env).returncode, 1)


class MigrationTests(unittest.TestCase):
    """An install of the shell-script hooks is replaced by the binary's hooks."""

    def old_settings(self):
        settings = deep(FOREIGN)
        settings["hooks"]["PreToolUse"].append({"matcher": "Bash", "hooks": [
            {"type": "command", "command": old_command(n)} for n in OLD_SCRIPTS[:4]]})
        settings["hooks"]["PreToolUse"].append({"matcher": "Read", "hooks": [
            {"type": "command", "command": old_command("deny-big-read"), "timeout": 5}]})
        for event in ("PostToolUse", "PostToolUseFailure"):
            settings["hooks"][event] = [{"matcher": "Bash|Read|LSP|mcp__jbcontext__.*", "hooks": [
                {"type": "command", "command": old_command("lookup-coach"), "timeout": 5}]}]
        settings["hooks"]["UserPromptSubmit"] = [{"hooks": [
            {"type": "command", "command": old_command("lookup-coach"), "timeout": 5}]}]
        return settings

    def test_old_script_hooks_are_owned_and_the_binary_hooks_too(self):
        for name in OLD_SCRIPTS:
            self.assertTrue(inst.is_owned({"command": old_command(name)}))
        self.assertTrue(inst.is_owned({"command": GUARD_PRE}))
        self.assertFalse(inst.is_owned({"command": "jq -r .x"}))

    def test_prune_and_merge_leave_only_the_binary_hooks_and_foreign_ones(self):
        settings = self.old_settings()
        removed = inst.prune(settings, FRAGMENT)
        self.assertEqual(len(removed), 4 + 1 + 3)
        inst.merge(settings, FRAGMENT)
        owned = [h["command"] for gs in settings["hooks"].values() for g in gs for h in g["hooks"] if inst.is_owned(h)]
        self.assertEqual(sorted(owned), sorted(FRAGMENT_COMMANDS))
        self.assertEqual(inst.missing(settings, FRAGMENT), [])
        self.assertEqual(settings["hooks"]["SessionStart"], FOREIGN["hooks"]["SessionStart"] + FRAGMENT["hooks"]["SessionStart"])
        self.assertEqual(settings["hooks"]["PreToolUse"][0], FOREIGN["hooks"]["PreToolUse"][0])

    def test_one_command_for_two_matchers_is_registered_for_each(self):
        settings = {"hooks": {"PreToolUse": [{"matcher": "Read", "hooks": [{"type": "command", "command": GUARD_PRE, "timeout": 5}]}]}}
        added = inst.merge(settings, FRAGMENT)
        self.assertEqual(added.count(GUARD_PRE), 1)
        guard_matchers = [g["matcher"] for g in settings["hooks"]["PreToolUse"]
                          if GUARD_PRE in [h["command"] for h in g["hooks"]]]
        self.assertEqual(sorted(guard_matchers), ["Bash", "Read"])

    def test_hand_installed_orch_hooks_are_replaced_by_the_gate_script(self):
        settings = deep(FOREIGN)
        settings["hooks"]["SessionStart"].append({"matcher": "^(startup|resume|clear|compact)$", "hooks": [
            {"type": "command", "command": LEGACY_ORCH_HOOK + "session-start", "timeout": 10}]})
        settings["hooks"]["Stop"] = [{"hooks": [{"type": "command", "command": LEGACY_ORCH_HOOK + "stop", "timeout": 10}]}]
        settings["hooks"]["PreToolUse"].append({"matcher": "Edit|Write|Agent|Task|Bash", "hooks": [
            {"type": "command", "command": LEGACY_ORCH_PRETOOL, "timeout": 10}]})
        removed = inst.prune(settings, FRAGMENT)
        self.assertEqual(sorted(removed), sorted([LEGACY_ORCH_HOOK + "session-start", LEGACY_ORCH_HOOK + "stop", LEGACY_ORCH_PRETOOL]))
        inst.merge(settings, FRAGMENT)
        everything = [h["command"] for gs in settings["hooks"].values() for g in gs for h in g["hooks"]]
        self.assertEqual([c for c in everything if "scripts/orch" in c], [])
        self.assertEqual(everything.count(ORCH_HOOK + "stop"), 1)
        self.assertEqual(everything.count(ORCH_HOOK + "session-start"), 1)
        self.assertEqual(settings["hooks"]["SessionStart"][0], FOREIGN["hooks"]["SessionStart"][0])


class OrchHookGateTests(unittest.TestCase):
    """orch-hook.sh starts `orch hook <event>` only in a session orch could act on."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        (self.tmp / "hooks" / "claude").mkdir(parents=True)
        (self.tmp / "scripts").mkdir()
        self.gate = self.tmp / "hooks" / "claude" / "orch-hook.sh"
        self.gate.write_text((HERE / "orch-hook.sh").read_text())
        self.gate.chmod(0o755)
        self.log = self.tmp / "orch.log"
        orch = self.tmp / "scripts" / "orch"
        # Like orch hook blocking: logs its args and the payload it read, says why on stderr, exits 2.
        orch.write_text(f'#!/bin/sh\necho "$* $(cat)" >> "{self.log}"\necho blocked >&2\nexit 2\n')
        orch.chmod(0o755)
        self.roles = self.tmp / "roles"
        self.roles.mkdir()

    def ran(self, event, roles_dir=True, **env):
        """The `orch hook` calls the gate made, each as "<args> <payload>"; checks that a
        started orch's exit code and stderr come back unchanged and a skipped one exits 0."""
        base = {"PATH": "/usr/bin:/bin", "HOME": str(self.tmp)}
        if roles_dir:
            base["ORCH_ROLES_DIR"] = str(self.roles)
        result = subprocess.run([str(self.gate), event], env={**base, **env}, input='{"p":1}', capture_output=True, text=True)
        calls = self.log.read_text().splitlines() if self.log.exists() else []
        self.log.unlink(missing_ok=True)
        if calls:
            self.assertEqual((result.returncode, result.stderr), (2, "blocked\n"))
        else:
            self.assertEqual((result.returncode, result.stderr), (0, ""))
        for c in calls:
            self.assertTrue(c.endswith(' {"p":1}'), f"orch did not get the hook payload on stdin: {c!r}")
        return [c.removesuffix(' {"p":1}') for c in calls]

    def test_a_plain_session_never_starts_orch(self):
        for event in ("pretool", "stop", "session-start"):
            self.assertEqual(self.ran(event, HERDR_PANE_ID="w1:p1", ORCA_AGENT_SESSION_ID="claude_x"), [])

    def test_a_lead_starts_orch_for_stop_and_session_start_but_not_pretool(self):
        self.assertEqual(self.ran("pretool", ORCH_ROLE="lead"), [])
        self.assertEqual(self.ran("stop", ORCH_ROLE="lead"), ["hook stop"])
        self.assertEqual(self.ran("session-start", ORCH_ROLE="lead"), ["hook session-start"])

    def test_an_orchestrator_role_starts_orch_for_pretool(self):
        self.assertEqual(self.ran("pretool", ORCH_ROLE="orchestrator"), ["hook pretool"])

    def test_a_role_marker_from_any_backend_starts_orch(self):
        for var, ident in (("HERDR_PANE_ID", "w1:p1"), ("ORCA_PANE_KEY", "pk1"),
                           ("ORCA_TERMINAL_HANDLE", "th1"), ("ORCA_AGENT_SESSION_ID", "claude_s1")):
            (self.roles / ident).write_text("{}")
            self.assertEqual(self.ran("pretool", **{var: ident}), ["hook pretool"], var)
            (self.roles / ident).unlink()
            self.assertEqual(self.ran("pretool", **{var: ident}), [], var)

    def test_the_marker_is_found_where_orch_writes_it_when_roles_dir_is_unset_or_uses_a_tilde(self):
        default = self.tmp / ".local" / "state" / "orchestrate" / "roles"
        default.mkdir(parents=True)
        (default / "w1:p1").write_text("{}")
        self.assertEqual(self.ran("pretool", roles_dir=False, HERDR_PANE_ID="w1:p1"), ["hook pretool"])
        (self.tmp / "r").mkdir()
        (self.tmp / "r" / "w2:p1").write_text("{}")
        self.assertEqual(self.ran("pretool", roles_dir=False, ORCH_ROLES_DIR="~/r", HERDR_PANE_ID="w2:p1"), ["hook pretool"])


if __name__ == "__main__":
    unittest.main(verbosity=1)
