package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// repoConfig comes from <repo>/.agents/orchestrate.toml, so anyone who can
// commit to the repo controls it. It only shapes briefs.
type repoConfig struct {
	benchSuiteIgnored bool
	BaseBranch        string   `toml:"base_branch"`
	LeadSkills        []string `toml:"lead_skills"`
	Check             string   `toml:"check"`
	QuietMinutes      int      `toml:"quiet_minutes"`
	// StallMinutes is how long a working lead may report nothing and change
	// nothing in its worktree before the watcher says it stalled.
	StallMinutes int `toml:"stall_minutes"`
	// Notes reach the orchestrator on every board, so loadConfig caps them.
	Notes []string `toml:"notes"`
	// Locks maps a lock name to when a lead must hold it; every brief says so.
	Locks map[string]string `toml:"locks"`
	// HumanOnly lists question areas that only a person decides, whatever the
	// agents answered (money, tax, legal). `orch sweep` routes them to the user.
	HumanOnly []string `toml:"human_only"`
}

const maxNotes, maxNoteLen = 5, 160

// userConfig holds what a repository must not control: how leads launch
// and how many run on this machine. Each file decodes into its own type, so
// a repo key can't reach these fields under any spelling (the decoder
// matches field names case-insensitively).
type userConfig struct {
	UserBenchSuite string   `toml:"bench_suite"`
	MaxLeads       int      `toml:"max_leads"`
	ClaudeArgs     []string `toml:"claude_args"`
	PiArgs         []string `toml:"pi_args"`
	// LeadContextLimit is the context size, in tokens, past which a lead
	// handed unrelated work asks for a compact or a fresh session.
	LeadContextLimit int `toml:"lead_context_limit"`
	// UnreadMinutes is how long a message may sit unread, with its lead idle,
	// before the watcher rings the doorbell once more. 0 turns that off.
	UnreadMinutes int `toml:"unread_minutes"`
	// Dispatch rules say which harness and model suit which work. orch only
	// prints them; the orchestrator matches them by judgment.
	Dispatch   []dispatchRule       `toml:"dispatch"`
	Roles      map[string]modelRole `toml:"roles"`
	GateBudget *gateBudgetConfig    `toml:"gate_budget"`
	// Triage: a decision model rates each task at spawn (see triage.go).
	// TriageMode is shadow (record only, the default), enforce or off.
	// WeakModels may only run tasks triage rates settled, in enforce mode.
	TriageModel string   `toml:"triage_model"`
	TriageMode  string   `toml:"triage_mode"`
	WeakModels  []string `toml:"weak_models"`
	// QuickMode is shadow (the default: a triage verdict against a quick job
	// only warns), enforce (it refuses) or off (no `orch quick`).
	QuickMode string `toml:"quick_mode"`
	// AllowedModels is the whole list of models orch will start, per harness.
	// Empty means no restriction. A repository cannot set it.
	AllowedModels map[string][]string `toml:"allowed_models"`
	// AllowedWhenLimited adds models to the list only while a harness is
	// limited, for example Haiku while pi is down.
	AllowedWhenLimited []limitedAllow `toml:"allowed_when_limited"`
	// CtxLangs adds or overrides languages for `orch ctx` (see ctx_lang.go).
	CtxLangs []langCfg `toml:"ctx_langs"`
	// CtxIndex is the policy for the jbcontext index behind `orch ctx`: full
	// (the default: build the index when a repo has none and refresh a stale
	// one), incremental (refresh an existing index only, never a first full
	// index) or off (never call jbcontext). CtxDeny lists repositories (paths)
	// that never use jbcontext, whatever the policy.
	CtxIndex string   `toml:"ctx_index"`
	CtxDeny  []string `toml:"ctx_deny"`
	CtxTrust []string `toml:"ctx_trust"` // repos allowed to run repo-local language servers
}

// limitedAllow allows the models in Allow (by harness) while Harness is limited.
type limitedAllow struct {
	Harness string              `toml:"harness"`
	Lane    string              `toml:"lane"`
	Allow   map[string][]string `toml:"allow"`
}

type dispatchRule struct {
	When string    `toml:"when"`
	Use  []profile `toml:"use"`
	Why  string    `toml:"why"`
	// Tier ties the rule to a triage tier (settled, standard or planning),
	// so `orch spawn --auto` can pick its profile.
	Tier string `toml:"tier"`
}

type profile struct {
	Harness  string `toml:"harness"`
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
	Effort   string `toml:"effort"`
	// Degraded marks a weaker stand-in, used only when the primary profile's
	// harness is limited, and only with the user's approval.
	Degraded bool `toml:"degraded"`
}

