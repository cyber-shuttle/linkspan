// Tests for what a client can and cannot make the server do, and for the exit status VS Code reads.
//
//	captureSession, blockingStdinSession  Fake sessions with the ssh.Session methods runCommand touches: one records
//	                                      the exit status, the other never closes stdin.
//	exitCode
//	dial, keyPair, serve, connect
//	TestPanicIsolation                    A panic in any handler New installs must be recovered.
//	TestStreamLocalForwardAndTeardown     The socket directory avoids t.TempDir because macOS caps socket paths at 104
//	                                      characters.
//	TestRunCommand*                       The status is the child's own code, 255 when signalled and 127 when it
//	                                      never ran, and a client that never closes stdin must not delay it.
//	TestExecRequestRunsTheCommand, TestRejectsOtherKeys, TestLocalForwardOnly, TestPtyIsRefused
package sshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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

type captureSession struct {
	ssh.Session
	mu   sync.Mutex
	code int
}

type blockingStdinSession struct {
	captureSession
	release chan struct{}
}

func (c *captureSession) exitCode() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code
}

func (c *captureSession) Read([]byte) (int, error) { return 0, io.EOF }

func (c *captureSession) Write(p []byte) (int, error) { return len(p), nil }

func (c *captureSession) Stderr() io.ReadWriter { return c }

func (c *captureSession) Exit(code int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.code = code
	return nil
}

func dial(t *testing.T, addr string, hostKey ssh.PublicKey, signer gossh.Signer) (*gossh.Client, error) {
	t.Helper()
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "t",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.FixedHostKey(hostKey),
		Timeout:         5 * time.Second,
	})
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

func keyPair(t *testing.T) (gossh.Signer, ssh.PublicKey) {
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

func serve(t *testing.T, key ssh.PublicKey) (string, ssh.PublicKey) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, hostKey := keyPair(t)
	srv := New(key)
	srv.AddHostKey(hostSigner)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), hostKey
}

func connect(t *testing.T) *gossh.Client {
	t.Helper()
	signer, key := keyPair(t)
	addr, hostKey := serve(t, key)
	client, err := dial(t, addr, hostKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (b *blockingStdinSession) Read([]byte) (int, error) { <-b.release; return 0, io.EOF }

func TestPanicIsolation(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic escaped a guard (would crash Linkspan): %v", r)
		}
	}()
	_, key := keyPair(t)
	srv := New(key)
	srv.Handler(nil)
	srv.ChannelHandlers["direct-streamlocal@openssh.com"](nil, nil, nil, nil)
	srv.SubsystemHandlers["sftp"](nil)
}

func TestStreamLocalForwardAndTeardown(t *testing.T) {
	dir, err := os.MkdirTemp("", "sl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ul, err := net.Listen("unix", filepath.Join(dir, "e.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ul.Close() })

	echoed := make(chan error, 1)
	go func() {
		c, err := ul.Accept()
		if err != nil {
			echoed <- err
			return
		}
		defer func() { _ = c.Close() }()
		_, err = io.Copy(c, c)
		echoed <- err
	}()

	ch, reqs, err := connect(t).OpenChannel("direct-streamlocal@openssh.com",
		gossh.Marshal(struct {
			SocketPath string
			Reserved0  string
			Reserved1  uint32
		}{SocketPath: ul.Addr().String()}))
	if err != nil {
		t.Fatal(err)
	}
	go gossh.DiscardRequests(reqs)

	if _, err := ch.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(ch, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo round-trip failed: err=%v got=%q", err, buf)
	}

	_ = ch.Close()
	select {
	case err := <-echoed:
		if err != nil {
			t.Fatalf("the backing socket ended with %v, want a clean close", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the backing socket outlived the closed channel, so the forward leaked")
	}
}

func TestRunCommandExitStatuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  []string
		want int
	}{
		{"the child's own code", []string{"sh", "-c", "exit 42"}, 42},
		{"signalled, which has no code of its own", []string{"sh", "-c", "kill -TERM $$"}, exitSignalled},
		{"never ran at all", []string{"/nonexistent/binary"}, exitNeverRan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &captureSession{}
			runCommand(context.Background(), c, exec.Command(tc.cmd[0], tc.cmd[1:]...))
			if got := c.exitCode(); got != tc.want {
				t.Fatalf("exit status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRunCommandReturnsWithStdinOpen(t *testing.T) {
	c := &blockingStdinSession{release: make(chan struct{})}
	t.Cleanup(func() { close(c.release) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		runCommand(context.Background(), c, exec.Command("sh", "-c", "echo hi"))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runCommand blocked on a client that never closed its stdin")
	}
	if c.exitCode() != 0 {
		t.Fatalf("exit status = %d, want 0", c.exitCode())
	}
}

func TestExecRequestRunsTheCommand(t *testing.T) {
	client := connect(t)
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	out, err := session.Output("echo linkspan-exec-marker")
	if err != nil {
		t.Fatalf("exec request failed: %v", err)
	}
	if !strings.Contains(string(out), "linkspan-exec-marker") {
		t.Fatalf("exec output = %q, want the command's own output", out)
	}
}

func TestRejectsOtherKeys(t *testing.T) {
	authorizedSigner, key := keyPair(t)
	strangerSigner, _ := keyPair(t)
	addr, hostKey := serve(t, key)

	if _, err := dial(t, addr, hostKey, strangerSigner); err == nil {
		t.Fatal("a key the server was never given was accepted")
	}
	if _, err := dial(t, addr, hostKey, authorizedSigner); err != nil {
		t.Fatalf("the authorized key was rejected: %v", err)
	}
}

func TestLocalForwardOnly(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		if c, err := echo.Accept(); err == nil {
			defer func() { _ = c.Close() }()
			_, _ = io.Copy(c, c)
		}
	}()

	client := connect(t)
	forwarded, err := client.Dial("tcp", echo.Addr().String())
	if err != nil {
		t.Fatalf("a local port forward was refused: %v", err)
	}
	t.Cleanup(func() { _ = forwarded.Close() })

	if _, err := io.WriteString(forwarded, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(forwarded, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("forwarded round-trip failed: err=%v got=%q", err, buf)
	}

	if remote, err := client.Listen("tcp", "127.0.0.1:0"); err == nil {
		_ = remote.Close()
		t.Fatal("a reverse port forward was granted")
	}
}

func TestPtyIsRefused(t *testing.T) {
	session, err := connect(t).NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if err := session.RequestPty("xterm", 24, 80, gossh.TerminalModes{}); err == nil {
		t.Fatal("a pty was granted")
	}
}
