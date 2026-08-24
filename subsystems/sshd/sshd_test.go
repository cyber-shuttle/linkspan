package sshd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

func TestSSHServerLifecycle(t *testing.T) {
	_, key := testKeyPair(t)
	id, port, err := Start(key)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = stopByID(id) })

	// Start binds before returning, so the port is already accepting and the id
	// names it -- the caller is never handed a port that nothing listens on.
	if id != fmt.Sprintf("s-%d", port) {
		t.Fatalf("id %q does not name port %d", id, port)
	}
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("nothing accepting on the port Start returned: %v", err)
	}
	_ = conn.Close()

	if !waitFor(t, func() bool { st, ok := statusOf(id); return ok && st.State == stateRunning }) {
		t.Fatal("expected session to become active")
	}
	if err := stopByID(id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !waitFor(t, func() bool { _, ok := statusOf(id); return !ok }) {
		t.Fatal("expected session to be deregistered after stop")
	}
}

// Reaching the end means no panic escaped.
func TestPanicIsolation(t *testing.T) {
	started := make(chan struct{})
	safeGo("test", func() { close(started); panic("boom") })
	<-started
	time.Sleep(20 * time.Millisecond) // let the panic unwind and recover

	guardChannel("test", func(*ssh.Server, *gossh.ServerConn, gossh.NewChannel, ssh.Context) {
		panic("boom")
	})(nil, nil, nil, nil)

	rh := guardRequest("test", func(ssh.Context, *ssh.Server, *gossh.Request) (bool, []byte) {
		panic("boom")
	})
	if ok, payload := rh(nil, nil, nil); ok || payload != nil {
		t.Fatalf("expected (false, nil) on panic, got (%v, %v)", ok, payload)
	}
}

// gliderlabs dispatches these on a child goroutine that a channel-level recover
// cannot reach, so an escaped panic here would crash linkspan.
func TestSessionHandlerPanicIsolation(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic escaped guardSession (would crash linkspan): %v", r)
		}
	}()
	guardSession("test", func(ssh.Session) { panic("boom") })(nil)
}

func TestSupervisorBoundedRetries(t *testing.T) {
	tuning(t, 5, time.Millisecond, time.Hour)
	s, build, builds := failingServer(t, "bounded")

	s.run(build, nil)

	if *builds != 5 || s.state != stateFailed {
		t.Fatalf("got builds=%d state=%q", *builds, s.state)
	}
	if st, ok := statusOf("bounded"); !ok || st.State != stateFailed {
		t.Fatalf("failed session should stay registered & inactive: %+v", st)
	}
}

// Attempt 5 reports a long run, so failure defers to attempt 9 instead of 5.
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
	s.run(build, nil)

	if *builds != 9 || s.state != stateFailed {
		t.Fatalf("expected 9 builds → failed, got builds=%d state=%q", *builds, s.state)
	}
}

func TestSupervisorStopHonored(t *testing.T) {
	tuning(t, 1000, 5*time.Millisecond, time.Hour) // would loop ~forever without a stop
	s, build, _ := failingServer(t, "stoppable")

	done := make(chan struct{})
	go func() { s.run(build, nil); close(done) }()

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
	if _, ok := statusOf("stoppable"); ok {
		t.Fatal("expected session to be deregistered after stop")
	}
}

