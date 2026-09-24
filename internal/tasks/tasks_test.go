// Tests for a task's life in the registry, the bind and StopAll's promise: when it returns, every task has. Each
// test stops what it starts.
//
//	idle
//	lossyServer, Serve, Close  Close stops nothing, as gliderlabs behaves with no listener.
//	pollFailed
//	TestStopAllSwallowsAndDeregisters, TestErrorReachesFailedByID, TestExitStaysListed, TestStopAllWaitsForTheTask
//	TestRepeatedIDReplaces, TestStopOne, TestStartClosesTheListener
//	TestSocketIsOwnerOnly      The directory avoids t.TempDir because macOS caps socket paths at 104 characters.
package tasks

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func idle(ctx context.Context) error {
	<-ctx.Done()
	return errors.New("idle ended")
}

type lossyServer struct{ closed chan struct{} }

func (s *lossyServer) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		_ = c.Close()
	}
}

func (s *lossyServer) Close() error { close(s.closed); return nil }

func pollFailed() error {
	select {
	case err := <-Failed:
		return err
	default:
		return nil
	}
}

func TestStopAllSwallowsAndDeregisters(t *testing.T) {
	_, _ = (&Task{ID: "stoppable", Kind: "workflow", Run: idle}).Start()
	StopAll()
	if err := pollFailed(); err != nil {
		t.Fatalf("a stopped task reports nothing, got %v", err)
	}
	if len(Select("workflow")) != 0 {
		t.Fatal("a stopped task must leave the registry")
	}
}

func TestErrorReachesFailedByID(t *testing.T) {
	for _, tc := range []struct {
		name string
		task func(context.Context) error
		want string
	}{
		{"returned", func(context.Context) error { return errors.New("boom") }, "returned: boom"},
		{"panicked", func(context.Context) error { panic("handler bug") }, "panicked: panic: handler bug"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { Stop(tc.name) })
			_, _ = (&Task{ID: tc.name, Kind: "workflow", Run: tc.task}).Start()
			select {
			case err := <-Failed:
				if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
					t.Fatalf("Failed delivered %v, want %q", err, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the task never reached Failed")
			}
			if got := Select("workflow"); len(got) != 1 || got[0].State != StateFailed || !strings.HasPrefix(got[0].Error, strings.TrimPrefix(tc.want, tc.name+": ")) {
				t.Fatalf("a failed task must stay listed with its error, got %+v", got)
			}
		})
	}
}

func TestExitStaysListed(t *testing.T) {
	_, _ = (&Task{ID: "done", Kind: "workflow", Run: func(context.Context) error { return nil }}).Start()
	t.Cleanup(func() { Stop("done") })
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		got := Select("workflow")
		if len(got) == 1 && got[0].State == StateExited && got[0].Error == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a finished task must be listed exited until Stop, got %+v", got)
		}
	}
}

func TestStopAllWaitsForTheTask(t *testing.T) {
	returned := make(chan struct{})
	_, _ = (&Task{ID: "slow-exit", Kind: "workflow", Run: func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		close(returned)
		return nil
	}}).Start()
	StopAll()
	select {
	case <-returned:
	default:
		t.Fatal("StopAll returned before the task did")
	}
}

func TestRepeatedIDReplaces(t *testing.T) {
	cancelled := make(chan struct{})
	_, _ = (&Task{ID: "s-repeat", Kind: "sshd", Addr: "127.0.0.1:0", Run: func(ctx context.Context) error {
		<-ctx.Done()
		close(cancelled)
		return nil
	}}).Start()
	second, _ := (&Task{ID: "s-repeat", Kind: "sshd", Addr: "127.0.0.1:0", Run: idle}).Start()
	t.Cleanup(func() { StopAll() })
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the earlier namesake was not cancelled")
	}

	found := Select("sshd")
	if len(found) != 1 || found[0].Addr != second.Addr {
		t.Fatalf("want only the later namesake registered, got %+v", found)
	}
}

func TestStopOne(t *testing.T) {
	_, _ = (&Task{ID: "one", Kind: "workflow", Run: idle}).Start()
	_, _ = (&Task{ID: "two", Kind: "workflow", Run: idle}).Start()
	t.Cleanup(func() { StopAll() })
	if !Stop("one") || Stop("one") {
		t.Fatal("Stop must report a live task once")
	}
	if got := Select("workflow"); len(got) != 1 || got[0].ID != "two" {
		t.Fatalf("after stopping one, %+v remain", got)
	}
}

func TestStartClosesTheListener(t *testing.T) {
	srv := &lossyServer{closed: make(chan struct{})}
	created, err := (&Task{Kind: "http", Server: srv}).Start()
	if err != nil {
		t.Fatal(err)
	}
	StopAll()
	if err := pollFailed(); err != nil {
		t.Fatalf("a stopped server reports nothing, got %v", err)
	}
	if _, err := net.Dial("tcp", created.Addr); err == nil {
		t.Fatal("the listener still accepts after StopAll")
	}
	<-srv.closed
}

func TestSocketIsOwnerOnly(t *testing.T) {
	dir, err := os.MkdirTemp("", "sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "linkspan.sock")
	if err := os.WriteFile(sock, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listen(sock); err == nil {
		t.Fatal("a regular file at the socket path was unlinked and replaced")
	}
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	ln, err := listen(sock)
	if err != nil {
		t.Fatalf("a stale socket was not replaced: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("socket mode %v; group and other must have no access", perm)
	}
}
