package fork

import (
	"log"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/process"
)

func TestForkProcess(t *testing.T) {
	fm := GlobalForkProcessManager

	fp, err := fm.RunForkProcess("echo 'Hello, World!' && sleep 2")
	if err != nil {
		t.Fatalf("failed to run fork process: %v", err)
	}

	if fp == nil {
		t.Fatalf("fork process is nil")
	}

	got, err := fm.GetForkProcess(fp.InternalProcessId)
	if err != nil {
		t.Fatalf("failed to get fork process: %v", err)
	}
	if got != fp {
		t.Fatalf("got fork process %v, want %v", got, fp)
	}

	proc, err := process.GlobalProcessManager.GetInfo(got.InternalProcessId)
	if err != nil {
		t.Fatalf("failed to get process info: %v", err)
	}

	if proc.Completed {
		t.Fatalf("process should not be completed yet")
	}

	// sleep for a short duration to ensure the process is still running
	time.Sleep(3 * time.Second)
	proc, err = process.GlobalProcessManager.GetInfo(got.InternalProcessId)

	if err != nil {
		t.Fatalf("failed to get process info: %v", err)
	}

	if !proc.Completed {
		log.Printf("process is still running, waiting for it to complete...")
		log.Printf("%+v", proc)
		t.Fatalf("process should be completed")
	}

	defer func() {
		err := fm.RemoveForkProcess(fp.InternalProcessId)
		if err != nil {
			t.Fatalf("failed to remove fork process: %v", err)
		}
	}()
}

func TestForkProcessCleanup(t *testing.T) {
	fm := GlobalForkProcessManager

	fp, err := fm.RunForkProcess("echo 'Hello, World!' && sleep 5")
	if err != nil {
		t.Fatalf("failed to run fork process: %v", err)
	}

	if fp == nil {
		t.Fatalf("fork process is nil")
	}

	err = fm.KillAllForkProcesses()
	if err != nil {
		t.Fatalf("failed to kill all fork processes: %v", err)
	}

	_, err = fm.GetForkProcess(fp.InternalProcessId)
	if err == nil {
		t.Fatalf("expected error when getting removed fork process, got nil")
	}
}
