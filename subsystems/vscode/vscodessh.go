package vscode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"runtime/debug"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// SSH server for VS Code sessions, organized as two blocks — SERVER START and
// CLIENT CONNECT — over shared building blocks. Resilience invariant: every
// handler boundary is panic-isolated and the supervisor restarts the listener
// after a fatal exit, so one bad connection can never take linkspan down.

// ═══════════════════════════ SERVER START ═══════════════════════════

// StartSSHServerForVSCodeConnection starts a supervised, auto-restarting SSH
// server for a session and registers it for later shutdown.
func StartSSHServerForVSCodeConnection(sessionID, addr string, password string, publicKey string) *SSHServer {
	return supervise(sessionID, addr, func() *ssh.Server {
		return newServer(addr,
			onConnect(handleSession),
			withAuth(publicKey, password),
			withSubsystem("sftp", handleSFTP),
			withForwarding(),
			withKeepAlive(30*time.Second),
		)
	})
}

type serverOption func(*ssh.Server)

// newServer runs per (re)start, so each restart gets fresh per-server state.
func newServer(addr string, opts ...serverOption) *ssh.Server {
	srv := &ssh.Server{
		Addr: addr,
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session": recoverChannelHandler("session", ssh.DefaultSessionHandler),
		},
		RequestHandlers:   map[string]ssh.RequestHandler{},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{},
	}
	for _, opt := range opts {
		opt(srv)
	}
	return srv
}

func onConnect(h func(ssh.Session)) serverOption {
	return func(srv *ssh.Server) { srv.Handler = recoverSessionHandler("session", h) }
}

func withAuth(publicKey, password string) serverOption {
	return func(srv *ssh.Server) {
		srv.PublicKeyHandler = newPublicKeyHandler(publicKey)
		srv.PasswordHandler = func(ctx ssh.Context, pass string) bool { return pass == password }
	}
}

func withSubsystem(name string, h func(ssh.Session)) serverOption {
	return func(srv *ssh.Server) { srv.SubsystemHandlers[name] = recoverSessionHandler(name, h) }
}

// withForwarding allows client local (direct-tcpip) and reverse (tcpip-forward)
// port forwards. ForwardedTCPHandler holds per-server state, so it is per build.
func withForwarding() serverOption {
	return func(srv *ssh.Server) {
		fwd := &ssh.ForwardedTCPHandler{}
		srv.ChannelHandlers["direct-tcpip"] = recoverChannelHandler("direct-tcpip", ssh.DirectTCPIPHandler)
		srv.LocalPortForwardingCallback = func(ctx ssh.Context, dhost string, dport uint32) bool {
			log.Printf("local port forwarding requested: host=%s port=%d", dhost, dport)
			return true
		}
		srv.ReversePortForwardingCallback = func(ctx ssh.Context, bindHost string, bindPort uint32) bool {
			log.Printf("reverse port forwarding requested: host=%s port=%d", bindHost, bindPort)
			return true
		}
		srv.RequestHandlers["tcpip-forward"] = recoverRequestHandler("tcpip-forward", fwd.HandleSSHRequest)
		srv.RequestHandlers["cancel-tcpip-forward"] = recoverRequestHandler("cancel-tcpip-forward", fwd.HandleSSHRequest)
	}
}

// withKeepAlive keeps a quiet session alive at the TCP layer; IdleTimeout and
// MaxTimeout deliberately stay unset (0).
func withKeepAlive(period time.Duration) serverOption {
	return func(srv *ssh.Server) {
		srv.ConnCallback = func(ctx ssh.Context, conn net.Conn) net.Conn {
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.SetKeepAlive(true)
				_ = tc.SetKeepAlivePeriod(period)
			}
			return conn
		}
	}
}