func (p profile) String() string {
	s := strings.TrimSpace(p.Harness + " " + p.Model + " " + p.Effort)
	if p.Harness == "api" {
		s = strings.TrimSpace(p.Harness + " " + p.Provider + " " + p.Model)
	}
	if p.Degraded {
		s += " [degraded]"
	}
	return s
}

type config struct {
	repoConfig
	userConfig
	repoFile string // empty when the repo has no orchestrate.toml
}

// site is the repo an orchestrator command runs against.
type site struct {
	repo string
	root string
	cfg  config
}

func (a *app) site() (site, error) {
	repo, err := repoRoot(a.cwd)
	if err != nil {
		return site{}, err
	}
	cfg, err := loadConfig(repo)
	if err != nil {
		return site{}, err
	}
	root := runRoot(repo)
	if _, err := os.Stat(root); err == nil {
		// Existing state, possibly from before the marker: record whose it is.
		_ = claimRoot(root, repo)
	}
	return site{repo: repo, root: root, cfg: cfg}, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

// repoRoot returns the main checkout of the repository containing dir, also
// when dir is inside a linked worktree.
func repoRoot(dir string) (string, error) {
	common, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		return "", fail("not inside a git repository: %s", dir)
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common), nil
	}
	if top, err := git(dir, "rev-parse", "--show-toplevel"); err == nil && top != "" {
		return top, nil
	}
	return common, nil
}

func herdrWorktreesDir() string {
	var cfg struct {
		Worktrees struct {
			Directory string `toml:"directory"`
		} `toml:"worktrees"`
	}
	path := expandHome(envOr("ORCH_HERDR_CONFIG", "~/.config/herdr/config.toml"))
	if _, err := toml.DecodeFile(path, &cfg); err != nil || cfg.Worktrees.Directory == "" {
		return ""
	}
	return expandHome(cfg.Worktrees.Directory)
}

// runRoot keeps a repo's run state next to its Herdr worktrees, so both live
// under one directory per repo. The directory is named after the repo folder
// and records the repo path in a `.repo` file (see claimRoot); a different
// repo with the same folder name gets a path hash added instead of sharing
// the first one's state. It only reads, so a dry run still writes nothing.
func runRoot(repo string) string {
	name := filepath.Base(repo)
	plain := runRootNamed(name)
	if owner := rootOwner(plain); owner == "" || owner == repo {
		return plain
	}
	sum := sha256.Sum256([]byte(repo))
	return runRootNamed(name + "-" + hex.EncodeToString(sum[:4]))
}

func runRootNamed(name string) string {
	if home := os.Getenv("ORCH_HOME"); home != "" {
		return filepath.Join(expandHome(home), name)
	}
	if dir := herdrWorktreesDir(); dir != "" {
		return filepath.Join(dir, name, ".orchestrate")
	}
	return filepath.Join(expandHome("~/.local/state/orchestrate"), name)
}

// rootOwner is the repo path recorded in root, or "" when nobody claimed it.
func rootOwner(root string) string {
	b, _ := os.ReadFile(filepath.Join(root, ".repo"))
	return strings.TrimSpace(string(b))
}

// claimRoot records repo as the owner of root, creating root. Call it where
// state is first written. A root from before the marker belongs to the first
// repo that claims it. The marker is created exclusively, so of two repos that
// race for one name the loser is told and must re-resolve with runRoot.
func claimRoot(root, repo string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(root, ".repo"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if owner := rootOwner(root); owner != repo && owner != "" {
			return fail("%s already belongs to %s; run orch again", root, owner)
		}
		return nil
	}
	defer f.Close()
	_, err = f.WriteString(repo + "\n")
	return err
}

func rolesDir() string {
	return expandHome(envOr("ORCH_ROLES_DIR", "~/.local/state/orchestrate/roles"))
}

