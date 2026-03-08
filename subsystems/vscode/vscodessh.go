package vscode

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
)

// Simple SSH server with no authentication.
// - Interactive sessions present a small REPL that echoes input.
// - Non-interactive sessions execute the requested command on the host.

// SSHServer wraps a gliderlabs/ssh.Server so it can be stopped gracefully.
type SSHServer struct {
	server *ssh.Server
	mu     sync.Mutex
}

// activeServers tracks running SSH servers by session ID for later termination.
var (
	activeServers   = make(map[string]*SSHServer)
	activeServersMu sync.Mutex
)

// StartSSHServerForVSCodeConnection starts an SSH server and registers it for later shutdown.
// Returns the SSHServer instance so it can be stopped.
func StartSSHServerForVSCodeConnection(sessionID, addr string) *SSHServer {

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
		if len(s.Command()) > 0 {
			// Log the exact command requested by the client.
			cmdArgs := s.Command()
			log.Printf("exec request: user=%s remote=%s cmd=%q", user, remote, cmdArgs)

			cmd := exec.Command(s.Command()[0], s.Command()[1:]...)
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
		//fmt.Fprintf(s, "Welcome, %s! Type 'exit' or 'quit' to disconnect.\n", s.User())

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

			// copy input/output
			go func() { _, _ = io.Copy(s, f) }()
			go func() { _, _ = io.Copy(f, s) }()

			// handle window changes
			go func() {
				for win := range winCh {
					_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(win.Width), Rows: uint16(win.Height)})
				}
			}()

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
			//io.WriteString(s, "> ")
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

	// Create a forwarded TCP handler for reverse port forwarding
	forwardHandler := &ssh.ForwardedTCPHandler{}

	// Create server with no auth handlers — gliderlabs/ssh automatically
	// sets NoClientAuth=true when all auth handlers are nil.
	server := &ssh.Server{
		Addr:    addr,
		Handler: sessionHandler,
		LocalPortForwardingCallback: func(ctx ssh.Context, dhost string, dport uint32) bool {
			log.Printf("local port forwarding requested: host=%s port=%d", dhost, dport)
			return true // Allow all local port forwards
		},
		ReversePortForwardingCallback: func(ctx ssh.Context, bindHost string, bindPort uint32) bool {
			log.Printf("reverse port forwarding requested: host=%s port=%d", bindHost, bindPort)
			return true // Allow all reverse port forwards
		},
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":      ssh.DefaultSessionHandler,
			"direct-tcpip": ssh.DirectTCPIPHandler,
		},
		RequestHandlers: map[string]ssh.RequestHandler{
			"tcpip-forward":        forwardHandler.HandleSSHRequest,
			"cancel-tcpip-forward": forwardHandler.HandleSSHRequest,
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
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

	sshServer := &SSHServer{server: server}

	// Register the server for later termination
	activeServersMu.Lock()
	activeServers[sessionID] = sshServer
	activeServersMu.Unlock()

	log.Printf("starting ssh server on %s (session=%s)...\n", addr, sessionID)

	// Start server in a goroutine so we can return the SSHServer instance
	go func() {
		if err := server.ListenAndServe(); err != nil && err != ssh.ErrServerClosed {
			log.Printf("ssh server error (session=%s): %v", sessionID, err)
		}
		// Clean up when server stops
		activeServersMu.Lock()
		delete(activeServers, sessionID)
		activeServersMu.Unlock()
		log.Printf("ssh server stopped (session=%s)", sessionID)
	}()

	return sshServer
}

// Stop gracefully shuts down the SSH server.
func (s *SSHServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

// Close immediately closes the SSH server.
func (s *SSHServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server == nil {
		return nil
	}
	return s.server.Close()
}

// StopSSHServerBySessionID stops the SSH server associated with the given session ID.
func stopSSHServerBySessionID(sessionID string) error {
	activeServersMu.Lock()
	server, exists := activeServers[sessionID]
	activeServersMu.Unlock()

	if !exists {
		return fmt.Errorf("no ssh server found for session %s", sessionID)
	}

	return server.Close()
}

// SessionStatus represents the status of an SSH session.
type SessionStatus struct {
	ID     string `json:"id"`
	Active bool   `json:"active"`
	Addr   string `json:"addr,omitempty"`
}

// GetSessionStatus returns the status of the SSH server for the given session ID.
func getSessionStatus(sessionID string) (*SessionStatus, error) {
	activeServersMu.Lock()
	server, exists := activeServers[sessionID]
	activeServersMu.Unlock()

	if !exists {
		return nil, fmt.Errorf("no ssh server found for session %s", sessionID)
	}

	status := &SessionStatus{
		ID:     sessionID,
		Active: true,
	}

	// Get the address if the server is running
	if server.server != nil {
		status.Addr = server.server.Addr
	}

	return status, nil
}

// IsSessionActive checks if a session with the given ID is currently active.
func IsSessionActive(sessionID string) bool {
	activeServersMu.Lock()
	_, exists := activeServers[sessionID]
	activeServersMu.Unlock()
	return exists
}

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

func listAllSessions() []string {
	activeServersMu.Lock()
	defer activeServersMu.Unlock()

	sessionIDs := make([]string, 0, len(activeServers))
	for sessionID := range activeServers {
		sessionIDs = append(sessionIDs, sessionID)
	}
	return sessionIDs
}
