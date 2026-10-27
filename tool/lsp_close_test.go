package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startStubborn starts testdata/fake-stubborn.py through startLSP and
// handshakes, returning the client and the pid of the child the fake started.
func startStubborn(t *testing.T, exitOnExit bool) (*lspClient, int) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed for the fake language server")
	}
	fake, _ := filepath.Abs("testdata/fake-stubborn.py")
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_CHILD_PID", pidFile)
	if exitOnExit {
		t.Setenv("FAKE_EXIT_ON_EXIT", "1")
	} else {
		t.Setenv("FAKE_EXIT_ON_EXIT", "")
	}
	c, err := startLSP("python3", t.TempDir(), fake)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lspHandshake(c, t.TempDir(), nil); err != nil {
		c.close()
		t.Fatal(err)
	}
	// The handshake reply comes after the child's pid was written.
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return c, pid
}

func waitGone(t *testing.T, what string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still running after close", what, pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A server that ignores `exit`, and the child it started, are both gone after
// close: the whole process group is killed.
func TestLspCloseKillsServerAndItsChildren(t *testing.T) {
	c, child := startStubborn(t, false)
	server := c.cmd.Process.Pid
	c.close()
	waitGone(t, "server", server)
	waitGone(t, "child", child)
}

// A server that leaves on `exit` still has its children killed.
func TestLspCloseKillsChildrenOfAServerThatExited(t *testing.T) {
	c, child := startStubborn(t, true)
	c.close()
	waitGone(t, "child", child)
}

// A server that exits on `exit` is closed at once, not after the kill grace.
func TestLspCloseReturnsPromptlyWhenServerExits(t *testing.T) {
	c, _ := startStubborn(t, true)
	start := time.Now()
	c.close()
	if d := time.Since(start); d >= lspExitGrace {
		t.Errorf("close took %s, want under the %s grace", d, lspExitGrace)
	}
}
