package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The lsp daemon keeps language servers warm between `orch lsp` calls.
//
// One daemon per user listens on a unix socket (<cache>/lsp/daemon.sock, dir
// 0700, socket 0600). A request is one JSON object per connection (the verb's
// arguments, the caller's directory and environment) and the answer one JSON
// object (stdout, stderr, exit code), so the output is what the in-process
// path prints: the daemon runs the same lspExec, only the sessions come from a
// pool that outlives the call.
//
// Concurrency: requests are serialized by one mutex. The verb code reads the
// process environment and the working tree through package state, and one
// request at a time keeps both consistent; a request costs well under a second
// once the servers are warm, so queueing is cheap, and a server is never asked
// two things by two callers at once.
//
// Freshness: before a request runs, the pool compares the repo's dirty files
// and HEAD with what it last told the repo's servers, and sends
// workspace/didChangeWatchedFiles for the difference (see lspPool.begin).

const (
	lspProtocolVersion = 1
	lspLogLimit        = 5 << 20
	lspSockPathMax     = 100 // sun_path is 104 bytes on macOS, 108 on Linux
)

var (
	lspDaemonIdle  = 30 * time.Minute // the daemon exits with no request for this long
	lspRootIdle    = 15 * time.Minute // a repo untouched for this long loses its servers
	lspMaxRoots    = 4
	lspJanitorTick = 30 * time.Second
	lspStartWait   = 3 * time.Second
	lspCallLimit   = 5 * time.Minute

	// lspVersionFn is the build id the CLI and the daemon compare; tests replace it.
	lspVersionFn = lspBuildID
	// lspSpawn starts a detached daemon; tests replace it with an in-process one.
	lspSpawn = spawnLspDaemon
)

// activeLspPool is the pool of the request being run by the daemon, for code
// that opens servers outside lspRun (runTSServer). Nil outside the daemon.
var activeLspPool atomic.Pointer[lspPool]

type lspReq struct {
	Protocol int               `json:"protocol"`
	Version  string            `json:"version"`
	Cwd      string            `json:"cwd"`
	Argv     []string          `json:"argv"`
	Env      map[string]string `json:"env"`
}

type lspResp struct {
	Version  string `json:"version"`
	Mismatch bool   `json:"mismatch,omitempty"` // the daemon is another build and is leaving
	Fallback bool   `json:"fallback,omitempty"` // the daemon failed; run in process
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Exit     int    `json:"exit"`
}

// lspBuildID identifies this binary: its modification time and size, else the
// vcs revision it was built from.
func lspBuildID() string {
	if exe, err := os.Executable(); err == nil {
		if st, err := os.Stat(exe); err == nil {
			return fmt.Sprintf("%s-%d-%d", orchVersion, st.ModTime().UnixNano(), st.Size())
		}
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return orchVersion + "-" + s.Value
			}
		}
	}
	return orchVersion
}

// ---- paths ----

func lspCacheRoot() string {
	if r := os.Getenv("ORCH_CACHE_DIR"); r != "" {
		return r
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "orchestrate")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "orchestrate")
}

func lspLogPath() string { return filepath.Join(lspCacheRoot(), "lsp", "daemon.log") }

// lspSockDir is the private directory of the socket and its lock: <cache>/lsp,
// or, when that path is too long for a unix socket, a per-user directory under
// the temp dir. It is created and checked (owned by the user, mode 0700).
func lspSockDir() (string, error) {
	dir := filepath.Join(lspCacheRoot(), "lsp")
	if len(filepath.Join(dir, "daemon.sock")) > lspSockPathMax {
		sum := sha256.Sum256([]byte(dir))
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("orch-lsp-%d-%x", os.Getuid(), sum[:4]))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return "", errors.New("lsp socket directory is not owned by the user")
	}
	if st.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	if len(filepath.Join(dir, "daemon.sock")) > lspSockPathMax {
		return "", errors.New("lsp socket path is too long")
	}
	return dir, nil
}

// ---- client ----

func lspEnvKey(k string) bool {
	return strings.HasPrefix(k, "ORCH_") || k == "PATH" || k == "HOME" || k == "XDG_CONFIG_HOME"
}

func lspCallerEnv() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && lspEnvKey(k) {
			env[k] = v
		}
	}
	return env
}