// sftp and streamlocal have no cs-bridge caller to notice they went missing.
func TestNewServerWiring(t *testing.T) {
	_, key := testKeyPair(t)
	srv := newServer(key)

	if srv.Handler == nil || srv.PublicKeyHandler == nil || srv.PasswordHandler != nil ||
		srv.ConnCallback == nil || srv.LocalPortForwardingCallback == nil || srv.ReversePortForwardingCallback == nil {
		t.Fatal("a server handler/callback was left unwired")
	}
	for _, k := range []string{"session", "direct-tcpip", "direct-streamlocal@openssh.com"} {
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

// The path VS Code's remoteServerListenOnSocket mode depends on.
func TestDirectStreamLocalForwarding(t *testing.T) {
	dir, err := os.MkdirTemp("", "sl") // not t.TempDir: macOS caps socket paths at 104 chars
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ul, err := net.Listen("unix", filepath.Join(dir, "e.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	go func() {
		if c, err := ul.Accept(); err == nil {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}
	}()

	signer, authorizedKey := testKeyPair(t)
	srv := newServer(authorizedKey)
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(tl) }()
	defer srv.Close()

	client, err := gossh.Dial("tcp", tl.Addr().String(), &gossh.ClientConfig{
		User: "t", Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)}, HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ch, reqs, err := client.OpenChannel("direct-streamlocal@openssh.com",
		gossh.Marshal(streamLocalChannelData{SocketPath: ul.Addr().String()}))
	if err != nil {
		t.Fatal(err)
	}
	go gossh.DiscardRequests(reqs)
	defer ch.Close()

	buf := make([]byte, 4)
	if _, err := ch.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(ch, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo round-trip failed: err=%v got=%q", err, buf)
	}
}

func TestRunHostCommandWiresStdio(t *testing.T) {
	c := &captureSession{}
	runHostCommand(c, exec.Command("sh", "-c", "echo hello"))
	if got := c.String(); !strings.Contains(got, "hello") {
		t.Fatalf("expected command stdout written to the session, got %q", got)
	}
	if c.exitCode() != 0 {
		t.Fatalf("exit status = %d, want 0", c.exitCode())
	}
}

// gliderlabs sends 0 for any session whose handler just returns, so a failing
// command would look successful to the client unless we send the real status.
func TestRunHostCommandReportsExitStatus(t *testing.T) {
	c := &captureSession{}
	runHostCommand(c, exec.Command("sh", "-c", "exit 42"))
	if c.exitCode() != 42 {
		t.Fatalf("exit status = %d, want 42", c.exitCode())
	}
	if got := c.String(); strings.Contains(got, "command error") {
		t.Fatalf("a failing command must not write diagnostics to stdout, got %q", got)
	}
}

// A client that keeps its own stdin open never sends EOF. The command has still
// finished, so the session must end anyway.
func TestRunHostCommandReturnsWhileStdinIsStillOpen(t *testing.T) {
	c := &blockingStdinSession{release: make(chan struct{})}
	defer close(c.release)

	done := make(chan struct{})
	go func() { defer close(done); runHostCommand(c, exec.Command("sh", "-c", "echo hi")) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runHostCommand blocked on a client that never closed its stdin")
	}
	if c.exitCode() != 0 {
		t.Fatalf("exit status = %d, want 0", c.exitCode())
	}
}

type blockingStdinSession struct {
	captureSession
	release chan struct{}
}

func (b *blockingStdinSession) Read([]byte) (int, error) { <-b.release; return 0, io.EOF }

func TestRunHostCommandReportsAnUnrunnableCommand(t *testing.T) {
	c := &captureSession{}
	runHostCommand(c, exec.Command("/nonexistent/binary"))
	if c.exitCode() != 127 {
		t.Fatalf("exit status = %d, want 127", c.exitCode())
	}
}

type captureSession struct {
	ssh.Session
	mu   sync.Mutex
	out  bytes.Buffer
	code int
}

func (c *captureSession) Read([]byte) (int, error) { return 0, io.EOF }
func (c *captureSession) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(p)
}
func (c *captureSession) Stderr() io.ReadWriter { return c }
func (c *captureSession) Exit(code int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.code = code
	return nil
}
func (c *captureSession) exitCode() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code
}
func (c *captureSession) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

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

func testKeyPair(t *testing.T) (gossh.Signer, ssh.PublicKey) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer, signer.PublicKey()
}

// Test-only registry lookups: production needs only what the two routes use.
func statusOf(id string) (*SessionStatus, bool) {
	for _, s := range Statuses() {
		if s.ID == id {
			return s, true
		}
	}
	return nil, false
}

func stopByID(id string) error {
	server, ok := deleteServer(id)
	if !ok {
		return fmt.Errorf("no ssh server found for session %s", id)
	}
	return server.Close()
}