// newPublicKeyHandler accepts only the given key; a key that fails to parse
// rejects every connection.
func newPublicKeyHandler(publicKey string) ssh.PublicKeyHandler {
	authorizedKey, _, _, _, err := gossh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		log.Printf("failed to parse authorized public key: %v", err)
	}
	return func(ctx ssh.Context, key ssh.PublicKey) bool {
		return authorizedKey != nil && ssh.KeysEqual(key, authorizedKey)
	}
}

// ═══════════════════════════ CLIENT CONNECT ═══════════════════════════

func handleSession(s ssh.Session) {
	user, remote := peer(s)
	log.Printf("client connected: user=%s remote=%s", user, remote)
	defer log.Printf("client disconnected: user=%s remote=%s", user, remote)

	switch {
	case len(s.Command()) > 0:
		runExec(s)
	case hasPTY(s):
		runPTYShell(s)
	default:
		runShellStdin(s)
	}
}

func hasPTY(s ssh.Session) bool { _, _, ok := s.Pty(); return ok }

func peer(s ssh.Session) (user, remote string) {
	user = s.User()
	if r := s.RemoteAddr(); r != nil {
		remote = r.String()
	}
	return user, remote
}

// runExec runs the client's raw command via sh -c, like OpenSSH.
func runExec(s ssh.Session) {
	user, remote := peer(s)
	rawCmd := s.RawCommand()
	log.Printf("exec request: user=%s remote=%s cmd=%q", user, remote, rawCmd)
	runHostCommand(s, exec.Command("sh", "-c", rawCmd))
}

func runShellStdin(s ssh.Session) {
	runHostCommand(s, exec.Command(shellPath(), "-s"))
}

// runPTYShell bridges the session to a PTY-backed shell. The copy/resize
// goroutines are panic-isolated so a copy fault cannot crash the agent.
func runPTYShell(s ssh.Session) {
	ptyReq, winCh, _ := s.Pty()
	cmd := exec.Command(shellPath())
	f, err := pty.Start(cmd)
	if err != nil {
		fmt.Fprintf(s, "failed to start pty shell: %v\n", err)
		return
	}
	defer f.Close()

	if ptyReq.Window.Width > 0 && ptyReq.Window.Height > 0 {
		pty.Setsize(f, &pty.Winsize{Cols: uint16(ptyReq.Window.Width), Rows: uint16(ptyReq.Window.Height)})
	}

	safeGo("pty->client copy", func() { _, _ = io.Copy(s, f) })
	safeGo("client->pty copy", func() { _, _ = io.Copy(f, s) })
	safeGo("pty window-change", func() {
		for win := range winCh {
			_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(win.Width), Rows: uint16(win.Height)})
		}
	})

	_ = cmd.Wait()
}

func handleSFTP(s ssh.Session) {
	server, err := sftp.NewServer(s)
	if err != nil {
		log.Printf("[ssh] sftp server init error: %v", err)
		return
	}
	if err := server.Serve(); err != nil && err != io.EOF {
		log.Printf("[ssh] sftp server error: %v", err)
	}
}

func runHostCommand(s ssh.Session, cmd *exec.Cmd) {
	cmd.Env = os.Environ()
	cmd.Stdin = s
	cmd.Stdout = s
	cmd.Stderr = s
	if w := s.Stderr(); w != nil { // a separate stderr stream is used when present
		cmd.Stderr = w
	}
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(s, "command error: %v\n", err)
	}
}

func shellPath() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

// ─────────────────────────── panic isolation ───────────────────────────

// logPanic recovers and logs a panic; use as: defer logPanic(name).
func logPanic(name string) {
	if r := recover(); r != nil {
		log.Printf("[ssh] recovered panic in %s: %v\n%s", name, r, debug.Stack())
	}
}

func safeGo(name string, fn func()) {
	go func() { defer logPanic(name); fn() }()
}

func recoverChannelHandler(name string, h ssh.ChannelHandler) ssh.ChannelHandler {
	return func(srv *ssh.Server, c *gossh.ServerConn, nc gossh.NewChannel, ctx ssh.Context) {
		defer logPanic("channel " + name)
		h(srv, c, nc, ctx)
	}
}