func lspWantsDaemon(args []string) bool {
	if os.Getenv("ORCH_LSP_DAEMON") == "off" || len(args) == 0 {
		return false
	}
	known := false
	for _, v := range lspVerbs {
		known = known || v == args[0]
	}
	if !known {
		return false
	}
	for _, s := range args[1:] {
		if s == "--" {
			break
		}
		if s == "-h" || s == "--help" {
			return false
		}
	}
	return true
}

// lspViaDaemon answers a verb through the daemon. ok is false when the daemon
// could not be used and the caller must answer in process.
func lspViaDaemon(a *app, args []string) (error, bool) {
	resp, ok := lspCall(lspReq{Cwd: a.cwd, Argv: args}, true)
	if !ok {
		return nil, false
	}
	return lspReplay(a, resp), true
}

// lspReplay prints a response the way the in-process command would have.
func lspReplay(a *app, resp lspResp) error {
	io.WriteString(a.out, resp.Stdout)
	io.WriteString(a.errw, resp.Stderr)
	if resp.Exit != 0 {
		return &exitError{code: resp.Exit}
	}
	return nil
}

type lspDialError struct{ err error }

func (e lspDialError) Error() string { return e.err.Error() }

func lspRoundTrip(sock string, req lspReq) (lspResp, error) {
	var resp lspResp
	conn, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
	if err != nil {
		return resp, lspDialError{err}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(lspCallLimit))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return resp, err
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return resp, err
	}
	return resp, nil
}

// lspCall sends req to the daemon, starting one (or replacing one of another
// build) when start is set. It returns false when no usable answer came.
func lspCall(req lspReq, start bool) (lspResp, bool) {
	dir, err := lspSockDir()
	if err != nil {
		return lspResp{}, false
	}
	sock := filepath.Join(dir, "daemon.sock")
	req.Protocol, req.Version, req.Env = lspProtocolVersion, lspVersionFn(), lspCallerEnv()
	resp, err := lspRoundTrip(sock, req)
	var dial lspDialError
	switch {
	case err == nil && !resp.Mismatch:
		return resp, !resp.Fallback
	case err != nil && !errors.As(err, &dial):
		return lspResp{}, false // the daemon accepted the call and failed it
	case !start:
		return lspResp{}, false
	}
	if err := lspSpawn(); err != nil {
		return lspResp{}, false
	}
	for deadline := time.Now().Add(lspStartWait); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		resp, err = lspRoundTrip(sock, req)
		if err == nil && !resp.Mismatch {
			return resp, !resp.Fallback
		}
		if err != nil && !errors.As(err, &dial) {
			return lspResp{}, false
		}
	}
	return lspResp{}, false
}

