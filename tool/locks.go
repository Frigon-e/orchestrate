package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A named lock gives one lead at a time a shared resource (a test VM, a
// device, a fixed port) across many steps, which a single command's flock
// can't span. Locks are machine-wide files, so leads of every repo see them.
// A lock frees when its lead closes, its pane is gone, or its task is
// deleted; nothing else expires it.

var lockNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

type lockHolder struct {
	Name  string `json:"name"`
	Lead  string `json:"lead"`
	Dir   string `json:"dir"`
	Pane  string `json:"pane,omitempty"`
	Since int64  `json:"since"`
}

func locksDir() string {
	return expandHome(envOr("ORCH_LOCKS_DIR", "~/.local/state/orchestrate/locks"))
}

func lockPath(name string) string { return filepath.Join(locksDir(), name+".json") }

func readLock(name string) (*lockHolder, error) {
	b, err := os.ReadFile(lockPath(name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var h lockHolder
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fail("%s: %v", lockPath(name), err)
	}
	return &h, nil
}

// lockStale reports whether the holder can no longer use the lock. When
// Herdr can't be reached it keeps the lock: a wrongly kept lock only delays
// a run, a wrongly freed one lets two leads share the resource.
func lockStale(h *lockHolder) bool {
	if _, err := os.Stat(filepath.Join(h.Dir, "task.md")); os.IsNotExist(err) {
		return true
	}
	l, err := loadLead(h.Dir)
	if err != nil || l == nil {
		return false
	}
	if l.ClosedAt != 0 || (h.Pane != "" && l.PaneID != h.Pane) {
		return true
	}
	if l.PaneID != "" {
		if ok, err := paneExists(l.PaneID); err == nil && !ok {
			return true
		}
	}
	return false
}

// withLockGuard serializes the check-and-take of one lock name between
// processes, so two leads can't both see a stale holder and both take over.
func withLockGuard(name string, fn func() error) error {
	if err := os.MkdirAll(locksDir(), 0o755); err != nil {
		return err
	}
	return withFlock(filepath.Join(locksDir(), name+".guard"), fn)
}

// withFlock runs fn holding an exclusive flock on path, created if absent.
func withFlock(path string, fn func() error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// tryLock takes the lock for me, or returns the live holder that has it.
func tryLock(me lockHolder) (held *lockHolder, err error) {
	err = withLockGuard(me.Name, func() error {
		cur, err := readLock(me.Name)
		if err != nil {
			return err
		}
		if cur != nil && cur.Dir != me.Dir && !lockStale(cur) {
			held = cur
			return nil
		}
		if cur != nil && cur.Dir == me.Dir {
			return nil
		}
		b, _ := json.MarshalIndent(me, "", "  ")
		return writeAtomic(lockPath(me.Name), append(b, '\n'))
	})
	return held, err
}

// releaseLocks frees every lock held by the task in dir; close calls it.
func releaseLocks(dir string) []string {
	var freed []string
	for _, name := range lockNames() {
		_ = withLockGuard(name, func() error {
			if cur, err := readLock(name); err == nil && cur != nil && cur.Dir == dir {
				if os.Remove(lockPath(name)) == nil {
					freed = append(freed, name)
				}
			}
			return nil
		})
	}
	return freed
}

func lockNames() []string {
	paths, _ := filepath.Glob(filepath.Join(locksDir(), "*.json"))
	var out []string
	for _, p := range paths {
		out = append(out, strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	sort.Strings(out)
	return out
}

// lockLines describes the held locks for the board.
func (a *app) lockLines() []string {
	var out []string
	for _, name := range lockNames() {
		h, err := readLock(name)
		if err != nil || h == nil {
			continue
		}
		line := fmt.Sprintf("lock: %s held by %s (%s)", name, h.Lead, a.age(h.Since))
		if lockStale(h) {
			line += ", stale: frees on the next acquire"
		}
		out = append(out, line)
	}
	return out
}

func cmdLock(a *app, args []string) error {
	const form = "lock acquire|release <name> [--dir D] [--wait S] [--force] | lock list"
	if len(args) == 0 {
		return &exitError{code: 2, msg: "usage: orch " + form}
	}
	sub := args[0]
	fs := a.flags("lock " + sub)
	dirFlag := fs.String("dir", "", "task directory (default: $ORCH_TASK_DIR)")
	wait := fs.Int("wait", 0, "acquire: seconds to wait for the holder (0 fails at once)")
	force := fs.Bool("force", false, "release: free a lock another lead holds (needs the user's agreement)")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if sub == "list" {
		lines := a.lockLines()
		if len(lines) == 0 {
			lines = []string{"no locks held"}
		}
		fmt.Fprintln(a.out, strings.Join(lines, "\n"))
		return nil
	}
	if (sub != "acquire" && sub != "release") || len(pos) != 1 {
		return &exitError{code: 2, msg: "usage: orch " + form}
	}
	name := pos[0]
	if !lockNameRe.MatchString(name) {
		return fail("lock name %q must be a short lowercase slug", name)
	}
	if sub == "release" && *force {
		if err := os.Remove(lockPath(name)); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Fprintln(a.out, "released "+name+" (forced)")
		return nil
	}
	dir, err := leadDir(*dirFlag)
	if err != nil {
		return err
	}
	if sub == "release" {
		return withLockGuard(name, func() error {
			cur, err := readLock(name)
			if err != nil {
				return err
			}
			if cur == nil {
				fmt.Fprintln(a.out, name+" was not held")
				return nil
			}
			if cur.Dir != dir {
				return fail("%s is held by %s, not this task", name, cur.Lead)
			}
			if err := os.Remove(lockPath(name)); err != nil {
				return err
			}
			fmt.Fprintln(a.out, "released "+name)
			return nil
		})
	}

	me := lockHolder{Name: name, Lead: filepath.Base(dir), Dir: dir, Since: a.now().Unix()}
	if l, err := loadLead(dir); err == nil && l != nil {
		me.Pane = l.PaneID
	}
	poll := 10 * time.Second
	if ms, err := strconv.Atoi(os.Getenv("ORCH_LOCK_POLL_MS")); err == nil && ms > 0 {
		poll = time.Duration(ms) * time.Millisecond
	}
	deadline := time.Now().Add(time.Duration(*wait) * time.Second)
	waited := false
	for {
		held, err := tryLock(me)
		if err != nil {
			return err
		}
		if held == nil {
			if waited {
				_ = a.appendStatus(dir, "working", "got lock "+name, "", "orch")
			}
			fmt.Fprintln(a.out, "acquired "+name)
			return nil
		}
		msg := fmt.Sprintf("%s is held by %s since %s ago", name, held.Lead, a.age(held.Since))
		if *wait > 0 && !waited {
			waited = true
			_ = a.appendStatus(dir, "paused", "waiting for lock "+msg, "", "orch")
		}
		if !time.Now().Add(poll).Before(deadline) {
			return &exitError{code: 75, msg: msg + "; acquire again with --wait, or work on something else first"}
		}
		time.Sleep(poll)
	}
}
