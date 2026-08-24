package process

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestOutputWhileRunning reads a process's output while it is still writing.
// Under -race this is what catches an unguarded output buffer.
func TestOutputWhileRunning(t *testing.T) {
	id, err := Global.Start(exec.Command("sh", "-c", "echo first; sleep 0.3; echo second"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(Global.KillAll)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, _, err := Global.Output(id)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "second") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("process output never arrived")
}

func TestKillAllStopsAndDeregisters(t *testing.T) {
	id, err := Global.Start(exec.Command("sleep", "60"))
	if err != nil {
		t.Fatal(err)
	}
	Global.KillAll()
	if _, _, err := Global.Output(id); err == nil {
		t.Fatal("expected the process to be deregistered after KillAll")
	}
}

func TestStartRejectsNilAndUnknownIDs(t *testing.T) {
	if _, err := Global.Start(nil); err == nil {
		t.Fatal("expected an error for a nil cmd")
	}
	if err := Global.Kill("p-nope"); err == nil {
		t.Fatal("expected an error for an unknown id")
	}
}