// spawnLspDaemon starts `orch lsp serve` detached, with its output in the log
// (truncated when it passed 5 MB).
func spawnLspDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	log := lspLogPath()
	if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, err := os.Stat(log); err == nil && st.Size() > lspLogLimit {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	lf, err := os.OpenFile(log, flags, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd := exec.Command(exe, "lsp", "serve")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// cmdLspOp is warm, status and stop.
func cmdLspOp(a *app, args []string) error {
	for _, s := range args[1:] {
		if s == "-h" || s == "--help" {
			lspPrintHelp(a, args[0])
			return nil
		}
	}
	req := lspReq{Cwd: a.cwd, Argv: args}
	switch args[0] {
	case "warm":
		if len(args) > 2 {
			return &exitError{code: 2, msg: "usage: " + lspHelps["warm"].usage}
		}
		if len(args) == 2 {
			p := args[1]
			if !filepath.IsAbs(p) {
				p = filepath.Join(a.cwd, p)
			}
			if st, err := os.Stat(p); err != nil {
				return &exitError{code: 1, msg: "warm: " + err.Error()}
			} else if !st.IsDir() {
				p = filepath.Dir(p)
			}
			req.Cwd, req.Argv = p, []string{"warm"}
		}
	case "status":
		for _, s := range args[1:] {
			if s != "--json" {
				return &exitError{code: 2, msg: "usage: " + lspHelps["status"].usage}
			}
		}
	case "stop":
		if len(args) != 1 {
			return &exitError{code: 2, msg: "usage: " + lspHelps["stop"].usage}
		}
	}
	if os.Getenv("ORCH_LSP_DAEMON") == "off" {
		if args[0] == "warm" {
			return &exitError{code: 1, msg: "the lsp daemon is off (ORCH_LSP_DAEMON=off)"}
		}
		return a.noDaemon(args[0])
	}
	resp, ok := lspCall(req, args[0] == "warm")
	if !ok {
		if args[0] == "warm" {
			return &exitError{code: 1, msg: "the lsp daemon could not be started; see " + lspLogPath()}
		}
		return a.noDaemon(args[0])
	}
	return lspReplay(a, resp)
}

func (a *app) noDaemon(string) error {
	fmt.Fprintln(a.out, "no lsp daemon running")
	return nil
}

// ---- daemon ----

type lspDaemon struct {
	id   string
	sock string
	ln   net.Listener
	lock *os.File
	fi   os.FileInfo // the socket file this daemon created
	pool *lspPool

	runMu sync.Mutex // serializes requests (see the package comment)

	mu       sync.Mutex
	closing  bool
	lastReq  time.Time
	started  time.Time
	wg       sync.WaitGroup
	done     chan struct{}
	stopOnce sync.Once
}

var errLspDaemonRunning = errors.New("an lsp daemon is already running")

// startLspDaemon takes the lock, listens on dir/daemon.sock and serves in the
// background. The socket exists when it returns.
func startLspDaemon(dir, id string) (*lspDaemon, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockf, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// A daemon of another build gives the lock up when it is told to leave.
	var lockErr error
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if lockErr = syscall.Flock(int(lockf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr == nil || !time.Now().Before(deadline) {
			break
		}
	}
	if lockErr != nil {
		lockf.Close()
		return nil, errLspDaemonRunning
	}
	sock := filepath.Join(dir, "daemon.sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		lockf.Close()
		return nil, err
	}
	_ = os.Chmod(sock, 0o600)
	fi, _ := os.Stat(sock)
	maxRoots := lspMaxRoots
	if n, err := strconv.Atoi(os.Getenv("ORCH_LSP_MAX_ROOTS")); err == nil && n > 0 {
		maxRoots = n
	}
	d := &lspDaemon{id: id, sock: sock, ln: ln, lock: lockf, fi: fi, pool: newLspPool(maxRoots),
		lastReq: time.Now(), started: time.Now(), done: make(chan struct{})}
	d.wg.Add(1)
	go d.accept()
	go d.janitor()
	return d, nil
}

func (d *lspDaemon) accept() {
	defer d.wg.Done()
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		if d.closing {
			d.mu.Unlock()
			conn.Close()
			continue
		}
		d.wg.Add(1)
		d.lastReq = time.Now()
		d.mu.Unlock()
		go func() {
			defer d.wg.Done()
			defer conn.Close()
			d.handle(conn)
		}()
	}
}

// shutdown stops listening, gives the socket and the lock up at once (so a
// successor can start), finishes in-flight requests and closes the servers.
func (d *lspDaemon) shutdown() {
	d.stopOnce.Do(func() {
		d.mu.Lock()
		d.closing = true
		d.mu.Unlock()
		if st, err := os.Stat(d.sock); err == nil && d.fi != nil && os.SameFile(st, d.fi) {
			_ = os.Remove(d.sock)
		}
		_ = d.ln.Close()
		_ = syscall.Flock(int(d.lock.Fd()), syscall.LOCK_UN)
		_ = d.lock.Close()
		d.wg.Wait()
		d.runMu.Lock()
		d.pool.closeAll()
		d.runMu.Unlock()
		close(d.done)
	})
}

func (d *lspDaemon) janitor() {
	t := time.NewTicker(lspJanitorTick)
	defer t.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-t.C:
		}
		d.mu.Lock()
		idle := time.Since(d.lastReq) > lspDaemonIdle
		d.mu.Unlock()
		if idle && d.runMu.TryLock() {
			d.runMu.Unlock()
			go d.shutdown()
			return
		}
		if d.runMu.TryLock() {
			d.pool.closeIdle(lspRootIdle)
			d.runMu.Unlock()
		}
	}
}

func (d *lspDaemon) touch() {
	d.mu.Lock()
	d.lastReq = time.Now()
	d.mu.Unlock()
}

