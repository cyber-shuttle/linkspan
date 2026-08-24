// Package sshd is linkspan's embedded SSH server. Every handler boundary is
// panic-isolated and the supervisor restarts the listener, so one bad
// connection cannot take linkspan down. subsystems/vscode is its only caller.
package sshd

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"os"
	"os/exec"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// Start binds before it returns, so the caller gets the port that is actually
// held and a bind failure is reported now rather than surfacing later as a
// session that never accepted anything. Loopback only: the port reaches clients
// through the tunnel, never the node's network.
func Start(authorized ssh.PublicKey) (id string, port int, err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", 0, err
	}
	addr := ln.Addr().String()
	port = ln.Addr().(*net.TCPAddr).Port
	// The id embeds the port because the client reads it back out of the id.
	id = fmt.Sprintf("s-%d", port)
	supervise(id, addr, ln, func() *ssh.Server { return newServer(addr, authorized) })
	return id, port, nil
}

// Built per (re)start: ForwardedTCPHandler holds per-server state. sftp and
// streamlocal look unused because the client is VS Code, not cs-bridge.
func newServer(addr string, authorized ssh.PublicKey) *ssh.Server {
	fwd := &ssh.ForwardedTCPHandler{}
	return &ssh.Server{
		Addr:                          addr,
		Handler:                       guardSession("session", handleSession),
		PublicKeyHandler:              authorizer(authorized), // PasswordHandler stays nil: keys only
		ConnCallback:                  keepAlive(30 * time.Second),
		LocalPortForwardingCallback:   allowForward("local port forwarding"),
		ReversePortForwardingCallback: allowForward("reverse port forwarding"),
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":                        guardChannel("session", ssh.DefaultSessionHandler),
			"direct-tcpip":                   guardChannel("direct-tcpip", ssh.DirectTCPIPHandler),
			"direct-streamlocal@openssh.com": guardChannel("direct-streamlocal", directStreamLocal),
		},
		RequestHandlers: map[string]ssh.RequestHandler{
			"tcpip-forward":        guardRequest("tcpip-forward", fwd.HandleSSHRequest),
			"cancel-tcpip-forward": guardRequest("cancel-tcpip-forward", fwd.HandleSSHRequest),
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": guardSession("sftp", handleSFTP),
		},
	}
}

func authorizer(authorized ssh.PublicKey) ssh.PublicKeyHandler {
	return func(_ ssh.Context, key ssh.PublicKey) bool {
		return authorized != nil && ssh.KeysEqual(key, authorized)
	}
}

func allowForward(kind string) func(ssh.Context, string, uint32) bool {
	return func(_ ssh.Context, host string, port uint32) bool {
		log.Printf("%s requested: host=%s port=%d", kind, host, port)
		return true
	}
}

// IdleTimeout and MaxTimeout deliberately stay unset (0).
func keepAlive(period time.Duration) func(ssh.Context, net.Conn) net.Conn {
	return func(_ ssh.Context, conn net.Conn) net.Conn {
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(period)
		}
		return conn
	}
}

func handleSession(s ssh.Session) {
	user, remote := peer(s)
	log.Printf("client connected: user=%s remote=%s", user, remote)
	defer log.Printf("client disconnected: user=%s remote=%s", user, remote)

	switch _, _, isPTY := s.Pty(); {
	case len(s.Command()) > 0: // raw command via sh -c, like OpenSSH
		log.Printf("exec request: user=%s remote=%s cmd=%q", user, remote, s.RawCommand())
		runHostCommand(s, exec.CommandContext(s.Context(), "sh", "-c", s.RawCommand()))
	case isPTY:
		runPTYShell(s)
	default:
		runHostCommand(s, exec.CommandContext(s.Context(), shellPath(), "-s"))
	}
}

func peer(s ssh.Session) (user, remote string) {
	if r := s.RemoteAddr(); r != nil {
		remote = r.String()
	}
	return s.User(), remote
}

// The client is told the command's own exit status. Without the Exit call
// gliderlabs sends 0 for every session, so a failed command looks successful.
func runHostCommand(s ssh.Session, cmd *exec.Cmd) {
	var stderr io.Writer = s
	if w := s.Stderr(); w != nil { // a separate stderr stream is used when present
		stderr = w
	}
	cmd.Env, cmd.Stdout, cmd.Stderr = os.Environ(), s, stderr

	// Not cmd.Stdin = s. Wait would then block on an stdin copy that only ends
	// when the client closes its own stdin, so a command that had already
	// finished would hang the session. Wait closes this pipe when the process
	// exits, which ends the copy instead.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(stderr, "command error: %v\n", err)
		_ = s.Exit(127)
		return
	}
	safeGo("session stdin copy", func() { defer stdin.Close(); _, _ = io.Copy(stdin, s) })

	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		_ = s.Exit(0)
	case errors.As(err, &exit):
		code := exit.ExitCode()
		if code < 0 { // killed by a signal; we do not send SSH exit-signal
			code = 255
		}
		_ = s.Exit(code)
	default: // it never ran
		fmt.Fprintf(stderr, "command error: %v\n", err)
		_ = s.Exit(127)
	}
}

