// Tests for the registry and for StopAll's promise: when it returns, every
// task has. Each test stops what it starts.
//
//	idle                               It lives until cancelled.
//	lossyServer                        Its Close stops nothing, as gliderlabs
//	                                   behaves with no listener.
//	pollFailed                         It returns what Failed holds now, or nil.
//	TestStopAllSwallowsAndDeregisters  A stopped task's error must not reach
//	                                   Failed, and the task must leave the
//	                                   registry.
//	TestErrorReachesFailedByID         The error must arrive prefixed by the id,
//	                                   whether the task returned it or panicked.
//	TestStopAllWaitsForTheTask         The call must not return before the task
//	                                   has.
//	TestRepeatedIDReplaces             A second Start with the same id must
//	                                   replace the first, which must be
//	                                   cancelled.
//	TestServeReturnsOnStopAll          The task must return on StopAll even when
//	                                   the server's own Close is dropped.
//	TestStopAllWaitsForTheChild        The child must have exited, not merely
//	                                   been signalled, when StopAll returns.
//	TestExecKillsTheGroup              A helper the child left in its process
//	                                   group must die with it.
//	TestExecOutlivesAnOrphanedPipe     A child that exits leaving an orphan on
//	                                   its stdout must not hold Exec open past
//	                                   stdioGrace.
package procmgr

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	Start(KindWorkflow, "stoppable", "x", idle)
	StopAll()
	if err := pollFailed(); err != nil {
		t.Fatalf("a stopped process reports nothing, got %v", err)
	}
	if len(Running(KindWorkflow)) != 0 {
		t.Fatal("a stopped process must leave the registry")
	}
}

func TestErrorReachesFailedByID(t *testing.T) {
	for _, tc := range []struct {
		name string
		task Task
		want string
	}{
		{"returned", func(context.Context) error { return errors.New("boom") }, "returned: boom"},
		{"panicked", func(context.Context) error { panic("handler bug") }, "panicked: panic: handler bug"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Start(KindWorkflow, tc.name, "x", tc.task)
			select {
			case err := <-Failed:
				if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
					t.Fatalf("Failed delivered %v, want %q", err, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the process never reached Failed")
			}
			if len(Running(KindWorkflow)) != 0 {
				t.Fatal("a finished process must leave the registry")
			}
		})
	}
}

func TestStopAllWaitsForTheTask(t *testing.T) {
	returned := make(chan struct{})
	Start(KindWorkflow, "slow-exit", "x", func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		close(returned)
		return nil
	})
	StopAll()
	select {
	case <-returned:
	default:
		t.Fatal("StopAll returned before the task did")
	}
}

func TestRepeatedIDReplaces(t *testing.T) {
	cancelled := make(chan struct{})
	Start(KindSSHD, "s-repeat", "first-addr", func(ctx context.Context) error {
		<-ctx.Done()
		close(cancelled)
		return nil
	})
	Start(KindSSHD, "s-repeat", "second-addr", idle)
	t.Cleanup(func() { StopAll() })
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the earlier namesake was not cancelled")
	}

	found := Running(KindSSHD)
	if len(found) != 1 || found[0].Addr != "second-addr" {
		t.Fatalf("want only the later namesake registered, got %+v", found)
	}
}

func TestServeReturnsOnStopAll(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &lossyServer{closed: make(chan struct{})}
	Start(KindHTTP, "served", ln.Addr().String(), Serve(ln, srv))
	StopAll()
	if err := pollFailed(); err != nil {
		t.Fatalf("a stopped server reports nothing, got %v", err)
	}
	if _, err := net.Dial("tcp", ln.Addr().String()); err == nil {
		t.Fatal("the listener still accepts after StopAll")
	}
	<-srv.closed
}

func TestStopAllWaitsForTheChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command("sh", "-c", "/usr/bin/touch "+marker+" && /bin/sleep 30")
	Start(KindWorkflow, "child", "", func(ctx context.Context) error { return Exec(ctx, cmd) })
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never started")
		}
	}
	StopAll()
	if cmd.ProcessState == nil {
		t.Fatal("StopAll returned while the child was still running")
	}
	if err := pollFailed(); err != nil {
		t.Fatalf("a stopped child reports nothing, got %v", err)
	}
}

func TestExecKillsTheGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.Command("sh", "-c", "/bin/sleep 30 & echo $! > "+pidFile+"; wait")
	Start(KindWorkflow, "group", "", func(ctx context.Context) error { return Exec(ctx, cmd) })
	var pid int
	for deadline := time.Now().Add(5 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper never started")
		}
	}
	StopAll()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the helper outlived the child it was started by")
		}
	}
}

func TestExecOutlivesAnOrphanedPipe(t *testing.T) {
	old := stdioGrace
	stdioGrace = 100 * time.Millisecond
	t.Cleanup(func() { stdioGrace = old })
	cmd := exec.Command("sh", "-c", "/bin/sleep 5 & exit 0")
	cmd.Stdout = &bytes.Buffer{}
	start := time.Now()
	_ = Exec(context.Background(), cmd)
	if elapsed := time.Since(start); elapsed > stdioGrace+2*time.Second {
		t.Fatalf("Exec took %s, want it bounded near %s", elapsed, stdioGrace)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
