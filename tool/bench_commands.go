package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func benchOnlyFlags(fs *flag.FlagSet, names ...string) error {
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	var bad string
	fs.Visit(func(f *flag.Flag) {
		if bad == "" && !allowed[f.Name] {
			bad = f.Name
		}
	})
	if bad != "" {
		return fail("--%s is not valid for %s; valid flags: --%s", bad, fs.Name(), strings.Join(names, ", --"))
	}
	return nil
}

func benchRecordFlags(fs *flag.FlagSet) func() benchRecordOpts {
	repo := fs.String("repo", "", "historical repository path")
	commit := fs.String("commit", "", "explicit historical commit hash")
	basis := fs.String("commit-basis", "", "user or note (requires --commit)")
	evidence := fs.String("commit-evidence", "", "exact evidence for an asserted commit")
	text := fs.String("spec-text", "", "spec file verified against the gate hash")
	dirty := fs.String("dirty-from", "", "historical git status --porcelain=v1 file")
	console := fs.String("console", "", "console output to preserve as a blob")
	start := fs.String("console-start", "", "explicit start timestamp file")
	sessions := fs.Bool("with-sessions", false, "copy sessions in addition to exploration summaries")
	return func() benchRecordOpts {
		return benchRecordOpts{Repo: *repo, Commit: *commit, CommitBasis: *basis, CommitEvidence: *evidence, SpecText: *text, DirtyFrom: *dirty, Console: *console, ConsoleStart: *start, WithSessions: *sessions}
	}
}

type benchKeyOpts struct {
	Subject, FromRun, Author, Blind, Items, BaseKey, GitStatus string
	PromptTexts                                                []string
}
type benchStringFlags []string

func (v *benchStringFlags) String() string     { return strings.Join(*v, ",") }
func (v *benchStringFlags) Set(s string) error { *v = append(*v, s); return nil }

