package vscode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// Simple SSH server with no authentication.
// - Interactive sessions present a small REPL that echoes input.
// - Non-interactive sessions execute the requested command on the host.
//
// Resilience: a server started here must never take linkspan down and must keep
// its listener up so clients can reconnect. Every handler dispatch boundary is
// panic-isolated, and a supervisor restarts the listener after a fatal exit.

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

// SSHServer is a supervised SSH server whose listener is restarted on crashes.
type SSHServer struct {
	mu        sync.Mutex
	current   *ssh.Server // current live instance, rebuilt on each restart
	state     string
	restarts  int
	lastError string
	addr      string
	sessionID string
	stopCh    chan struct{}
	stopOnce  sync.Once
	stopped   bool
}

// activeServers tracks supervised servers by session ID; a "failed" server stays
// registered so its status remains queryable until the session is deleted.
var (
	activeServers   = make(map[string]*SSHServer)
	activeServersMu sync.Mutex
)

// logPanic recovers and logs a panic; use as `defer logPanic(name)`.
func logPanic(name string) {
	if r := recover(); r != nil {
		log.Printf("[ssh] recovered panic in %s: %v\n%s", name, r, debug.Stack())
	}
}

// safeGo runs fn in a panic-isolated goroutine.
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

// StartSSHServerForVSCodeConnection starts a supervised SSH server and registers
// it. The listener auto-restarts on crashes; a single bad connection can never
// bring it (or linkspan) down.
func StartSSHServerForVSCodeConnection(sessionID, addr string, password string, publicKey string) *SSHServer {

	// Session handler: support exec (non-interactive) and a tiny interactive REPL.
	sessionHandler := func(s ssh.Session) {
		// Log connection and disconnection for visibility.
		remote := ""
		if r := s.RemoteAddr(); r != nil {
			remote = r.String()
		}
		user := s.User()
		log.Printf("client connected: user=%s remote=%s", user, remote)
		defer log.Printf("client disconnected: user=%s remote=%s", user, remote)
		// Non-interactive command execution: run the command on the host.
		// Use RawCommand() to get the exact string the client sent, then
		// pass it to sh -c like OpenSSH does.
		if len(s.Command()) > 0 {
			rawCmd := s.RawCommand()
			log.Printf("exec request: user=%s remote=%s cmd=%q", user, remote, rawCmd)

			cmd := exec.Command("sh", "-c", rawCmd)
			cmd.Env = os.Environ()
			cmd.Stdin = s
			cmd.Stdout = s
			// gliderlabs/ssh.Session implements Stderr() for a separate stream
			if w := s.Stderr(); w != nil {
				cmd.Stderr = w
			} else {
				cmd.Stderr = s
			}
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(s, "command error: %v\n", err)
			}
			return
		}

		// Interactive session: if the client requested a PTY, run a shell in a pty
		// so terminal modes (echo, resize) are handled correctly. Otherwise fall
		// back to a small REPL.
		ptyReq, winCh, isPty := s.Pty()
		if isPty {
			// Launch a shell attached to a pty
			shell := os.Getenv("SHELL")
			if shell == "" {
				shell = "/bin/sh"
			}
			cmd := exec.Command(shell)
			f, err := pty.Start(cmd)
			if err != nil {
				fmt.Fprintf(s, "failed to start pty shell: %v\n", err)
				return
			}
			defer f.Close()

			// Set initial window size from the request
			if ptyReq.Window.Width > 0 && ptyReq.Window.Height > 0 {
				pty.Setsize(f, &pty.Winsize{Cols: uint16(ptyReq.Window.Width), Rows: uint16(ptyReq.Window.Height)})
			}

			// copy input/output (panic-isolated so a copy fault can't crash the agent)
			safeGo("pty->client copy", func() { _, _ = io.Copy(s, f) })
			safeGo("client->pty copy", func() { _, _ = io.Copy(f, s) })

			// handle window changes
			safeGo("pty window-change", func() {
				for win := range winCh {
					_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(win.Width), Rows: uint16(win.Height)})
				}
			})

			_ = cmd.Wait()
			return
		}

		if len(s.Command()) == 0 && !isPty {
			// No exec requested and no pty: run the user's shell to execute piped stdin.
			shell := os.Getenv("SHELL")
			if shell == "" {
				shell = "/bin/sh"
			}
			cmd := exec.Command(shell, "-s") // read commands from stdin
			cmd.Env = os.Environ()
			cmd.Stdin = s
			cmd.Stdout = s
			if w := s.Stderr(); w != nil {
				cmd.Stderr = w
			} else {
				cmd.Stderr = s
			}
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(s, "command error: %v\n", err)
			}
			return
		}

		// Fallback REPL when no PTY was requested
		r := bufio.NewReader(s)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					log.Printf("read error: %v", err)
				}
				break
			}
			// Log readline input coming from the client REPL
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				log.Printf("client input: user=%s remote=%s line=%q", user, remote, trimmed)
			}
			line = trimmed
			switch line {
			case "", "\n":
				// ignore empty
			case "exit", "quit":
				io.WriteString(s, "bye\n")
				return
			default:
				fmt.Fprintf(s, "%s", line)
			}
		}
	}

	// Parse the authorized public key for validation.
	authorizedKey, _, _, _, err := gossh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		log.Printf("failed to parse authorized public key: %v", err)
	}
	publicKeyHandler := func(ctx ssh.Context, key ssh.PublicKey) bool {
		if authorizedKey == nil {
			return false
		}
		return ssh.KeysEqual(key, authorizedKey)
	}

	// Password auth: accept only the configured password.
	passwordHandler := func(ctx ssh.Context, pass string) bool {
		return pass == password
	}

	// build constructs a fresh server for each (re)start. Handlers are wrapped so a
	// panic in any one is recovered instead of crashing the process.
	build := func() *ssh.Server {
		// Create a forwarded TCP handler for reverse port forwarding (per-server state)
		forwardHandler := &ssh.ForwardedTCPHandler{}

		return &ssh.Server{
			Addr:             addr,
			Handler:          sessionHandler,
			PublicKeyHandler: publicKeyHandler,
			PasswordHandler:  passwordHandler,
			// Keep a quiet/lagging session alive at the TCP layer; IdleTimeout/MaxTimeout stay unset (0).
			ConnCallback: func(ctx ssh.Context, conn net.Conn) net.Conn {
				if tc, ok := conn.(*net.TCPConn); ok {
					_ = tc.SetKeepAlive(true)
					_ = tc.SetKeepAlivePeriod(30 * time.Second)
				}
				return conn
			},
			LocalPortForwardingCallback: func(ctx ssh.Context, dhost string, dport uint32) bool {
				log.Printf("local port forwarding requested: host=%s port=%d", dhost, dport)
				return true // Allow all local port forwards
			},
			ReversePortForwardingCallback: func(ctx ssh.Context, bindHost string, bindPort uint32) bool {
				log.Printf("reverse port forwarding requested: host=%s port=%d", bindHost, bindPort)
				return true // Allow all reverse port forwards
			},
			ChannelHandlers: map[string]ssh.ChannelHandler{
				"session":      recoverChannelHandler("session", ssh.DefaultSessionHandler),
				"direct-tcpip": recoverChannelHandler("direct-tcpip", ssh.DirectTCPIPHandler),
			},
			RequestHandlers: map[string]ssh.RequestHandler{
				"tcpip-forward":        recoverRequestHandler("tcpip-forward", forwardHandler.HandleSSHRequest),
				"cancel-tcpip-forward": recoverRequestHandler("cancel-tcpip-forward", forwardHandler.HandleSSHRequest),
			},
			SubsystemHandlers: map[string]ssh.SubsystemHandler{
				// Runs inside the recover-wrapped "session" channel goroutine, so a
				// panic here is already contained.
				"sftp": func(s ssh.Session) {
					server, err := sftp.NewServer(s)
					if err != nil {
						log.Printf("[ssh] sftp server init error: %v", err)
						return
					}
					if err := server.Serve(); err != nil && err != io.EOF {
						log.Printf("[ssh] sftp server error: %v", err)
					}
				},
			},
		}
	}

	s := &SSHServer{
		state:     stateRunning,
		addr:      addr,
		sessionID: sessionID,
		stopCh:    make(chan struct{}),
	}

	activeServersMu.Lock()
	activeServers[sessionID] = s
	activeServersMu.Unlock()

	log.Printf("[ssh] starting supervised ssh server on %s (session=%s)", addr, sessionID)

	safeGo("ssh supervisor "+sessionID, func() { s.supervise(build) })

	return s
}

