// Tests for the fork path and a spawned server's states. Cancellation reaches the child and its helpers, an orphaned
// pipe cannot hold Exec, and the test binary re-executed with servePort set is the server: it answers on that port
// after an optional delay and exits after an optional lifetime.
//
//	servePort, serveDelay, serveLife  A delay of "exit" exits at once.
//	self, probe, startFake, awaitState
//	TestMain
//	TestStopAllWaitsForTheChild       The child must have exited, not merely been signalled.
//	TestExecKillsTheGroup, TestExecOutlivesAnOrphanedPipe
//	TestServerRunsAndAnswers, TestServerExitsBeforeAnswering, TestServerRunFails, TestServerExitsAfterRunning
package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	servePort  = "LINKSPAN_TEST_SERVE_PORT"
	serveDelay = "LINKSPAN_TEST_SERVE_DELAY"
	serveLife  = "LINKSPAN_TEST_SERVE_LIFE"
)

func self(delay, life string) func(context.Context, int) (*exec.Cmd, error) {
	return func(_ context.Context, port int) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), servePort+"="+strconv.Itoa(port), serveDelay+"="+delay, serveLife+"="+life)
		return cmd, nil
	}
}

func probe(addr string) bool {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func startFake(t *testing.T, p *Task, run func(context.Context, int) (*exec.Cmd, error)) Task {
	t.Helper()
	p.Kind, p.Spawn = "terminal", run
	s, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Stop(s.ID) })
	return s
}

func awaitState(t *testing.T, id string, want State) Task {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		for _, s := range Select("terminal") {
			if s.ID == id && s.State == want {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server %s never reached %s: %+v", id, want, Select("terminal"))
		}
	}
}

func TestMain(m *testing.M) {
	port := os.Getenv(servePort)
	if port == "" {
		os.Exit(m.Run())
	}
	if os.Getenv(serveDelay) == "exit" {
		os.Exit(3)
	}
	if d, _ := time.ParseDuration(os.Getenv(serveDelay)); d > 0 {
		time.Sleep(d)
	}
	if life, _ := time.ParseDuration(os.Getenv(serveLife)); life > 0 {
		time.AfterFunc(life, func() { os.Exit(0) })
	}
	srv := &http.Server{
		Addr:              "127.0.0.1:" + port,
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }),
		ReadHeaderTimeout: time.Second,
	}
	_ = srv.ListenAndServe()
}

func TestStopAllWaitsForTheChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command("sh", "-c", "/usr/bin/touch "+marker+" && /bin/sleep 30")
	_, _ = (&Task{ID: "child", Kind: "workflow", Run: func(ctx context.Context) error { return Exec(ctx, cmd) }}).Start()
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
	_, _ = (&Task{ID: "group", Kind: "workflow", Run: func(ctx context.Context) error { return Exec(ctx, cmd) }}).Start()
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

func TestServerRunsAndAnswers(t *testing.T) {
	old := pollInterval
	pollInterval = 20 * time.Millisecond
	t.Cleanup(func() { pollInterval = old })
	s := startFake(t, &Task{Attrs: func(Task) map[string]string { return map[string]string{"k": "v"} }}, self("200ms", ""))
	wire, _ := json.Marshal(s)
	if s.State != StateStarting || !strings.Contains(string(wire), `"k":"v"`) || s.Kind != "terminal" || s.Addr != "127.0.0.1:"+strings.TrimPrefix(s.ID, "t-") {
		t.Fatalf("created %+v as %s; want starting with the attrs and kind kept and the id by kind and port", s, wire)
	}
	if len(Select("jupyter")) != 0 {
		t.Fatal("listed under another kind")
	}
	running := awaitState(t, s.ID, StateRunning)
	if !probe(running.Addr) {
		t.Fatal("running, but the port does not answer")
	}
	if !Stop(s.ID) || len(Select("terminal")) != 0 {
		t.Fatal("Stop must forget the server")
	}
	if probe(s.Addr) {
		t.Fatal("the server still answers after Stop")
	}
	if Stop(s.ID) {
		t.Fatal("a second Stop must report nothing to stop")
	}
}

func TestServerExitsBeforeAnswering(t *testing.T) {
	s := startFake(t, &Task{}, self("exit", ""))
	if failed := awaitState(t, s.ID, StateFailed); failed.Error == "" {
		t.Fatal("a failed server must carry the reason")
	}
	select {
	case err := <-Failed:
		t.Fatalf("a server's exit must not be fatal, got %v", err)
	default:
	}
}

func TestServerRunFails(t *testing.T) {
	s := startFake(t, &Task{}, func(context.Context, int) (*exec.Cmd, error) { return nil, errors.New("no toolchain") })
	if failed := awaitState(t, s.ID, StateFailed); failed.Error != "no toolchain" || probe(s.Addr) {
		t.Fatalf("got %+v; want the prepare error and no process", failed)
	}
}

func TestServerExitsAfterRunning(t *testing.T) {
	old := pollInterval
	pollInterval = 20 * time.Millisecond
	t.Cleanup(func() { pollInterval = old })
	s := startFake(t, &Task{}, self("", "400ms"))
	awaitState(t, s.ID, StateRunning)
	if exited := awaitState(t, s.ID, StateExited); exited.Error != "" {
		t.Fatalf("a clean exit must carry no error, got %q", exited.Error)
	}
}
