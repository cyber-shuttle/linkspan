// Package sshd is linkspan's embedded SSH server. Every handler boundary is
// panic-isolated and the supervisor restarts the listener, so one bad
// connection cannot take linkspan down.
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
	supervise(id, addr, ln, func() *ssh.Server { return newServer(authorized) })
	return id, port, nil
}

// Built per (re)start: ForwardedTCPHandler holds per-server state. sftp and
// streamlocal look unused because the client is VS Code, not cs-bridge.
func newServer(authorized ssh.PublicKey) *ssh.Server {
	fwd := &ssh.ForwardedTCPHandler{}
	return &ssh.Server{
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
	user, remote := s.User(), s.RemoteAddr()
	log.Printf("client connected: user=%s remote=%s", user, remote)
	defer log.Printf("client disconnected: user=%s remote=%s", user, remote)

	_, winCh, isPTY := s.Pty()
	if isPTY && len(s.Command()) > 0 {
		// A pty was allocated but a command was given, so runPTYShell -- the only
		// reader of window-change -- never runs. gliderlabs delivers those with an
		// unbuffered-in-effect send (cap 1, pre-filled), so an unread resize wedges
		// this session's request loop.
		safeGo("drain window-change", func() {
			for range winCh {
			}
		})
	}

	switch {
	case len(s.Command()) > 0: // raw command via sh -c, like OpenSSH
		log.Printf("exec request: user=%s remote=%s cmd=%q", user, remote, s.RawCommand())
		runHostCommand(s, exec.CommandContext(s.Context(), "sh", "-c", s.RawCommand()))
	case isPTY:
		runPTYShell(s)
	default:
		runHostCommand(s, exec.CommandContext(s.Context(), shellPath(), "-s"))
	}
}

func runHostCommand(s ssh.Session, cmd *exec.Cmd) {
	stderr := s.Stderr()
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
	if err != nil && !errors.As(err, new(*exec.ExitError)) {
		fmt.Fprintf(stderr, "command error: %v\n", err) // it never ran
	}
	reportExit(s, err)
}

// reportExit gives the client the command's own status. gliderlabs sends 0 for
// any session whose handler simply returns, so every failure -- including a
// shell that never started -- would otherwise look like success.
func reportExit(s ssh.Session, err error) {
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
	default:
		_ = s.Exit(127) // it never ran
	}
}

func runPTYShell(s ssh.Session) {
	ptyReq, winCh, _ := s.Pty()
	cmd := exec.CommandContext(s.Context(), shellPath())
	if ptyReq.Term != "" {
		// The client's terminal type. Without it the shell inherits the batch
		// job's environment, which has no TERM, and curses programs misbehave.
		cmd.Env = append(os.Environ(), "TERM="+ptyReq.Term)
	}
	f, err := pty.Start(cmd)
	if err != nil {
		// A PTY session carries a single stream, so this goes to s rather than
		// s.Stderr() -- which is also what sshd does when a pty is allocated.
		fmt.Fprintf(s, "failed to start pty shell: %v\n", err)
		reportExit(s, err)
		return
	}
	defer f.Close()

	resize := func(w, h int) {
		if w > 0 && h > 0 {
			_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
		}
	}
	resize(ptyReq.Window.Width, ptyReq.Window.Height)

	drained := make(chan struct{})
	// The reader stops when the shell exits (the master reports EOF) or when the
	// session goes away (the write to the client fails). Either way the pty is
	// done with, and closing it is what stops a shell whose session ended --
	// s.Context() belongs to the connection, so it would keep one alive until
	// the whole connection dropped. Not the writer: stdin reaching EOF only
	// means the client sent no more input, which is not the session closing.
	safeGo("pty->client copy", func() { defer close(drained); _, _ = io.Copy(s, f); _ = f.Close() })
	safeGo("client->pty copy", func() { _, _ = io.Copy(f, s) })
	safeGo("pty window-change", func() {
		for win := range winCh {
			resize(win.Width, win.Height)
		}
	})

	err = cmd.Wait()
	// Let the reader finish before the deferred Close pulls the master out from
	// under it, or the shell's last output is dropped. Bounded, because a
	// wedged read must not hold the handler.
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
	}
	reportExit(s, err)
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

type supervisor struct {
	mu        sync.Mutex
	current   *ssh.Server // rebuilt on each restart
	state     string
	addr      string
	sessionID string
	listener  net.Listener // closed by signalStop, so a stop always ends Accept
	stopCh    chan struct{}
	stopped   bool
}

func supervise(sessionID, addr string, first net.Listener, build func() *ssh.Server) {
	s := &supervisor{state: stateRunning, addr: addr, sessionID: sessionID, stopCh: make(chan struct{})}

	activeServersMu.Lock()
	activeServers[sessionID] = s
	activeServersMu.Unlock()

	log.Printf("[ssh] starting supervised ssh server on %s (session=%s)", addr, sessionID)
	safeGo("ssh supervisor "+sessionID, func() { s.run(build, first) })
}

func (s *supervisor) run(build func() *ssh.Server, first net.Listener) {
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
		// Publish the listener before serving. A stop between here and Serve
		// would otherwise leave no trace: gliderlabs' trackListener resets the
		// server's doneChan when it has no listeners yet, so Close is erased and
		// Serve accepts forever on a listener nothing shuts. signalStop closes
		// whatever is published, so either order ends the same way.
		s.mu.Lock()
		if s.stopped {
			s.state = stateStopped
			s.mu.Unlock()
			if ln != nil {
				_ = ln.Close()
			}
			break
		}
		s.listener = ln
		s.mu.Unlock()

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
		s.deregister()
		log.Printf("[ssh] session %s: supervisor exited (stopped)", s.sessionID)
	}
}

func (s *supervisor) signalStop() *ssh.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stopCh)
	}
	if s.listener != nil {
		_ = s.listener.Close() // ends Accept even if Serve has not started yet
	}
	return s.current
}

// signalStop may already have closed the listener, and gliderlabs closes it
// again on its way out. A listener that is already shut is what was asked for.
func (s *supervisor) Close() error {
	if srv := s.signalStop(); srv != nil {
		if err := srv.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
	}
	return nil
}

// A failed server stays registered, so its status stays queryable.
var (
	activeServers   = make(map[string]*supervisor)
	activeServersMu sync.Mutex
)

// deregister removes this supervisor's entry, and only its own. The id is
// s-<port>, so a later session can be handed the same port and therefore the
// same id; deleting by id alone would take that live session out of the
// registry when this one finally exited.
func (s *supervisor) deregister() {
	activeServersMu.Lock()
	defer activeServersMu.Unlock()
	if activeServers[s.sessionID] == s {
		delete(activeServers, s.sessionID)
	}
}

// Nothing may take a server's own lock while activeServersMu is held.
func snapshotServers() []*supervisor {
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
