// Package sshd is linkspan's embedded SSH server: every session, channel and
// request handler is panic-isolated and a supervisor restarts the listener, so a
// panicking handler cannot take linkspan down.
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

	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

const (
	exitNeverRan  = 127
	exitSignalled = 255 // we do not send SSH exit-signal
)

// Start binds before returning, so the caller gets a port that is already held.
func Start(authorized ssh.PublicKey) (id string, port int, err error) {
	// ponytail: no TCP keepalive -- the listener is loopback and the peer is the
	// devtunnel host on this node; revisit if sshd ever binds off-loopback again.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", 0, err
	}
	addr := ln.Addr().String()
	port = ln.Addr().(*net.TCPAddr).Port
	id = fmt.Sprintf("s-%d", port) // cs-bridge strips the "s-" to recover the port
	supervise(id, addr, ln, func() *ssh.Server { return newServer(authorized) })
	return id, port, nil
}

// IdleTimeout and MaxTimeout deliberately stay unset.
func newServer(authorized ssh.PublicKey) *ssh.Server {
	return &ssh.Server{
		Handler:                     guardSession("session", handleSession),
		PublicKeyHandler:            authorizer(authorized), // PasswordHandler stays nil: keys only
		PtyCallback:                 func(ssh.Context, ssh.Pty) bool { return false },
		LocalPortForwardingCallback: allowForward("local port forwarding"),
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":                        guardChannel("session", ssh.DefaultSessionHandler),
			"direct-tcpip":                   guardChannel("direct-tcpip", ssh.DirectTCPIPHandler),
			"direct-streamlocal@openssh.com": guardChannel("direct-streamlocal", directStreamLocal),
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

func handleSession(s ssh.Session) {
	user, remote := s.User(), s.RemoteAddr()
	log.Printf("client connected: user=%s remote=%s", user, remote)
	defer log.Printf("client disconnected: user=%s remote=%s", user, remote)

	if len(s.Command()) > 0 {
		log.Printf("exec request: user=%s remote=%s cmd=%q", user, remote, s.RawCommand())
		runHostCommand(s, exec.CommandContext(s.Context(), "sh", "-c", s.RawCommand()))
		return
	}
	runHostCommand(s, exec.CommandContext(s.Context(), shellPath(), "-s"))
}

func runHostCommand(s ssh.Session, cmd *exec.Cmd) {
	stderr := s.Stderr()
	cmd.Env, cmd.Stdout, cmd.Stderr = os.Environ(), s, stderr

	// Not cmd.Stdin = s. Wait would then block on a copy that only ends when the
	// client closes its own stdin, hanging a command that had already finished.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(stderr, "command error: %v\n", err)
		_ = s.Exit(exitNeverRan)
		return
	}
	safeGo("session stdin copy", func() { defer stdin.Close(); _, _ = io.Copy(stdin, s) })

	err = cmd.Run()
	if err != nil && !errors.As(err, new(*exec.ExitError)) {
		fmt.Fprintf(stderr, "command error: %v\n", err)
	}
	reportExit(s, err)
}

// gliderlabs sends 0 for any session whose handler simply returns, so without
// this every failure -- including a shell that never started -- looks like success.
func reportExit(s ssh.Session, err error) {
	var exit *exec.ExitError
	switch {
	case err == nil:
		_ = s.Exit(0)
	case errors.As(err, &exit):
		code := exit.ExitCode()
		if code < 0 {
			code = exitSignalled
		}
		_ = s.Exit(code)
	default:
		_ = s.Exit(exitNeverRan)
	}
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

// Wraps the handler, not its channel: gliderlabs dispatches these on a fresh
// goroutine that a channel-level recover cannot reach.
func guardSession(name string, h func(ssh.Session)) func(ssh.Session) {
	return func(s ssh.Session) {
		defer logPanic("handler " + name)
		h(s)
	}
}

var (
	maxConsecutiveFailures = 5
	minRestartBackoff      = 1 * time.Second
	maxRestartBackoff      = 30 * time.Second
	healthyRunThreshold    = 60 * time.Second
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
	current   *ssh.Server
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
		// Publish the listener before serving. gliderlabs' trackListener resets the
		// server's doneChan while it has no listeners, so a Close in between would
		// be erased and Serve would accept forever on a listener nothing shuts.
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

func (s *supervisor) Close() error {
	if srv := s.signalStop(); srv != nil {
		// signalStop already closed the listener and gliderlabs closes it again on
		// its way out; an already-shut listener is what was asked for.
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

// Removes this supervisor's entry, and only its own: ids repeat when a later
// session is handed the same port, so deleting by id alone could evict a live one.
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
