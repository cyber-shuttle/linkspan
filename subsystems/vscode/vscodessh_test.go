package vscode

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// TestVSCodeSSHServerLifecycle starts a real supervised server, checks it reports
// running, then stops it and confirms it is deregistered.
func TestVSCodeSSHServerLifecycle(t *testing.T) {
	s := StartSSHServerForVSCodeConnection("test-session", "127.0.0.1:0", "pw", "dummy-key")
	if s == nil {
		t.Fatal("failed to start SSH server")
	}
	t.Cleanup(func() { _ = stopSSHServerBySessionID("test-session") })

	if !waitFor(t, func() bool { st, e := getSessionStatus("test-session"); return e == nil && st.Active }) {
		t.Fatal("expected session to become active")
	}
	if err := stopSSHServerBySessionID("test-session"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !waitFor(t, func() bool { _, e := getSessionStatus("test-session"); return e != nil }) {
		t.Fatal("expected session to be deregistered after stop")
	}
}

// TestPanicIsolation verifies the recover wrappers contain panics rather than
// propagating them (which would crash the agent). Reaching the end means none escaped.
func TestPanicIsolation(t *testing.T) {
	started := make(chan struct{})
	safeGo("test", func() { close(started); panic("boom") })
	<-started
	time.Sleep(20 * time.Millisecond) // let the panic unwind and recover

	recoverChannelHandler("test", func(*ssh.Server, *gossh.ServerConn, gossh.NewChannel, ssh.Context) {
		panic("boom")
	})(nil, nil, nil, nil)

	rh := recoverRequestHandler("test", func(ssh.Context, *ssh.Server, *gossh.Request) (bool, []byte) {
		panic("boom")
	})
	if ok, payload := rh(nil, nil, nil); ok || payload != nil {
		t.Fatalf("expected (false, nil) on panic, got (%v, %v)", ok, payload)
	}
}

// TestSessionHandlerPanicIsolation verifies recoverSessionHandler contains a panic
// raised inside a session/subsystem handler body. gliderlabs/ssh runs the user
// Handler and SubsystemHandlers on a child goroutine (session.go dispatches them
// via `go func(){ handler(sess); ... }()`), which the channel-level recover cannot
// reach — so the recover must wrap the handler itself. An escaped panic here would
// crash the whole linkspan process.
func TestSessionHandlerPanicIsolation(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic escaped recoverSessionHandler (would crash linkspan): %v", r)
		}
	}()
	recoverSessionHandler("test", func(ssh.Session) { panic("boom") })(nil)
}

// TestSupervisorBoundedRetries: a listener that fails immediately is retried
// exactly maxConsecutiveFailures times, then marked failed and left registered.
func TestSupervisorBoundedRetries(t *testing.T) {
	tuning(t, 5, time.Millisecond, time.Hour)
	s, build, builds := failingServer(t, "bounded")

	s.run(build)

	if *builds != 5 || s.state != stateFailed || s.restarts != 5 || s.lastError == "" {
		t.Fatalf("got builds=%d state=%q restarts=%d err=%q", *builds, s.state, s.restarts, s.lastError)
	}
	if st, err := getSessionStatus("bounded"); err != nil || st.State != stateFailed || st.Active {
		t.Fatalf("failed session should stay registered & inactive: %+v err=%v", st, err)
	}
}

// TestSupervisorHealthyRunResetsCounter: a run ≥ healthyRunThreshold resets the
// rapid-failure counter. Attempt 5 reports a long run, so failure is deferred to
// attempt 9 instead of 5.
func TestSupervisorHealthyRunResetsCounter(t *testing.T) {
	tuning(t, 5, time.Millisecond, time.Hour)

	base := time.Unix(0, 0)
	var times []time.Time
	for i := 1; i <= 9; i++ {
		end := base
		if i == 5 {
			end = base.Add(2 * time.Hour) // healthy run resets the counter
		}
		times = append(times, base, end) // start, end per attempt
	}
	idx := 0
	old := nowFunc
	nowFunc = func() time.Time {
		if idx >= len(times) {
			return base
		}
		v := times[idx]
		idx++
		return v
	}
	t.Cleanup(func() { nowFunc = old })

	s, build, builds := failingServer(t, "reset")
	s.run(build)

	if *builds != 9 || s.state != stateFailed {
		t.Fatalf("expected 9 builds → failed, got builds=%d state=%q", *builds, s.state)
	}
}