func benchKeyBinding(a *app, suite string, items []benchKeyItem, opts benchKeyOpts) (string, benchItemset, error) {
	var set benchItemset
	if opts.Subject != "" && opts.FromRun != "" {
		return "", set, fail("choose --subject or --from-run")
	}
	if opts.Blind != "" && opts.Blind != "true" && opts.Blind != "false" && opts.Blind != "unknown" {
		return "", set, fail("--blind must be true, false or unknown")
	}
	subject := opts.Subject
	if opts.FromRun != "" {
		r, e := benchRunLoad(suite, opts.FromRun)
		if e != nil {
			return "", set, e
		}
		subject = r.Subject
	}
	if subject == "" {
		runs, e := benchRuns(suite)
		if e != nil {
			return "", set, e
		}
		subjects := map[string]bool{}
		for _, r := range runs {
			if r.Subject != "" {
				subjects[r.Subject] = true
			}
		}
		if len(subjects) > 1 {
			return "", set, fail("suite has several subjects; use --subject or --from-run")
		}
		s, e := benchSuiteLoad(suite)
		if e != nil && !os.IsNotExist(e) {
			return "", set, e
		}
		subject = s.Subject.Default
		if subject == "" && len(subjects) == 1 {
			for id := range subjects {
				subject = id
			}
		}
	}
	if subject == "" {
		return "", set, fail("key subject required: use --subject or --from-run")
	}
	s, e := benchSubjectLoad(subject)
	if e != nil {
		return "", set, e
	}
	if opts.GitStatus != "" {
		copy := *s
		if e := benchApplySubjectOpts(&copy, benchRecordOpts{DirtyFrom: opts.GitStatus}); e != nil {
			return "", set, e
		}
		copy.ID = benchSubjectID(copy)
		copy.Description = benchDescribe(copy, nil)
		if e := benchSaveSubject(copy); e != nil {
			return "", set, e
		}
		subject = copy.ID
	}
	if opts.BaseKey != "" {
		if e := benchKeyName(opts.BaseKey); e != nil {
			return "", set, e
		}
		if _, e := benchKeyLoad(suite, opts.BaseKey); e != nil {
			return "", set, e
		}
	}
	its := []gateItem{}
	if opts.Items != "" {
		b, e := os.ReadFile(expandHome(opts.Items))
		if e != nil {
			return "", set, e
		}
		var rows []struct {
			ID         string   `json:"id"`
			ItemID     string   `json:"item_id"`
			Package    string   `json:"package"`
			Text       string   `json:"text"`
			Item       string   `json:"item"`
			Revision   string   `json:"revision"`
			Supersedes []string `json:"supersedes"`
		}
		if e := json.Unmarshal(b, &rows); e != nil {
			return "", set, e
		}
		for _, row := range rows {
			id := orDefault(row.ID, row.ItemID)
			pkg := row.Package
			if pkg == "" {
				pkg, _, _ = strings.Cut(id, "#")
			}
			its = append(its, gateItem{ID: itemKey(pkg, id), Package: pkg, Text: orDefault(row.Text, row.Item), Kind: "legacy", Revision: row.Revision, Supersedes: row.Supersedes})
		}
	} else {
		for _, it := range items {
			its = append(its, gateItem{ID: it.ID, Package: it.Package, Text: it.Item, Kind: "legacy"})
		}
	}
	ids := map[string]bool{}
	for _, it := range its {
		if it.ID == "" || it.Text == "" || ids[it.ID] {
			return "", set, fail("missing or duplicate item id %q", it.ID)
		}
		ids[it.ID] = true
	}
	for i, it := range items {
		if opts.Items != "" {
			if !ids[it.ID] {
				return "", set, fail("key item %s absent from --items", it.ID)
			}
			for _, input := range its {
				if input.ID != it.ID {
					continue
				}
				var original struct {
					Item string `json:"item"`
				}
				_ = json.Unmarshal(it.Original, &original)
				if input.Package != it.Package || (original.Item != "" && original.Item != input.Text) {
					return "", set, fail("key item %s differs from --items", it.ID)
				}
				items[i].Item = input.Text
			}
		}
	}
	set = benchMakeItemset("key", "", its)
	if e := benchStoreInit(a.now()); e != nil {
		return "", set, e
	}
	if e := benchSaveItemset(set); e != nil {
		return "", set, e
	}
	return subject, set, nil
}
func cmdBenchSpec(a *app, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(a.out, "bench spec add <file> [--variant-of HASH --label NAME --author TEXT --notes FILE] | --from-git REPO PATH [--all-versions]")
		return nil
	}
	if len(args) == 0 || args[0] != "add" {
		return fail("usage: bench spec add <file> [--variant-of HASH --label NAME] | --from-git REPO PATH [--all-versions]")
	}
	fs := a.flags("bench spec add")
	from := fs.String("from-git", "", "repository for byte-exact committed versions")
	all := fs.Bool("all-versions", false, "register every distinct committed version")
	parent := fs.String("variant-of", "", "base spec hash")
	label := fs.String("label", "", "variant label")
	author := fs.String("author", "", "recorded author")
	notes := fs.String("notes", "", "notes file to store as a blob")
	pos, e := parseArgs(fs, args[1:])
	if e != nil {
		return e
	}
	if e := exactArgs(pos, 1, "bench spec add <file> | --from-git REPO PATH [--all-versions]"); e != nil {
		return e
	}
	if *all && *from == "" {
		return fail("--all-versions requires --from-git")
	}
	if (*parent == "") != (*label == "") {
		return fail("--variant-of and --label go together")
	}
	if *parent != "" {
		if e := benchName(*label); e != nil {
			return e
		}
		if _, e := objectGet("spec", *parent); e != nil {
			return e
		}
	}
	if e := benchStoreInit(a.now()); e != nil {
		return e
	}
	repo := *from
	if repo == "" {
		repo, _ = repoRoot(a.cwd)
	}
	path := pos[0]
	info := benchRepoInfo(repo)
	save := func(text, path string) error {
		v, set := benchSpecFromText(text, path, info, a.now())
		benchSpecHistory(repo, &v, true)
		if len(v.Commits) > 0 {
			v.State = "committed"
		} else if *from == "" {
			v.State = "untracked"
		}
		if *parent == v.Hash {
			return fail("a spec cannot be its own variant")
		}
		v.VariantOf, v.VariantLabel, v.Author = *parent, *label, *author
		if *notes != "" {
			b, e := os.ReadFile(expandHome(*notes))
			if e != nil {
				return e
			}
			v.NotesBlob, e = benchBlob(b, filepath.Base(*notes), *notes, a.now())
			if e != nil {
				return e
			}
		}
		if e := benchSaveSpec(v, set, text); e != nil {
			return e
		}
		fmt.Fprintf(a.out, "spec: %s (%d packages, itemset %s)\n", v.Hash, len(v.Packages), set.ID)
		return nil
	}
	if *from != "" {
		log := "HEAD"
		if *all {
			log, e = git(repo, "log", "--all", "--reverse", "--format=%H", "--", path)
			if e != nil {
				return e
			}
		}
		seen := map[string]bool{}
		for _, commit := range strings.Fields(log) {
			text, e := benchGitText(repo, commit, path)
			if e != nil {
				return e
			}
			hash := specHash([]byte(text))
			if seen[hash] {
				continue
			}
			seen[hash] = true
			if e := save(text, path); e != nil {
				return e
			}
		}
		return nil
	}
	full := expandHome(path)
	if !filepath.IsAbs(full) {
		full = filepath.Join(a.cwd, full)
	}
	b, e := os.ReadFile(full)
	if e != nil {
		return e
	}
	rel, e := filepath.Rel(repo, full)
	if e != nil || repo == "" {
		rel = path
	}
	return save(string(b), rel)
}

