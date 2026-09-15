package sessions

import (
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

func TestWait(t *testing.T) {
	created, err := Start(tasks.Task{Kind: "test"}, "true")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.ID, "t-") || created.Attrs(created)["command"] != "true" {
		t.Fatalf("created %+v, want a t- session running true", created)
	}
	ended, paused := Wait(created.ID)
	if ended.State != tasks.StateExited || paused || ended.Pid == 0 || len(tasks.Select("test")) != 0 {
		t.Fatalf("Wait answered %+v paused=%v with %d listed, want exited with its pid and forgotten", ended, paused, len(tasks.Select("test")))
	}
}

func TestPausedEnd(t *testing.T) {
	_, err := Start(tasks.Task{Kind: "test", ID: "named"}, "sleep", "30")
	if err != nil {
		t.Fatal(err)
	}
	Pausing("named", true)
	Pausing("named", false)
	Pausing("named", true)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if l := tasks.Select("test"); len(l) == 1 && l[0].Pid != 0 {
			_ = syscall.Kill(l[0].Pid, syscall.SIGKILL)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session never ran")
		}
	}
	if ended, paused := Wait("named"); !paused || ended.State != tasks.StateFailed {
		t.Fatalf("Wait answered %+v paused=%v, want a paused failed end", ended, paused)
	}
}