func runPTYShell(s ssh.Session) {
	ptyReq, winCh, _ := s.Pty()
	cmd := exec.CommandContext(s.Context(), shellPath())
	f, err := pty.Start(cmd)
	if err != nil {
		fmt.Fprintf(s, "failed to start pty shell: %v\n", err)
		return
	}
	defer f.Close()

	resize := func(w, h int) {
		if w > 0 && h > 0 {
			_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
		}
	}
	resize(ptyReq.Window.Width, ptyReq.Window.Height)

	safeGo("pty->client copy", func() { _, _ = io.Copy(s, f) })
	safeGo("client->pty copy", func() { _, _ = io.Copy(f, s) })
	safeGo("pty window-change", func() {
		for win := range winCh {
			resize(win.Width, win.Height)
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

// direct-streamlocal@openssh.com open payload (OpenSSH PROTOCOL).
type streamLocalChannelData struct {
	SocketPath string
	Reserved0  string
	Reserved1  uint32
}

func directStreamLocal(_ *ssh.Server, _ *gossh.ServerConn, newChan gossh.NewChannel, _ ssh.Context) {
	var d streamLocalChannelData
	if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
		newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}
	log.Printf("streamlocal forwarding requested: path=%s", d.SocketPath)
	dconn, err := net.Dial("unix", d.SocketPath)
	if err != nil {
		newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := newChan.Accept()
	if err != nil {
		dconn.Close()
		return
	}
	go gossh.DiscardRequests(reqs)
	pipe := func(dst io.Writer, src io.Reader) {
		safeGo("streamlocal copy", func() { defer ch.Close(); defer dconn.Close(); _, _ = io.Copy(dst, src) })
	}
	pipe(dconn, ch)
	pipe(ch, dconn)
}

func shellPath() string { return cmp.Or(os.Getenv("SHELL"), "/bin/sh") }

func logPanic(name string) {
	if r := recover(); r != nil {
		log.Printf("[ssh] recovered panic in %s: %v\n%s", name, r, debug.Stack())
	}
}

func safeGo(name string, fn func()) { go func() { defer logPanic(name); fn() }() }

func guardChannel(name string, h ssh.ChannelHandler) ssh.ChannelHandler {
	return func(srv *ssh.Server, c *gossh.ServerConn, nc gossh.NewChannel, ctx ssh.Context) {
		defer logPanic("channel " + name)
		h(srv, c, nc, ctx)
	}
}

// On panic the zero return, (false, nil), rejects the request.
func guardRequest(name string, h ssh.RequestHandler) ssh.RequestHandler {
	return func(ctx ssh.Context, srv *ssh.Server, req *gossh.Request) (bool, []byte) {
		defer logPanic("request " + name)
		return h(ctx, srv, req)
	}
}

// Wraps the handler, not its channel: gliderlabs dispatches these on a fresh
// goroutine that a channel-level recover cannot reach.
func guardSession(name string, h func(ssh.Session)) func(ssh.Session) {
	return func(s ssh.Session) {
		defer logPanic("handler " + name)
		h(s)
	}
}

// Vars so tests can shrink them.
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
	addr      string
	sessionID string
	stopCh    chan struct{}
	stopped   bool
}

func supervise(sessionID, addr string, first net.Listener, build func() *ssh.Server) *SSHServer {
	s := &SSHServer{state: stateRunning, addr: addr, sessionID: sessionID, stopCh: make(chan struct{})}

	activeServersMu.Lock()
	activeServers[sessionID] = s
	activeServersMu.Unlock()

	log.Printf("[ssh] starting supervised ssh server on %s (session=%s)", addr, sessionID)
	safeGo("ssh supervisor "+sessionID, func() { s.run(build, first) })
	return s
}

func (s *SSHServer) run(build func() *ssh.Server, first net.Listener) {
	backoff, consecutive := minRestartBackoff, 0
	defer func() {
		if first != nil { // stopped before it was ever served
			_ = first.Close()
		}
	}()

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

		ln, err := first, error(nil)
		first = nil
		if ln == nil { // every restart rebinds; Serve closed the last listener
			ln, err = net.Listen("tcp", s.addr)
		}
		start := nowFunc()
		if err == nil {
			err = srv.Serve(ln)
		}
		ranFor := nowFunc().Sub(start)

		s.mu.Lock()
		if s.stopped || errors.Is(err, ssh.ErrServerClosed) {
			s.state = stateStopped
			s.mu.Unlock()
			break
		}
		if ranFor >= healthyRunThreshold {
			consecutive, backoff = 0, minRestartBackoff
		}
		consecutive++
		if consecutive >= maxConsecutiveFailures {
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
		backoff = min(backoff*2, maxRestartBackoff)
	}

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
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stopCh)
	}
	return s.current
}

func (s *SSHServer) Close() error {
	if srv := s.signalStop(); srv != nil {
		return srv.Close()
	}
	return nil
}

// A failed server stays registered, so its status stays queryable.
var (
	activeServers   = make(map[string]*SSHServer)
	activeServersMu sync.Mutex
)

func deleteServer(sessionID string) (*SSHServer, bool) {
	activeServersMu.Lock()
	defer activeServersMu.Unlock()
	server, exists := activeServers[sessionID]
	delete(activeServers, sessionID)
	return server, exists
}

// Nothing may take a server's own lock while activeServersMu is held.
func snapshotServers() []*SSHServer {
	activeServersMu.Lock()
	defer activeServersMu.Unlock()
	return slices.Collect(maps.Values(activeServers))
}

func StopAll() {
	for _, server := range snapshotServers() {
		if err := server.Close(); err != nil {
			log.Printf("error stopping ssh server: %v", err)
		}
	}
}

type SessionStatus struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Addr  string `json:"addr,omitempty"`
}

func Statuses() []*SessionStatus {
	servers := snapshotServers()
	statuses := make([]*SessionStatus, 0, len(servers))
	for _, s := range servers {
		s.mu.Lock()
		statuses = append(statuses, &SessionStatus{ID: s.sessionID, State: s.state, Addr: s.addr})
		s.mu.Unlock()
	}
	return statuses
}