// TestSupervisorStopHonored: the stop signal breaks the restart loop and
// deregisters the session even mid-backoff.
func TestSupervisorStopHonored(t *testing.T) {
	tuning(t, 1000, 5*time.Millisecond, time.Hour) // would loop ~forever without a stop
	s, build, _ := failingServer(t, "stoppable")

	done := make(chan struct{})
	go func() { s.run(build); close(done) }()

	time.Sleep(20 * time.Millisecond)
	_ = s.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop after Close()")
	}
	if s.state != stateStopped {
		t.Fatalf("expected state %q, got %q", stateStopped, s.state)
	}
	if _, err := getSessionStatus("stoppable"); err == nil {
		t.Fatal("expected session to be deregistered after stop")
	}
}

// TestNewServerWiring confirms the SERVER START options install every expected
// handler and callback — a guard against an option being dropped or mis-wired.
func TestNewServerWiring(t *testing.T) {
	srv := newServer(":0",
		onConnect(handleSession),
		withAuth("key", "pw"),
		withSubsystem("sftp", handleSFTP),
		withForwarding(),
		withKeepAlive(time.Second),
	)

	if srv.Handler == nil || srv.PublicKeyHandler == nil || srv.PasswordHandler == nil ||
		srv.ConnCallback == nil || srv.LocalPortForwardingCallback == nil || srv.ReversePortForwardingCallback == nil {
		t.Fatal("a server handler/callback was left unwired")
	}
	for _, k := range []string{"session", "direct-tcpip"} {
		if srv.ChannelHandlers[k] == nil {
			t.Fatalf("missing channel handler %q", k)
		}
	}
	for _, k := range []string{"tcpip-forward", "cancel-tcpip-forward"} {
		if srv.RequestHandlers[k] == nil {
			t.Fatalf("missing request handler %q", k)
		}
	}
	if srv.SubsystemHandlers["sftp"] == nil {
		t.Fatal("missing sftp subsystem handler")
	}
}

// TestRunHostCommandWiresStdio runs a real command through the shared host-command
// wiring (used by both the exec and shell-stdin paths) and confirms its stdout is
// written back to the session. Protects the de-duplicated stdio wiring.
func TestRunHostCommandWiresStdio(t *testing.T) {
	c := &captureSession{}
	runHostCommand(c, exec.Command("sh", "-c", "echo hello"))
	if got := c.String(); !strings.Contains(got, "hello") {
		t.Fatalf("expected command stdout written to the session, got %q", got)
	}
}

// --- helpers ---

// captureSession is a minimal ssh.Session that feeds no stdin (immediate EOF) and
// captures everything written to it. Stderr() returns nil to exercise the fallback
// that routes stderr to the session's main stream.
type captureSession struct {
	ssh.Session
	mu  sync.Mutex
	out bytes.Buffer
}

func (c *captureSession) Read([]byte) (int, error) { return 0, io.EOF }
func (c *captureSession) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(p)
}
func (c *captureSession) Stderr() io.ReadWriter { return nil }
func (c *captureSession) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

// failingServer registers a session whose build factory always yields a listener
// that fails immediately (invalid port), and returns a counter of build calls.
func failingServer(t *testing.T, id string) (*SSHServer, func() *ssh.Server, *int) {
	t.Helper()
	s := &SSHServer{state: stateRunning, sessionID: id, addr: "x", stopCh: make(chan struct{})}
	activeServersMu.Lock()
	activeServers[id] = s
	activeServersMu.Unlock()
	t.Cleanup(func() {
		activeServersMu.Lock()
		delete(activeServers, id)
		activeServersMu.Unlock()
	})
	n := new(int)
	return s, func() *ssh.Server { *n++; return &ssh.Server{Addr: "127.0.0.1:999999"} }, n
}

// tuning shrinks the supervisor timings for fast tests and restores them after.
func tuning(t *testing.T, maxFail int, backoff, healthy time.Duration) {
	t.Helper()
	mf, mn, mx, th := maxConsecutiveFailures, minRestartBackoff, maxRestartBackoff, healthyRunThreshold
	t.Cleanup(func() {
		maxConsecutiveFailures, minRestartBackoff, maxRestartBackoff, healthyRunThreshold = mf, mn, mx, th
	})
	maxConsecutiveFailures, minRestartBackoff, maxRestartBackoff, healthyRunThreshold = maxFail, backoff, backoff, healthy
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
