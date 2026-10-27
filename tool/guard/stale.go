package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// buildBackoff is how long a failed build of one source state is not retried.
const buildBackoff = 10 * time.Minute

// failedRecently reports whether a build of this fingerprint failed within the
// backoff. The record is "<fingerprint> <unix seconds>" in guard-build-failed,
// written by the build wrapper when the script exits non-zero.
func failedRecently(path, fingerprint string, now time.Time) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	fp, at, ok := strings.Cut(strings.TrimSpace(string(data)), " ")
	secs, err := strconv.ParseInt(at, 10, 64)
	return ok && err == nil && fp == fingerprint && now.Sub(time.Unix(secs, 0)) < buildBackoff
}

// CheckStale compares the source under sourceDir with the fingerprint the
// binary was built from. When they differ it starts scripts/build-guard in the
// background and returns at once: a stale binary still runs its own logic and
// is replaced for the next call. One rebuild runs at a time, held by a flock on
// guard-build.lock in the state dir that the build process inherits, so it
// frees itself when the build ends or dies. The build output is appended to
// guard-build.log. A build that fails is not retried for the same sources for
// ten minutes, so a broken tree does not start a build on every call. A missing
// sourceDir skips the check.
func CheckStale(fingerprint, sourceDir string) {
	if sourceDir == "" {
		return
	}
	script := filepath.Join(sourceDir, "scripts", "build-guard")
	if _, err := os.Stat(script); err != nil {
		return
	}
	now, err := Fingerprint(filepath.Join(sourceDir, "tool"))
	if err != nil || now == fingerprint {
		return
	}
	dir := StateDir()
	if os.MkdirAll(dir, 0o777) != nil {
		return
	}
	failed := filepath.Join(dir, "guard-build-failed")
	if failedRecently(failed, now, time.Now()) {
		return
	}
	lock, err := os.OpenFile(filepath.Join(dir, "guard-build.lock"), os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return
	}
	out, err := os.OpenFile(filepath.Join(dir, "guard-build.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer out.Close()
	// The wrapper records a failure; the script runs as $0 with the lock inherited.
	cmd := exec.Command("/bin/sh", "-c", `"$0" || printf '%s %s\n' "$1" "$(date +%s)" > "$2"`, script, now, failed)
	cmd.Dir = sourceDir
	cmd.Stdout, cmd.Stderr = out, out
	cmd.ExtraFiles = []*os.File{lock}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}
