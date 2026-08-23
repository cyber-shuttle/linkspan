package process

import (
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// startDetachedProcess starts a process that is not a child of the test
// binary, the way a CRIU-restored task is not a child of linkspan, and
// returns its pid. Its stdio is redirected so the shell's own output pipe
// closes as soon as the pid has been reported.
func startDetachedProcess(t *testing.T, seconds string) int {
	t.Helper()
	out, err := exec.Command("sh", "-c", "sleep "+seconds+" >/dev/null 2>&1 </dev/null & echo $!").Output()
	if err != nil {
		t.Fatalf("failed to start detached process: %v", err)
	}

	var pid int
	if _, err := fmt.Sscan(string(out), &pid); err != nil || pid <= 0 {
		t.Fatalf("failed to parse detached pid from %q: %v", out, err)
	}
	return pid
}

func TestAdoptInvalidPid(t *testing.T) {
	pm := newProcessManager()

	if _, err := pm.Adopt(0); err == nil {
		t.Fatalf("expected pid 0 to be rejected")
	}
	if _, err := pm.Adopt(-1); err == nil {
		t.Fatalf("expected a negative pid to be rejected")
	}
}

func TestAdoptExitedPid(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to run helper process: %v", err)
	}

	if _, err := newProcessManager().Adopt(cmd.Process.Pid); err == nil {
		t.Fatalf("expected adopting an exited pid to fail")
	}
}

func TestAdoptProcess(t *testing.T) {
	pm := newProcessManager()
	pid := startDetachedProcess(t, "30")

	id, err := pm.Adopt(pid)
	if err != nil {
		t.Fatalf("failed to adopt process %d: %v", pid, err)
	}

	proc, err := pm.GetInfo(id)
	if err != nil {
		t.Fatalf("failed to get process info: %v", err)
	}
	if !proc.Adopted {
		t.Fatalf("expected the process to be marked adopted")
	}
	if proc.Cmd.Process.Pid != pid {
		t.Fatalf("got pid %d, want %d", proc.Cmd.Process.Pid, pid)
	}
	if proc.Completed {
		t.Fatalf("process should not be completed yet")
	}

	if err := pm.Kill(id); err != nil {
		t.Fatalf("failed to kill adopted process: %v", err)
	}
}

// The exit of an adopted process must reach Done: that is what
// --shutdown-on-fork-completion waits on after a restore.
func TestAdoptedProcessCompletion(t *testing.T) {
	pm := newProcessManager()

	id, err := pm.Adopt(startDetachedProcess(t, "1"))
	if err != nil {
		t.Fatalf("failed to adopt process: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- pm.Wait(id) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("process completed with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Wait did not return after the adopted process exited")
	}

	proc, err := pm.GetInfo(id)
	if err != nil {
		t.Fatalf("failed to get process info: %v", err)
	}
	if !proc.Completed {
		t.Fatalf("process should be completed")
	}
}

// An adopted process was never reaped by us, so once it has exited its pid may
// already belong to something else and must not be signalled.
func TestKillExitedAdoptedProcessIsNoOp(t *testing.T) {
	pm := newProcessManager()

	id, err := pm.Adopt(startDetachedProcess(t, "1"))
	if err != nil {
		t.Fatalf("failed to adopt process: %v", err)
	}
	if err := pm.Wait(id); err != nil {
		t.Fatalf("process completed with error: %v", err)
	}

	if err := pm.Kill(id); err != nil {
		t.Fatalf("expected killing an exited adopted process to be a no-op, got %v", err)
	}
	if err := pm.Interrupt(id); err != nil {
		t.Fatalf("expected interrupting an exited adopted process to be a no-op, got %v", err)
	}
}

func TestProcessAlive(t *testing.T) {
	if err := ProcessAlive(0); err == nil {
		t.Fatalf("expected pid 0 to be rejected as invalid")
	}

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to run helper process: %v", err)
	}
	if err := ProcessAlive(cmd.Process.Pid); err == nil {
		t.Fatalf("expected an exited pid to be reported as not alive")
	}

	pid := startDetachedProcess(t, "30")
	defer exec.Command("kill", "-9", fmt.Sprint(pid)).Run()
	if err := ProcessAlive(pid); err != nil {
		t.Fatalf("expected a running pid to be reported alive: %v", err)
	}
}