func recoverRequestHandler(name string, h ssh.RequestHandler) ssh.RequestHandler {
	return func(ctx ssh.Context, srv *ssh.Server, req *gossh.Request) (bool, []byte) {
		defer logPanic("request " + name) // on panic the zero return (false, nil) rejects the request
		return h(ctx, srv, req)
	}
}

// recoverSessionHandler panic-isolates an ssh.Handler / ssh.SubsystemHandler.
// gliderlabs runs these on a freshly spawned goroutine (`go func(){ handler(sess);
// sess.Exit(0) }()`), which a channel-level recover cannot reach — so the recover
// must wrap the handler itself, on the goroutine where it actually runs.
func recoverSessionHandler(name string, h func(ssh.Session)) func(ssh.Session) {
	return func(s ssh.Session) {
		defer logPanic("handler " + name)
		h(s)
	}
}

// ─────────────────────────── supervisor ───────────────────────────

// Supervisor tuning (vars so tests can shrink them).
var (
	maxConsecutiveFailures = 5
	minRestartBackoff      = 1 * time.Second
	maxRestartBackoff      = 30 * time.Second
	healthyRunThreshold    = 60 * time.Second // a run this long resets the failure count
	nowFunc                = time.Now
)

const (
	stateRunning    = "running"
	stateRestarting = "restarting"
	stateFailed     = "failed"
	stateStopped    = "stopped"
)

type SSHServer struct {
	mu        sync.Mutex
	current   *ssh.Server // rebuilt on each restart
	state     string
	restarts  int
	lastError string
	addr      string
	sessionID string
	stopCh    chan struct{}
	stopOnce  sync.Once
	stopped   bool
}

func supervise(sessionID, addr string, build func() *ssh.Server) *SSHServer {
	s := &SSHServer{state: stateRunning, addr: addr, sessionID: sessionID, stopCh: make(chan struct{})}

	activeServersMu.Lock()
	activeServers[sessionID] = s
	activeServersMu.Unlock()

	log.Printf("[ssh] starting supervised ssh server on %s (session=%s)", addr, sessionID)
	safeGo("ssh supervisor "+sessionID, func() { s.run(build) })
	return s
}

// run restarts the listener on non-graceful exits up to maxConsecutiveFailures;
// a run of at least healthyRunThreshold resets the count.
func (s *SSHServer) run(build func() *ssh.Server) {
	backoff, consecutive := minRestartBackoff, 0

	for {
		s.mu.Lock()
		if s.stopped {
			s.state = stateStopped
			s.mu.Unlock()
			break
		}
		srv := build()
		s.current, s.state = srv, stateRunning
		s.mu.Unlock()

		start := nowFunc()
		err := srv.ListenAndServe()
		ranFor := nowFunc().Sub(start)

		s.mu.Lock()
		if s.stopped || errors.Is(err, ssh.ErrServerClosed) {
			s.state = stateStopped
			s.mu.Unlock()
			break
		}
		s.restarts++
		if err != nil {
			s.lastError = err.Error()
		}
		if ranFor >= healthyRunThreshold {
			consecutive, backoff = 0, minRestartBackoff
		}
		if consecutive++; consecutive >= maxConsecutiveFailures {
			s.state = stateFailed
			s.mu.Unlock()
			log.Printf("[ssh] session %s: giving up after %d failures (%v)", s.sessionID, consecutive, err)
			break
		}
		s.state = stateRestarting
		s.mu.Unlock()

		log.Printf("[ssh] session %s: crashed (%v); restart %d/%d in %s", s.sessionID, err, consecutive, maxConsecutiveFailures, backoff)
		select {
		case <-time.After(backoff):
		case <-s.stopCh:
		}
		if backoff *= 2; backoff > maxRestartBackoff {
			backoff = maxRestartBackoff
		}
	}

	// Clean stops deregister; a failed server stays registered for observability.
	s.mu.Lock()
	failed := s.state == stateFailed
	s.mu.Unlock()
	if !failed {
		deleteServer(s.sessionID)
		log.Printf("[ssh] session %s: supervisor exited (stopped)", s.sessionID)
	}
}