func userConfigPath() string {
	if p := os.Getenv("ORCH_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(envOr("XDG_CONFIG_HOME", expandHome("~/.config")), "orchestrate", "config.toml")
}

var userKeys = []string{"gate_budget", "ctx_trust", "roles", "max_leads", "claude_args", "pi_args", "lead_context_limit", "unread_minutes", "dispatch", "triage_model", "triage_mode", "quick_mode", "weak_models", "allowed_models", "allowed_when_limited"}

// removedKeys are config keys of removed features. A file that still has one
// loads as if it did not.
var removedKeys = []string{"reuse_shadow"}

func removedKey(k string) bool {
	return slices.ContainsFunc(removedKeys, func(o string) bool { return strings.EqualFold(o, k) })
}

// decodeStrict refuses keys v has no field for. A key that belongs in the
// other file gets a pointer there instead of "unknown key".
func decodeStrict(path string, v any, otherKeys []string, otherFile string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	md, err := toml.DecodeFile(path, v)
	if err != nil {
		return fail("%s: %v", path, err)
	}
	for _, key := range md.Undecoded() {
		k := key.String()
		if removedKey(k) {
			continue
		}
		if slices.ContainsFunc(otherKeys, func(o string) bool { return strings.EqualFold(o, k) }) {
			return fail("%s: %q is not allowed here; it belongs in %s", path, k, otherFile)
		}
		return fail("%s: unknown key %q", path, k)
	}
	return nil
}

var repoKeys = []string{"base_branch", "lead_skills", "check", "quiet_minutes", "stall_minutes", "notes", "locks", "human_only"}

// readRepoConfig decodes a repo file over the defaults already in rc.
func readRepoConfig(path string, rc *repoConfig) error {
	if _, err := os.Stat(path); err == nil {
		md, err := toml.DecodeFile(path, rc)
		if err != nil {
			return fail("%s: %v", path, err)
		}
		for _, key := range md.Undecoded() {
			k := key.String()
			if len(key) > 0 && strings.EqualFold(key[0], "bench_suite") {
				rc.benchSuiteIgnored = true
				continue
			}
			if removedKey(k) {
				continue
			}
			if slices.ContainsFunc(userKeys, func(o string) bool { return strings.EqualFold(o, k) }) {
				return fail("%s: %q is not allowed here; it belongs in %s", path, k, userConfigPath())
			}
			return fail("%s: unknown key %q", path, k)
		}
	}
	if len(rc.Notes) > maxNotes {
		return fail("%s: %d notes, at most %d: rewrite or drop one before adding another", path, len(rc.Notes), maxNotes)
	}
	for _, n := range rc.Notes {
		if strings.TrimSpace(n) == "" || strings.ContainsAny(n, "\r\n") || utf8.RuneCountInString(n) > maxNoteLen {
			return fail("%s: each note must be one non-empty line of at most %d characters: %q", path, maxNoteLen, short(n, 40))
		}
	}
	if len(rc.Locks) > maxNotes {
		return fail("%s: %d locks, at most %d", path, len(rc.Locks), maxNotes)
	}
	for name, when := range rc.Locks {
		if !lockNameRe.MatchString(name) {
			return fail("%s: lock name %q must be a short lowercase slug", path, name)
		}
		if strings.TrimSpace(when) == "" || strings.ContainsAny(when, "\r\n") || utf8.RuneCountInString(when) > maxNoteLen {
			return fail("%s: lock %s needs one line of at most %d characters saying when to hold it", path, name, maxNoteLen)
		}
	}
	return nil
}

// loadUserConfig reads the machine's settings over their defaults. Leads use
// it alone; they have no repo checkout to read the rest from.
func loadUserConfig() (userConfig, error) {
	uc := userConfig{MaxLeads: 3, LeadContextLimit: 150_000, UnreadMinutes: 10, TriageMode: "shadow", QuickMode: "shadow"}
	err := decodeStrict(userConfigPath(), &uc, repoKeys, "<repo>/.agents/orchestrate.toml")
	if err == nil && uc.GateBudget != nil {
		limits := gateRunLimits(nil, uc.GateBudget)
		// Total output defaults scale with the actual pass count. Validate an
		// explicit reserve against an explicit total here, not a zero-pass run.
		if uc.GateBudget.OutputTokens == nil && uc.GateBudget.OutputReserve != nil {
			limits.OutputTokens = max(limits.OutputTokens, *uc.GateBudget.OutputReserve)
		}
		if e := newRunBudget(limits, time.Now()).validate(); e != nil {
			err = fail("%s: gate_budget: %v", userConfigPath(), e)
		}
	}
	return uc, err
}

func loadConfig(repo string) (config, error) {
	cfg := config{repoConfig: repoConfig{LeadSkills: []string{"route-work"}, QuietMinutes: 30, StallMinutes: 15}}
	path := filepath.Join(repo, ".agents", "orchestrate.toml")
	user := userConfigPath()
	if err := readRepoConfig(path, &cfg.repoConfig); err != nil {
		return cfg, err
	}
	if _, err := os.Stat(path); err == nil {
		cfg.repoFile = path
	}
	uc, err := loadUserConfig()
	if err != nil {
		return cfg, err
	}
	cfg.userConfig = uc
	switch cfg.CtxIndex {
	case "", "full", "incremental", "off":
	default:
		return cfg, fail("%s: ctx_index must be full, incremental or off, not %q", user, cfg.CtxIndex)
	}
	for name, r := range cfg.Roles {
		if name == "shadow_flash" {
			delete(cfg.Roles, name) // the role of the removed shadow judges
			continue
		}
		if !slices.Contains(modelRoles, name) {
			return cfg, fail("%s: unknown role %q", user, name)
		}
		if r.ClassifierLimits != nil {
			if name != "question_classifier" {
				return cfg, fail("%s: classifier_limits belongs only to question_classifier", user)
			}
			if err := r.ClassifierLimits.validate(); err != nil {
				return cfg, fail("%s: %v", user, err)
			}
		}
		if r.Promote && name != "question_classifier" {
			return cfg, fail("%s: promote belongs only to question_classifier", user)
		}
		for _, p := range r.Use {
			if (name == "decider" || name == "question_classifier") && p.Harness != "api" {
				return cfg, fail("%s: role %s needs harness api and a provider", user, name)
			}
			if (p.Harness != "claude" && p.Harness != "pi" && p.Harness != "api") || p.Model == "" || (p.Harness == "api" && ((name != "decider" && name != "gate_adjudicator" && name != "question_classifier") || p.Provider == "" || !validLane("pi:"+p.Provider))) {
				return cfg, fail("%s: role %s: choices need claude/pi and a model, or api with a provider and model for decider, gate_adjudicator or question_classifier", user, name)
			}
		}
	}
	for i, r := range cfg.Dispatch {
		if strings.TrimSpace(r.When) == "" || len(r.Use) == 0 {
			return cfg, fail("%s: dispatch rule %d needs `when` and at least one `use` profile", user, i+1)
		}
		if t := r.Tier; t != "" && t != "settled" && t != "standard" && t != "planning" {
			return cfg, fail("%s: dispatch rule %d: tier must be settled, standard or planning, not %q", user, i+1, t)
		}
		for _, p := range r.Use {
			if (p.Harness != "claude" && p.Harness != "pi") || p.Model == "" {
				return cfg, fail("%s: dispatch rule %d: each profile needs harness claude or pi and a model", user, i+1)
			}
			if err := cfg.modelListed(p.Harness, p.Model); err != nil {
				return cfg, fail("%s: dispatch rule %d: %v", user, i+1, err)
			}
		}
	}
	for h := range cfg.AllowedModels {
		if h != "claude" && h != "pi" {
			return cfg, fail("%s: allowed_models has harness %q; use claude or pi", user, h)
		}
	}
	for i, w := range cfg.AllowedWhenLimited {
		if w.Lane != "" && !validLane(w.Lane) {
			return cfg, fail("%s: allowed_when_limited entry %d: invalid lane %q", user, i+1, w.Lane)
		}
		if w.Harness != "claude" && w.Harness != "pi" {
			return cfg, fail("%s: allowed_when_limited entry %d: harness must be claude or pi", user, i+1)
		}
		for h := range w.Allow {
			if h != "claude" && h != "pi" {
				return cfg, fail("%s: allowed_when_limited entry %d allows harness %q; use claude or pi", user, i+1, h)
			}
		}
	}
	if m := cfg.QuickMode; m != "shadow" && m != "enforce" && m != "off" {
		return cfg, fail("%s: quick_mode must be shadow, enforce or off, not %q", user, m)
	}
	if m := cfg.TriageMode; m != "shadow" && m != "enforce" && m != "off" {
		return cfg, fail("%s: triage_mode must be shadow, enforce or off, not %q", user, m)
	}
	if cfg.MaxLeads < 1 || cfg.QuietMinutes < 1 || cfg.StallMinutes < 1 || cfg.LeadContextLimit < 1 {
		return cfg, fail("max_leads and lead_context_limit (%s) and quiet_minutes and stall_minutes (%s) must be at least 1", user, path)
	}
	if cfg.BaseBranch == "" {
		cfg.BaseBranch = "HEAD"
		if b, err := git(repo, "symbolic-ref", "--short", "HEAD"); err == nil && b != "" {
			cfg.BaseBranch = b
		}
	}
	return cfg, nil
}

// selfPath is how agents should invoke orch: the wrapper, not the cached binary.
func selfPath() string {
	if p := os.Getenv("ORCH_SELF"); p != "" {
		return p
	}
	p, _ := os.Executable()
	return p
}

func skillDir() (string, error) {
	if d := os.Getenv("ORCH_SKILL_DIR"); d != "" {
		return d, nil
	}
	return "", fail("ORCH_SKILL_DIR is unset; run orch through the skill's scripts/orch")
}