func (d *lspDaemon) handle(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var req lspReq
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	resp := lspResp{Version: d.id}
	op := ""
	if len(req.Argv) > 0 {
		op = req.Argv[0]
	}
	switch {
	case req.Protocol != lspProtocolVersion || (op != "status" && op != "stop" && req.Version != d.id):
		resp.Mismatch = true
		_ = json.NewEncoder(conn).Encode(resp)
		go d.shutdown()
		return
	}
	resp = d.run(req)
	resp.Version = d.id
	_ = json.NewEncoder(conn).Encode(resp)
	if op == "stop" {
		go d.shutdown()
	}
	d.touch()
}

// applyEnv makes the daemon's environment the caller's, for the variables the
// verbs read.
func applyLspEnv(env map[string]string) {
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && lspEnvKey(k) {
			if _, keep := env[k]; !keep {
				os.Unsetenv(k)
			}
		}
	}
	for k, v := range env {
		if lspEnvKey(k) {
			os.Setenv(k, v)
		}
	}
}

func (d *lspDaemon) run(req lspReq) (resp lspResp) {
	var out, errb bytes.Buffer
	a := &app{cwd: req.Cwd, stdin: strings.NewReader(""), out: &out, errw: &errb, now: time.Now}
	op := ""
	if len(req.Argv) > 0 {
		op = req.Argv[0]
	}
	if op == "status" {
		// Read-only and quick: it does not wait for a running request.
		err := d.status(a, req.Argv[1:])
		resp.Exit = a.report(err)
		resp.Stdout, resp.Stderr = out.String(), errb.String()
		return resp
	}
	if op == "stop" {
		fmt.Fprintln(a.out, "lsp daemon stopped")
		resp.Stdout = out.String()
		return resp
	}
	d.runMu.Lock()
	defer d.runMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "lsp daemon: panic serving %v: %v\n%s", req.Argv, r, debug.Stack())
			resp = lspResp{Fallback: true}
		}
	}()
	applyLspEnv(req.Env)
	activeLspPool.Store(d.pool)
	defer activeLspPool.Store(nil)
	var err error
	if op == "warm" {
		err = d.warm(a, req)
	} else {
		err = lspExec(a, req.Argv, d.pool)
	}
	resp.Exit = a.report(err)
	resp.Stdout, resp.Stderr = out.String(), errb.String()
	return resp
}

// warm resolves the repo now and opens its servers in the background.
func (d *lspDaemon) warm(a *app, req lspReq) error {
	e, err := codeEnvFor(a)
	if err != nil {
		return err
	}
	d.pool.begin(e)
	type job struct {
		l   *langDef
		dir string
	}
	var jobs []job
	var names []string
	for _, l := range allLangs() {
		if !repoHasLang(e.Code, l.ID) {
			continue
		}
		names = append(names, l.ID)
		switch {
		case l.native() && l.ID == "go":
			for _, dir := range e.goModuleDirs() {
				jobs = append(jobs, job{l, dir})
			}
		case l.native() && l.ID == "typescript":
			jobs = append(jobs, job{l, e.Code})
		default:
			for _, r := range rootsOf(e.Code, l.ID) {
				jobs = append(jobs, job{l, filepath.Join(e.Code, filepath.FromSlash(strings.TrimSuffix(r.Dir, "/")))})
			}
		}
	}
	fmt.Fprintf(a.out, "warming %s (%s)\n", e.Code, strings.Join(names, ", "))
	go func() {
		d.runMu.Lock()
		applyLspEnv(req.Env)
		activeLspPool.Store(d.pool)
		var started []*lspSession
		for _, j := range jobs {
			if s, _ := d.pool.session(e, j.l, j.dir); s != nil {
				started = append(started, s)
			}
		}
		activeLspPool.Store(nil)
		d.runMu.Unlock()
		// A started server has not loaded the workspace yet (gopls loads its
		// packages on the first query, seconds on a large repo), so one
		// throwaway symbol query makes the first real lookup fast. It runs
		// outside runMu so lookups in other roots are not held up.
		for _, s := range started {
			if s.caps.WS {
				go s.c.call("workspace/symbol", map[string]any{"query": "orchWarmPrime"}, 2*time.Minute)
			}
		}
	}()
	return nil
}

// ---- status ----

type lspSessionStatus struct {
	Lang  string `json:"lang"`
	Bin   string `json:"server"`
	Pid   int    `json:"pid"`
	RSSKB int    `json:"rss_kb"`
}