// supervise runs the listener, restarting it on non-graceful exits up to
// maxConsecutiveFailures; a run of at least healthyRunThreshold resets the count.
func (s *SSHServer) supervise(build func() *ssh.Server) {
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
		activeServersMu.Lock()
		delete(activeServers, s.sessionID)
		activeServersMu.Unlock()
		log.Printf("[ssh] session %s: supervisor exited (stopped)", s.sessionID)
	}
}

// signalStop marks the server stopped and wakes the supervisor.
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

// stopSSHServerBySessionID stops the SSH server for the given session ID and
// removes it from the registry (including a "failed" server whose supervisor
// has already exited).
func stopSSHServerBySessionID(sessionID string) error {
	activeServersMu.Lock()
	server, exists := activeServers[sessionID]
	if exists {
		delete(activeServers, sessionID)
	}
	activeServersMu.Unlock()

	if !exists {
		return fmt.Errorf("no ssh server found for session %s", sessionID)
	}

	return server.Close()
}

// SessionStatus represents the status of an SSH session.
type SessionStatus struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Active    bool   `json:"active"`
	Addr      string `json:"addr,omitempty"`
	Restarts  int    `json:"restarts"`
	LastError string `json:"last_error,omitempty"`
}

// status snapshots the supervisor state; Active is true only while the listener
// is up, so callers get truthful liveness rather than mere registration.
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

