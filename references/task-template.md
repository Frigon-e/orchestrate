---
id: @@ID@@
kind: @@KIND@@
repo: @@REPO@@
base: @@BASE@@
harness: @@HARNESS@@
model: @@MODEL@@
effort: @@EFFORT@@
depends_on: [@@DEPENDS@@]
---

# Intent

{{Why this task exists and what the user wants out of it, in their words where possible. One short paragraph. The lead re-reads this when the spec runs out.}}

# Scope

In: {{what this lead owns: features, files or directories, branches}}
Out: {{what it must not touch, and which other task owns it if any}}

# Pointers

{{Where to start reading, one per line: `path/file.ext:line` - why it matters; docs, ADRs, issues, earlier task reports. Pointers, not pasted content: cite a spec or doc section as `path:first-last`, never paste it, and never point at a whole file larger than 8 KB or at a file that loads on its own (root and nested `CLAUDE.md`). Every character of this brief is re-read by the lead on every turn and after each compaction, so keep the whole brief under 15 KB (`orch check` warns above 15000 characters) and write nothing twice. To find more, search with `orch ctx` (find, sym, text, batch) before reading files.}}

# Spec

{{What to build or find out. For ship: behaviour, decisions already made, constraints, edge cases. For scout: the questions to answer and what a good answer looks like.}}

# Acceptance

{{How the lead proves it is done: commands to run and their expected result, tests that must exist, or for scout the evidence each finding needs. For Go tests in orch write `orch test-changed`, not a `-run` pattern; never ask for the full suite. When the change alters a default or a mode, add: search the tests for the old default (`orch ctx text`) and run the neighbouring test files by hand, because `orch test-changed` selects only tests that reference changed code.}}

# Authority

May decide: {{technical choices the lead settles alone}}
Must escalate: {{choices that go back as needs-decision: product behaviour, compatibility, scope changes, anything irreversible}}
Never: {{hard limits: push, merge to the base branch, touch secrets, edit outside scope}}

# Report-back

@@REPORT_BACK@@
