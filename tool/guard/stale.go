package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// CheckStale compares the source under sourceDir with the fingerprint the
// binary was built from. When they differ it starts scripts/build-guard in the
// background and returns at once: a stale binary still runs its own logic and
// is replaced for the next call. One rebuild runs at a time, held by a flock on
// guard-build.lock in the state dir that the build process inherits, so it
// frees itself when the build ends or dies. The build output is appended to
// guard-build.log. A missing sourceDir skips the check.
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
	cmd := exec.Command(script)
	cmd.Dir = sourceDir
	cmd.Stdout, cmd.Stderr = out, out
	cmd.ExtraFiles = []*os.File{lock}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}
