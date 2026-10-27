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
        self.assertEqual(settings["hooks"]["SessionStart"], FOREIGN["hooks"]["SessionStart"])
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

    def test_every_fragment_command_runs_the_guard_binary_the_build_script_makes(self):
        """The binary is built, not checked in: the script that builds it must exist, and each
        event must pass the mode (pre or coach) the binary's command line knows."""
        build = HERE.parent.parent / "scripts" / "build-guard"
        self.assertTrue(os.access(build, os.X_OK), "scripts/build-guard is missing or not executable")
        for event, groups in FRAGMENT["hooks"].items():
            for group in groups:
                for hook in group["hooks"]:
                    want = GUARD_PRE if event == "PreToolUse" else GUARD_COACH
                    self.assertEqual(hook["command"], want, event)
                    self.assertEqual(hook["timeout"], 5, event)

    def test_pre_tool_use_registers_one_command_for_bash_and_one_for_read(self):
        groups = FRAGMENT["hooks"]["PreToolUse"]
        self.assertEqual([(g["matcher"], [h["command"] for h in g["hooks"]]) for g in groups],
                         [("Bash", [GUARD_PRE]), ("Read", [GUARD_PRE])])

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
        self.assertEqual(settings["hooks"]["SessionStart"], FOREIGN["hooks"]["SessionStart"])
        self.assertEqual(settings["hooks"]["PreToolUse"][0], FOREIGN["hooks"]["PreToolUse"][0])

    def test_one_command_for_two_matchers_is_registered_for_each(self):
        settings = {"hooks": {"PreToolUse": [{"matcher": "Read", "hooks": [{"type": "command", "command": GUARD_PRE, "timeout": 5}]}]}}
        added = inst.merge(settings, FRAGMENT)
        self.assertEqual(added.count(GUARD_PRE), 1)
        self.assertEqual(sorted(g["matcher"] for g in settings["hooks"]["PreToolUse"]), ["Bash", "Read"])


if __name__ == "__main__":
    unittest.main(verbosity=1)
