// Tests for the CRIU wiring, against a fake criu that records its argv and kills what it dumps, with HOME in a
// temp dir so the snapshots land there. Each test stops what it starts.
//
//	fakeCriu   Also HOME; without a fake, criu is unset.
//	await
//	TestPause   Every answer in one flow: 501, 404, a snapshot, its replacement, and a failed dump.
//	TestResume
package checkpoint

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/install"
	"github.com/cyber-shuttle/linkspan/internal/sessions"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

func fakeCriu(t *testing.T, fake bool) (argvLog string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv")
	script := "#!/bin/sh\necho \"$@\" >> \"" + argvLog + "\"\ncase \"$1\" in dump) kill -9 \"$3\";; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "criu"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	was := criu
	criu = ""
	if fake {
		criu = filepath.Join(dir, "criu")
	}
	t.Cleanup(func() { criu = was })
	return argvLog
}

func await(t *testing.T, id string, state tasks.State) tasks.Task {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if i := slices.IndexFunc(tasks.Select(sessions.Process), func(t tasks.Task) bool { return t.ID == id }); i >= 0 {
			if got := tasks.Select(sessions.Process)[i]; got.State == state {
				return got
			}
		}
	}
	t.Fatalf("%s never reached %s", id, state)
	return tasks.Task{}
}

func TestPause(t *testing.T) {
	fakeCriu(t, false)
	if status, _, msg := pause(context.Background(), nil); status != http.StatusNotImplemented {
		t.Fatalf("pause without criu answered %d %q, want 501", status, msg)
	}
	argvLog := fakeCriu(t, true)
	if status, _, msg := pause(context.Background(), map[string]any{"id": "payload"}); status != http.StatusNotFound {
		t.Fatalf("pausing a session that is not running answered %d %q, want 404", status, msg)
	}
	start := func() (tasks.Task, chan bool) {
		sessions.Start(tasks.Task{Kind: sessions.Process, ID: "payload"}, "sleep", "30")
		t.Cleanup(func() { tasks.Stop("payload") })
		waited := make(chan bool, 1)
		go func() { _, paused := sessions.Wait("payload"); waited <- paused }()
		return await(t, "payload", tasks.StateRunning), waited
	}
	running, waited := start()
	status, body, msg := pause(context.Background(), map[string]any{"id": "payload"})
	if status != http.StatusOK {
		t.Fatalf("pause answered %d %q", status, msg)
	}
	images := filepath.Join(install.Dir(), "checkpoints", "payload")
	if got := body.(map[string]string); got["id"] != "payload" || got["dir"] != images {
		t.Fatalf("pause answered %v, want the payload at %s", got, images)
	}
	if argv, _ := os.ReadFile(argvLog); !strings.Contains(string(argv), "dump -t "+strconv.Itoa(running.Pid)+" --images-dir "+images+".part --shell-job --tcp-established") {
		t.Fatalf("criu ran with %q", argv)
	}
	if !<-waited || len(tasks.Select(sessions.Process)) != 0 {
		t.Fatal("the waiter must be told of the pause and the session forgotten")
	}
	data, err := os.ReadFile(filepath.Join(images, "snapshot"))
	if err != nil || !strings.Contains(string(data), `"pid":`+strconv.Itoa(running.Pid)) {
		t.Fatalf("snapshot = %q, %v; want the pid %d", data, err, running.Pid)
	}
	start()
	if status, _, msg := pause(context.Background(), map[string]any{"id": "payload"}); status != http.StatusOK {
		t.Fatalf("a second pause answered %d %q", status, msg)
	}
	if later, _ := os.ReadFile(filepath.Join(images, "snapshot")); string(later) == string(data) {
		t.Fatal("a second pause must replace the snapshot")
	}
	if err := os.WriteFile(criu, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, waited = start()
	if status, _, _ := pause(context.Background(), map[string]any{"id": "payload"}); status != http.StatusInternalServerError {
		t.Fatalf("a failed pause answered %d, want 500", status)
	}
	folders, _ := filepath.Glob(filepath.Join(install.Dir(), "checkpoints", "*"))
	if len(folders) != 1 || folders[0] != images || len(tasks.Select(sessions.Process)) != 1 {
		t.Fatalf("a failed pause left %v and %d sessions, want the earlier snapshot alone and the session running", folders, len(tasks.Select(sessions.Process)))
	}
	tasks.Stop("payload")
	if <-waited {
		t.Fatal("a stop after a failed pause is not a pause")
	}
}

func TestResume(t *testing.T) {
	argvLog := fakeCriu(t, true)
	if status, _, msg := resume(context.Background(), map[string]any{"id": "gone"}); status != http.StatusNotFound {
		t.Fatalf("a missing snapshot answered %d %q, want 404", status, msg)
	}
	images := filepath.Join(install.Dir(), "checkpoints", "payload")
	if err := os.MkdirAll(images, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(images, "snapshot"), []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`,"stdout":"dev/null","stderr":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	status, body, msg := resume(context.Background(), map[string]any{"id": "payload"})
	if status != http.StatusOK {
		t.Fatalf("resume answered %d %q", status, msg)
	}
	ended := body.(tasks.Task)
	if got := ended.Attrs(ended)["command"]; ended.ID != "payload" || !strings.HasSuffix(got, "criu restore --images-dir "+images+" --shell-job --tcp-established --unprivileged --inherit-fd fd[1]:dev/null") {
		t.Fatalf("the session is %s running %q, want payload running the resume", ended.ID, got)
	}
	if ended.State != tasks.StateExited || ended.Pid != os.Getpid() || len(tasks.Select(sessions.Process)) != 0 {
		t.Fatalf("the resumed session ended as %+v, want exited with the tree's pid %d and forgotten", ended, os.Getpid())
	}
	if argv, _ := os.ReadFile(argvLog); strings.Count(string(argv), "restore --images-dir "+images) != 1 {
		t.Fatalf("criu ran with %q", argv)
	}
}
