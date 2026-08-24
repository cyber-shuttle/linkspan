package process

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Under -race, this is what catches an unguarded output buffer.
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

func TestExitedTracksTheProcess(t *testing.T) {
	id, err := Global.Start(exec.Command("sh", "-c", "sleep 0.3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(Global.KillAll)

	if Global.Exited(id) {
		t.Fatal("a just-started process should not report exited")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !Global.Exited(id) {
		time.Sleep(10 * time.Millisecond)
	}
	if !Global.Exited(id) {
		t.Fatal("process never reported exited")
	}
	if !Global.Exited("p-nope") {
		t.Fatal("an unknown id has nothing running under it, so it counts as exited")
	}
}