// getSessionStatus returns the status of the SSH server for the given session ID.
func getSessionStatus(sessionID string) (*SessionStatus, error) {
	activeServersMu.Lock()
	server, exists := activeServers[sessionID]
	activeServersMu.Unlock()

	if !exists {
		return nil, fmt.Errorf("no ssh server found for session %s", sessionID)
	}

	return server.status(), nil
}

// IsSessionActive reports whether the session's listener is currently up.
func IsSessionActive(sessionID string) bool {
	activeServersMu.Lock()
	server, exists := activeServers[sessionID]
	activeServersMu.Unlock()
	if !exists {
		return false
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.state == stateRunning
}

// StopAllSSHServers stops every registered SSH server.
func StopAllSSHServers() {
	activeServersMu.Lock()
	servers := make([]*SSHServer, 0, len(activeServers))
	for _, server := range activeServers {
		servers = append(servers, server)
	}
	activeServersMu.Unlock()

	for _, server := range servers {
		err := server.Close()
		if err != nil {
			log.Printf("error stopping ssh server: %v", err)
		}
	}
}

// listAllSessionStatuses returns a status snapshot for every registered session.
func listAllSessionStatuses() []*SessionStatus {
	activeServersMu.Lock()
	servers := make([]*SSHServer, 0, len(activeServers))
	for _, server := range activeServers {
		servers = append(servers, server)
	}
	activeServersMu.Unlock()

	statuses := make([]*SessionStatus, 0, len(servers))
	for _, server := range servers {
		statuses = append(statuses, server.status())
	}
	return statuses
}
