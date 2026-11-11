package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// jbRefreshEvery: a pi prompt starts a jbcontext refresh of its checkout at most this often.
const jbRefreshEvery = 10 * time.Minute

// jbRefreshStart starts the refresh and leaves it running; tests replace it.
var jbRefreshStart = func(root string) {
	cmd := exec.Command("jbcontext", "index", "--only-incremental", "--silent", "--project-path", root)
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// jbRefresh starts a background incremental jbcontext index of the checkout
// holding cwd, so jbcontext search answers for a recent HEAD. Only pi calls it:
// jbcontext setup-agent gives Claude its own SessionStart and SessionEnd index
// hooks and has none for pi. jbcontext itself skips a repository with no index.
// Off with ORCH_JB_REFRESH=off, and in read-only retrieval.
func jbRefresh(cwd string) {
	if os.Getenv("ORCH_JB_REFRESH") == "off" || os.Getenv("ORCH_CTX_READONLY") != "" || os.Getenv("ORCH_CTX_ROOT") != "" || !onPath("jbcontext") {
		return
	}
	root := gitRoot(cwd)
	if root == "" {
		return
	}
	dir := filepath.Join(StateDir(), "jbrefresh")
	if os.MkdirAll(dir, 0o777) != nil {
		return
	}
	sum := sha256.Sum256([]byte(root))
	stamp := filepath.Join(dir, hex.EncodeToString(sum[:8]))
	if info, err := os.Stat(stamp); err == nil && time.Since(info.ModTime()) < jbRefreshEvery {
		return
	}
	if os.WriteFile(stamp, []byte(root+"\n"), 0o644) != nil {
		return
	}
	jbRefreshStart(root)
}

// splitAlternatives splits a pattern on its top-level |.
func splitAlternatives(pattern string) []string {
	var out []string
	depth, from := 0, 0
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case '|':
			if depth == 0 {
				out = append(out, pattern[from:i])
				from = i + 1
			}
		}
	}
	return append(out, pattern[from:])
}

// ladderLog records that the lookup ladder reminder fired.
func ladderLog(harness, session, repo string) {
	if os.Getenv("ORCH_LOOKUP_LOG") == "off" {
		return
	}
	appendLine(LogPath(), LookupLine{
		TS: nowTS(), Harness: harness, Session: session, Repo: repo,
		Tool: "prompt", OK: true, Reminder: strPtr("lookup-ladder"),
	})
}

func nowTS() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z") }
