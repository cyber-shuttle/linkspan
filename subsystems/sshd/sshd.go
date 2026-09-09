// Package sshd is the SSH server VS Code Remote-SSH connects to: one public
// key per server, commands run as the job's user. Every handler and goroutine
// recovers from panics, so a client cannot bring Linkspan down. A stopped
// server is not replaced.
//
//	Server                   Its Start registers the bound server under procmgr
//	                         as s-<port>.
//	guard                    It is the one recover body; callers start their own
//	                         goroutine.
//	runCommand               It runs a command on the session and reports the
//	                         exit status VS Code reads. Stdin is a pipe copied by
//	                         hand, since a client that never closes stdin would
//	                         hold Wait open after the child exits.
//	handleDirectStreamLocal  It forwards a channel to a unix socket. The payload
//	                         mirrors x/crypto/ssh's unexported
//	                         streamLocalChannelOpenDirectMsg.
//	newServer                It builds a server for one key with no PTY, local
//	                         forwarding only, and sh running either the
//	                         requested command or the commands on stdin, as
//	                         OpenSSH does for a session without a PTY. The
//	                         handler tables and the key callback are wrapped by
//	                         iteration so no entry is left unguarded; the pty
//	                         and forwarding callbacks return constants.
//	New                      It binds a loopback port for one key; the port
//	                         accepts before it returns.
package sshd

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

	"github.com/cyber-shuttle/linkspan/internal/procmgr"
	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

const (
	exitNeverRan  = 127
	exitSignalled = 255
)

type Server struct {
	ID   string
	Port int
	ln   net.Listener
	srv  *ssh.Server
}

func guard(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("sshd: recovered panic in %s: %v\n%s", name, r, debug.Stack())
		}
	}()
	fn()
}

func runCommand(ctx context.Context, s ssh.Session, cmd *exec.Cmd) {
	stderr := s.Stderr()
	cmd.Env, cmd.Stdout, cmd.Stderr = os.Environ(), s, stderr

	stdin, _ := cmd.StdinPipe()
	go guard("session stdin copy", func() { defer func() { _ = stdin.Close() }(); _, _ = io.Copy(stdin, s) })
	err := procmgr.Exec(ctx, cmd)

	code := exitNeverRan
	switch {
	case cmd.ProcessState == nil:
		_, _ = fmt.Fprintf(stderr, "command error: %v\n", err)
	case cmd.ProcessState.ExitCode() < 0:
		code = exitSignalled
	default:
		code = cmd.ProcessState.ExitCode()
	}
	_ = s.Exit(code)
}

func handleDirectStreamLocal(_ *ssh.Server, _ *gossh.ServerConn, newChan gossh.NewChannel, _ ssh.Context) {
	var req struct {
		SocketPath string
		Reserved0  string
		Reserved1  uint32
	}
	if err := gossh.Unmarshal(newChan.ExtraData(), &req); err != nil {
		_ = newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}
	log.Printf("sshd: streamlocal forward to %s", req.SocketPath)
	sock, err := net.Dial("unix", req.SocketPath)
	if err != nil {
		_ = newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := newChan.Accept()
	if err != nil {
		_ = sock.Close()
		return
	}
	go guard("streamlocal requests", func() { gossh.DiscardRequests(reqs) })
	closeBoth := sync.OnceFunc(func() { _ = ch.Close(); _ = sock.Close() })
	go guard("streamlocal copy", func() { defer closeBoth(); _, _ = io.Copy(sock, ch) })
	go guard("streamlocal copy", func() { defer closeBoth(); _, _ = io.Copy(ch, sock) })
}

func newServer(key ssh.PublicKey) *ssh.Server {
	srv := &ssh.Server{
		Handler: func(s ssh.Session) {
			user, remote := s.User(), s.RemoteAddr()
			log.Printf("sshd: connected user=%s remote=%s", user, remote)
			defer log.Printf("sshd: disconnected user=%s remote=%s", user, remote)

			args := []string{"-s"}
			if len(s.Command()) > 0 {
				log.Printf("sshd: exec user=%s remote=%s cmd=%q", user, remote, s.RawCommand())
				args = []string{"-c", s.RawCommand()}
			}
			runCommand(s.Context(), s, exec.Command("sh", args...))
		},
		PublicKeyHandler: func(_ ssh.Context, offered ssh.PublicKey) bool {
			return ssh.KeysEqual(offered, key)
		},
		PtyCallback: func(ssh.Context, ssh.Pty) bool { return false },
		LocalPortForwardingCallback: func(_ ssh.Context, host string, port uint32) bool {
			log.Printf("sshd: port forward to %s:%d", host, port)
			return true
		},
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":                        ssh.DefaultSessionHandler,
			"direct-tcpip":                   ssh.DirectTCPIPHandler,
			"direct-streamlocal@openssh.com": handleDirectStreamLocal,
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": func(s ssh.Session) {
				server, err := sftp.NewServer(s)
				if err == nil {
					err = server.Serve()
				}
				if err != nil && !errors.Is(err, io.EOF) {
					log.Printf("sshd: sftp: %v", err)
				}
			},
		},
	}
	session, auth := srv.Handler, srv.PublicKeyHandler
	srv.Handler = func(s ssh.Session) { guard("handler session", func() { session(s) }) }
	srv.PublicKeyHandler = func(ctx ssh.Context, k ssh.PublicKey) (ok bool) {
		guard("public key", func() { ok = auth(ctx, k) })
		return ok
	}
	for name, h := range srv.ChannelHandlers {
		srv.ChannelHandlers[name] = func(s *ssh.Server, c *gossh.ServerConn, nc gossh.NewChannel, ctx ssh.Context) {
			guard("channel "+name, func() { h(s, c, nc, ctx) })
		}
	}
	for name, h := range srv.SubsystemHandlers {
		srv.SubsystemHandlers[name] = func(s ssh.Session) { guard("handler "+name, func() { h(s) }) }
	}
	return srv
}

func New(key ssh.PublicKey) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("sshd: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return &Server{ID: fmt.Sprintf("s-%d", port), Port: port, ln: ln, srv: newServer(key)}, nil
}

func (s *Server) Start() {
	log.Printf("sshd: %s listening on %s", s.ID, s.ln.Addr())
	procmgr.Start(procmgr.KindSSHD, s.ID, s.ln.Addr().String(), procmgr.Serve(s.ln, s.srv))
}