func (s *SSHServer) signalStop() *ssh.Server {
	s.mu.Lock()
	s.stopped = true
	srv := s.current
	s.mu.Unlock()
	s.stopOnce.Do(func() { close(s.stopCh) })
	return srv
}

// Stop gracefully shuts down the SSH server and its supervisor.
func (s *SSHServer) Stop(ctx context.Context) error {
	if srv := s.signalStop(); srv != nil {
		return srv.Shutdown(ctx)
	}
	return nil
}

// Close immediately closes the SSH server and its supervisor.
func (s *SSHServer) Close() error {
	if srv := s.signalStop(); srv != nil {
		return srv.Close()
	}
	return nil
}

// ─────────────────────────── registry ───────────────────────────

// activeServers tracks supervised servers by session ID; a failed server stays
// registered so its status is queryable until the session is deleted.
var (
	activeServers   = make(map[string]*SSHServer)
	activeServersMu sync.Mutex
)

func lookupServer(sessionID string) (*SSHServer, bool) {
	activeServersMu.Lock()
	defer activeServersMu.Unlock()
	server, exists := activeServers[sessionID]
	return server, exists
}

// deleteServer atomically removes and returns the server for a session ID.
func deleteServer(sessionID string) (*SSHServer, bool) {
	activeServersMu.Lock()
	defer activeServersMu.Unlock()
	server, exists := activeServers[sessionID]
	if exists {
		delete(activeServers, sessionID)
	}
	return server, exists
}

// snapshotServers copies the registry so callers can iterate without the lock.
func snapshotServers() []*SSHServer {
	activeServersMu.Lock()
	defer activeServersMu.Unlock()
	servers := make([]*SSHServer, 0, len(activeServers))
	for _, server := range activeServers {
		servers = append(servers, server)
	}
	return servers
}

func stopSSHServerBySessionID(sessionID string) error {
	server, exists := deleteServer(sessionID)
	if !exists {
		return fmt.Errorf("no ssh server found for session %s", sessionID)
	}
	return server.Close()
}

// StopAllSSHServers stops every registered SSH server.
func StopAllSSHServers() {
	for _, server := range snapshotServers() {
		if err := server.Close(); err != nil {
			log.Printf("error stopping ssh server: %v", err)
		}
	}
}

// ─────────────────────────── status ───────────────────────────

type SessionStatus struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Active    bool   `json:"active"`
	Addr      string `json:"addr,omitempty"`
	Restarts  int    `json:"restarts"`
	LastError string `json:"last_error,omitempty"`
}

// status snapshots supervisor state; Active is true only while the listener is up.
func (s *SSHServer) status() *SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &SessionStatus{
		ID:        s.sessionID,
		State:     s.state,
		Active:    s.state == stateRunning,
		Addr:      s.addr,
		Restarts:  s.restarts,
		LastError: s.lastError,
	}
}

func getSessionStatus(sessionID string) (*SessionStatus, error) {
	server, exists := lookupServer(sessionID)
	if !exists {
		return nil, fmt.Errorf("no ssh server found for session %s", sessionID)
	}
	return server.status(), nil
}

// IsSessionActive reports whether the session's listener is currently up.
func IsSessionActive(sessionID string) bool {
	server, exists := lookupServer(sessionID)
	if !exists {
		return false
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.state == stateRunning
}

func listAllSessionStatuses() []*SessionStatus {
	servers := snapshotServers()
	statuses := make([]*SessionStatus, 0, len(servers))
	for _, server := range servers {
		statuses = append(statuses, server.status())
	}
	return statuses
}