func benchShow(a *app, r *benchRun) error {
	return json.NewEncoder(a.out).Encode(struct {
		*benchRun
		TestedOn string `json:"tested_on"`
	}{r, benchTestedOn(r)})
}

const benchReadmeSubjects = `
## Subjects and immutable objects (schema 2)

New store.json files identify schema 2; migration leaves historical schema 1 metadata intact.
objects/spec/HASH.md preserves byte-exact spec text
(up to 2 MB); HASH is the 16-hex gate specHash and spec_sha256 is the full SHA256.
objects/spec/HASH.json records packages, content hashes, history and variants.
objects/itemset/ID.json holds canonical items. objects/blob/SHA256 holds private
prompts, console output, notes and small dirty patches. Subjects bind repo identity,
spec version, commit evidence and dirty state in subjects/ID.json. Runs and keys reference these ids.
Store directories are 0700 and files are 0600. No redaction is provided.

- Register versions: orch bench spec add --from-git /repo docs/spec.md --all-versions
- Register variant: orch bench spec add builds.md --variant-of HASH --label builds-7 --notes NOTES.md
- Bind a key: orch bench key add suite key.json --note 'review' --from-run run3 --author 'recorded author' --blind unknown --items items.json --base-key key-1
- Import with evidence: orch bench import /gate/HASH --suite suite --label run3 --repo /repo --spec-text spec.md
  An explicit historical commit requires --commit SHA --commit-basis user|note
  --commit-evidence 'exact evidence'. --dirty-from accepts porcelain git status.
  --console output.txt preserves output; --console-start start.txt computes wall
  clock only with explicit timestamps. --with-sessions copies session transcripts.
- Verify: orch bench verify --suite suite
- Propose schema 1 reconstruction without changing originals: orch bench migrate --suite suite
- Export: orch bench export suite --out suite.tar.gz
- Restore into a fresh store: orch bench import-archive suite.tar.gz

Report-only imports preserve report.md, never derive counts from prose.
Historical commit, dirty state, cost, turns, runner and author are never guessed.
Unknown spec text is allowed with a warning; verification reports it as information.
Missing raw paths do not affect stored-data comparisons. Other repos or spec paths
require --cross-subject. --total refuses differing group sets; --total=all opts in.
Pass seconds (summed) and wall clock are distinct. Behavior identity uses exact ids
and explicit mapping, never fuzzy location overlap. Missing identity makes dependent
quality unavailable. Keys and original runs are immutable. Subject correction adds
a new subject and a migration proposal with proposed history, never a new binding
on the original run. Unknown historical subjects remain unknown.
Archives contain spec/item text, repository paths and prompts. Keep them private.
`