type lspRootStatus struct {
	Repo     string             `json:"repo"`
	IdleSec  int                `json:"idle_s"`
	SyncSec  int                `json:"last_sync_s"` // -1: never
	Head     string             `json:"head,omitempty"`
	Sessions []lspSessionStatus `json:"sessions"`
}

type lspStatus struct {
	Pid     int             `json:"pid"`
	RSSKB   int             `json:"rss_kb"`
	Version string          `json:"version"`
	UpSec   int             `json:"up_s"`
	IdleSec int             `json:"idle_s"`
	Roots   []lspRootStatus `json:"roots"`
}

func lspRSS(pid int) int {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func (d *lspDaemon) status(a *app, args []string) error {
	d.mu.Lock()
	st := lspStatus{Pid: os.Getpid(), Version: d.id, UpSec: int(time.Since(d.started).Seconds()), IdleSec: int(time.Since(d.lastReq).Seconds())}
	d.mu.Unlock()
	st.RSSKB = lspRSS(st.Pid)
	st.Roots = d.pool.status()
	if len(args) > 0 {
		return json.NewEncoder(a.out).Encode(st)
	}
	fmt.Fprintf(a.out, "daemon pid %d rss %s up %s idle %s version %s\n", st.Pid, lspMB(st.RSSKB), lspDur(st.UpSec), lspDur(st.IdleSec), st.Version)
	if len(st.Roots) == 0 {
		fmt.Fprintln(a.out, "no live repos")
	}
	for _, r := range st.Roots {
		sync := "never"
		if r.SyncSec >= 0 {
			sync = lspDur(r.SyncSec) + " ago"
		}
		fmt.Fprintf(a.out, "%s  idle %s  last sync %s  head %s\n", r.Repo, lspDur(r.IdleSec), sync, r.Head)
		for _, s := range r.Sessions {
			fmt.Fprintf(a.out, "  %-10s %s pid %d rss %s\n", s.Lang, s.Bin, s.Pid, lspMB(s.RSSKB))
		}
	}
	return nil
}

func lspMB(kb int) string { return fmt.Sprintf("%.1f MB", float64(kb)/1024) }

func lspDur(sec int) string {
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%dh%dm", sec/3600, sec%3600/60)
	case sec >= 60:
		return fmt.Sprintf("%dm%ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%ds", sec)
}

// cmdLspServe is `orch lsp serve`: the daemon itself.
func cmdLspServe(a *app, args []string) error {
	if len(args) > 0 {
		return &exitError{code: 2, msg: "usage: orch lsp serve (started by orch lsp on demand)"}
	}
	dir, err := lspSockDir()
	if err != nil {
		return err
	}
	d, err := startLspDaemon(dir, lspVersionFn())
	if errors.Is(err, errLspDaemonRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s lsp daemon pid %d version %s listening on %s\n", time.Now().Format(time.RFC3339), os.Getpid(), d.id, d.sock)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		d.shutdown()
	}()
	<-d.done
	return nil
}

// ---- pool ----

type lspPooled struct {
	s     *lspSession
	lang  string
	roots []string
}

type lspPoolRoot struct {
	sessions map[string]*lspPooled
	last     time.Time
	snap     *lspSnap
	synced   time.Time
}

// lspPool is the daemon's servers by repo and (language, server, project
// roots), at most max repos at once (the least recently used one is closed to
// make room).
type lspPool struct {
	mu    sync.Mutex
	max   int
	roots map[string]*lspPoolRoot
}

func newLspPool(max int) *lspPool { return &lspPool{max: max, roots: map[string]*lspPoolRoot{}} }

func (c *lspClient) alive() bool {
	select {
	case <-c.dead:
		return false
	default:
		return true
	}
}

// forget closes a document the server was told about, so it reads the file
// from disk again (an open document's text is the server's truth, not the file).
func (c *lspClient) forget(path string) {
	c.openedMu.Lock()
	was := c.opened[path]
	delete(c.opened, path)
	c.openedMu.Unlock()
	c.forgetDiags(path)
	if was {
		c.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": fileURI(path)}})
	}
}

// begin runs at the start of every request on a repo: it marks the repo used
// (making room if it is new) and brings its servers up to date with the files.
func (p *lspPool) begin(e *codeEnv) {
	repo := e.Code
	p.mu.Lock()
	root := p.roots[repo]
	if root == nil {
		for len(p.roots) >= p.max {
			p.evictLocked(p.lruLocked())
		}
		root = &lspPoolRoot{sessions: map[string]*lspPooled{}}
		p.roots[repo] = root
	}
	root.last = time.Now()
	p.mu.Unlock()

	snap := takeLspSnap(repo)
	p.mu.Lock()
	defer p.mu.Unlock()
	old := root.snap
	root.snap = snap
	root.synced = time.Now()
	if old == nil {
		return
	}
	changes := diffLspSnaps(repo, old, snap)
	if len(changes) == 0 {
		return
	}
	for _, ps := range root.sessions {
		ps.sync(repo, changes)
	}
}

// sync tells one server about the changes that concern its language.
func (ps *lspPooled) sync(repo string, changes map[string]int) {
	var list []map[string]any
	for rel, kind := range changes {
		if !lspWatched(ps.lang, rel) {
			continue
		}
		abs := filepath.Join(repo, filepath.FromSlash(rel))
		ps.s.c.forget(abs)
		list = append(list, map[string]any{"uri": fileURI(abs), "type": kind})
	}
	if len(list) > 0 {
		ps.s.c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": list})
	}
}

func (p *lspPool) lruLocked() string {
	best, bt := "", time.Time{}
	for k, r := range p.roots {
		if best == "" || r.last.Before(bt) {
			best, bt = k, r.last
		}
	}
	return best
}

func (p *lspPool) evictLocked(repo string) {
	if r := p.roots[repo]; r != nil {
		for _, ps := range r.sessions {
			ps.s.c.close()
		}
		delete(p.roots, repo)
	}
}

func (p *lspPool) closeIdle(idle time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, r := range p.roots {
		if time.Since(r.last) > idle {
			p.evictLocked(k)
		}
	}
}

func (p *lspPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.roots {
		p.evictLocked(k)
	}
}

// tsAllRoots are the TypeScript project roots one server serves for a repo.
func tsAllRoots(code string, extra ...string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, r := range rootsOf(code, "typescript") {
		add(filepath.Join(code, filepath.FromSlash(strings.TrimSuffix(r.Dir, "/"))))
	}
	for _, x := range extra {
		add(x)
	}
	sort.Strings(out)
	return out
}

// session returns the warm server of language l for the project root dir of
// the repo e, starting it when there is none (or it died).
func (p *lspPool) session(e *codeEnv, l *langDef, dir string) (*lspSession, string) {
	if l.native() && l.ID == "typescript" {
		return p.tsSession(e.Code, dir)
	}
	srv := serverFor(e, l)
	if g, ok := srv.(goplsServer); ok {
		g.remote = true
		srv = g
	}
	return p.get(e, l.ID, srv, dir, []string{dir})
}

func (p *lspPool) tsSession(code string, extra ...string) (*lspSession, string) {
	roots := tsAllRoots(code, extra...)
	e := &codeEnv{Code: code}
	return p.get(e, "typescript", tsServer{}, roots[0], roots)
}

func (p *lspPool) get(e *codeEnv, lang string, srv lspServer, root string, roots []string) (*lspSession, string) {
	cands, _ := srv.commands(e.Code)
	bin := ""
	if len(cands) > 0 {
		bin = cands[0].Bin + " " + strings.Join(cands[0].Args, " ")
	}
	key := lang + "\x00" + bin + "\x00" + strings.Join(roots, "\x00")
	p.mu.Lock()
	pr := p.roots[e.Code]
	if pr == nil {
		pr = &lspPoolRoot{sessions: map[string]*lspPooled{}, last: time.Now()}
		p.roots[e.Code] = pr
	}
	if ps := pr.sessions[key]; ps != nil {
		if ps.s.c.alive() {
			p.mu.Unlock()
			return ps.s, ""
		}
		delete(pr.sessions, key)
	}
	p.mu.Unlock()
	s, why := openServer(e.Code, srv, root, roots)
	if s == nil {
		return nil, why
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pr.sessions[key] = &lspPooled{s: s, lang: lang, roots: roots}
	return s, ""
}

func (p *lspPool) status() []lspRootStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []lspRootStatus
	for repo, r := range p.roots {
		rs := lspRootStatus{Repo: repo, IdleSec: int(time.Since(r.last).Seconds()), SyncSec: -1, Sessions: []lspSessionStatus{}}
		if !r.synced.IsZero() {
			rs.SyncSec = int(time.Since(r.synced).Seconds())
		}
		if r.snap != nil {
			rs.Head = r.snap.head[:min(7, len(r.snap.head))]
		}
		for _, ps := range r.sessions {
			pid := ps.s.c.cmd.Process.Pid
			rs.Sessions = append(rs.Sessions, lspSessionStatus{Lang: ps.lang, Bin: ps.s.bin, Pid: pid, RSSKB: lspRSS(pid)})
		}
		sort.Slice(rs.Sessions, func(i, j int) bool { return rs.Sessions[i].Lang < rs.Sessions[j].Lang })
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

// ---- freshness ----

type lspFileState struct {
	mtime, size int64
	missing     bool
	untracked   bool
}

// lspSnap is the part of a checkout a server can be stale about: HEAD and
// every dirty or untracked file (git status, so ignored files are left out).
type lspSnap struct {
	head  string
	files map[string]lspFileState
}

// lspWatched says whether a server of the language cares about the repo-relative
// path: a source file of the language, or its project manifest.
func lspWatched(lang, rel string) bool {
	if l := langOf(rel); l != nil && l.ID == lang {
		return true
	}
	base := filepath.Base(rel)
	switch lang {
	case "go":
		return lspManifest(base) && !strings.HasSuffix(base, ".json")
	case "typescript":
		return lspManifest(base) && strings.HasSuffix(base, ".json")
	}
	return false
}

// lspManifest is a file name a project is configured by (Go or TypeScript).
func lspManifest(base string) bool {
	return base == "go.mod" || base == "go.sum" || base == "go.work" || base == "package.json" ||
		strings.HasPrefix(base, "tsconfig") && strings.HasSuffix(base, ".json")
}

func takeLspSnap(repo string) *lspSnap {
	snap := &lspSnap{files: map[string]lspFileState{}}
	var wg sync.WaitGroup
	var status string
	wg.Add(2)
	go func() { defer wg.Done(); snap.head, _ = git(repo, "rev-parse", "HEAD") }()
	go func() {
		defer wg.Done()
		cmd := exec.Command("git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
		cmd.Dir = repo
		out, _ := cmd.Output()
		status = string(out)
	}()
	wg.Wait()
	toks := strings.Split(status, "\x00")
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if len(t) < 4 {
			continue
		}
		xy, path := t[:2], t[3:]
		if xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C' {
			// "R  new\0old": the old name is a deletion.
			if i+1 < len(toks) {
				i++
				snap.files[toks[i]] = lspFileState{missing: true}
			}
		}
		if langOf(path) == nil && !lspManifest(filepath.Base(path)) {
			continue
		}
		st := lspFileState{untracked: xy == "??"}
		if fi, err := os.Stat(filepath.Join(repo, filepath.FromSlash(path))); err != nil {
			st.missing = true
		} else {
			st.mtime, st.size = fi.ModTime().UnixNano(), fi.Size()
		}
		snap.files[path] = st
	}
	return snap
}

const (
	lspCreated = 1
	lspChanged = 2
	lspDeleted = 3
)

// diffLspSnaps lists the repo-relative paths that differ between two snapshots
// with the change a watcher would report. A file dirty before and clean now is
// changed; a HEAD move adds what git says changed between the two commits.
func diffLspSnaps(repo string, old, cur *lspSnap) map[string]int {
	created := map[string]bool{}
	paths := map[string]bool{}
	for p, n := range cur.files {
		o, ok := old.files[p]
		if !ok || o != n {
			paths[p] = true
			created[p] = n.untracked && !ok
		}
	}
	for p := range old.files {
		if _, ok := cur.files[p]; !ok {
			paths[p] = true
		}
	}
	if old.head != cur.head && old.head != "" && cur.head != "" {
		cmd := exec.Command("git", "diff", "--name-status", "--no-renames", "-z", old.head, cur.head)
		cmd.Dir = repo
		if out, err := cmd.Output(); err == nil {
			toks := strings.Split(string(out), "\x00")
			for i := 0; i+1 < len(toks); i += 2 {
				paths[toks[i+1]] = true
				if toks[i] == "A" {
					created[toks[i+1]] = true
				}
			}
		}
	}
	out := map[string]int{}
	for p := range paths {
		switch _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(p))); {
		case err != nil:
			out[p] = lspDeleted
		case created[p]:
			out[p] = lspCreated
		default:
			out[p] = lspChanged
		}
	}
	return out
}
