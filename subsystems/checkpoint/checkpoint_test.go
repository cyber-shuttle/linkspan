// Tests for the CRIU wiring, against a fake criu that records its argv and kills what it dumps, with HOME in a
// temp dir so the snapshots land there. Each test stops what it starts.
//
//	fakeCriu   Also HOME; without a fake, criu is unset.
//	await
//	TestPause   Every branch in one flow, a failed dump among them.
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
	script := "#!/bin/sh\necho \"$@\" >> \"" + argvLog + "\"\ncase \"$*\" in *--leave-running*) ;; dump*) kill -9 \"$3\";; esac\n"
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
	created, err := sessions.Start(tasks.Task{Kind: sessions.Process}, "sleep", "30")
	if err != nil {
		t.Fatal(err)
	}
	id := created.ID
	t.Cleanup(func() { tasks.Stop(id) })
	waited := make(chan bool, 1)
	go func() { _, paused := sessions.Wait(id); waited <- paused }()
	running := await(t, id, tasks.StateRunning)
	params := map[string]any{"leave_running": true}
	status, body, msg := pause(context.Background(), params)
	if status != http.StatusOK {
		t.Fatalf("pause answered %d %q", status, msg)
	}
	got := body.([]map[string]string)
	if len(got) != 1 || got[0]["id"] != id || got[0]["snapshot"] != id || !strings.HasPrefix(filepath.Base(got[0]["dir"]), "ckpt-") {
		t.Fatalf("pause answered %v, want one ckpt- folder named by the session", got)
	}
	images := got[0]["dir"]
	if argv, _ := os.ReadFile(argvLog); !strings.Contains(string(argv), "dump -t "+strconv.Itoa(running.Pid)+" --images-dir "+images+" --shell-job --tcp-established") || !strings.Contains(string(argv), "--leave-running") {
		t.Fatalf("criu ran with %q", argv)
	}
	if all := snapshots(); all[id][0] != images || all[id][1] != strconv.Itoa(running.Pid) {
		t.Fatalf("snapshots = %v, want %s at %s with pid %d", all, id, images, running.Pid)
	}
	params["ids"] = []any{id}
	if status, _, _ := pause(context.Background(), params); status != http.StatusOK || len(snapshots()) != 1 || snapshots()[id][0] == images {
		t.Fatalf("a repeated ref must replace the earlier snapshot, answered %d with %v", status, snapshots())
	}
	params["ref"] = "named"
	if status, _, _ := pause(context.Background(), params); status != http.StatusOK || snapshots()["named"] == nil {
		t.Fatalf("a ref answered %d, snapshots %v, want one named", status, snapshots())
	}
	delete(params, "ids")
	if status, _, msg := pause(context.Background(), params); status != http.StatusBadRequest {
		t.Fatalf("a ref without one id answered %d %q, want 400", status, msg)
	}
	delete(params, "ref")
	params["ids"] = []any{id, "p-0"}
	if status, _, msg := pause(context.Background(), params); status != http.StatusNotFound || !strings.Contains(msg, "1 of 2") {
		t.Fatalf("a set with an unknown id answered %d %q, want 404", status, msg)
	}
	if err := os.WriteFile(criu, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := pause(context.Background(), map[string]any{"ids": []any{id}}); status != http.StatusInternalServerError || len(snapshots()) != 2 {
		t.Fatalf("a failed pause answered %d and left %d snapshots, want 500 and the two before", status, len(snapshots()))
	}
	if folders, _ := filepath.Glob(filepath.Join(install.Dir(), "checkpoints", "ckpt-*")); len(folders) != 2 {
		t.Fatalf("a failed pause left %d folders, want the two snapshots alone", len(folders))
	}
	if err := os.WriteFile(criu, []byte("#!/bin/sh\nkill -9 \"$3\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if status, _, msg := pause(context.Background(), map[string]any{"ids": []any{id}}); status != http.StatusOK || !<-waited || len(tasks.Select(sessions.Process)) != 0 {
		t.Fatalf("a pause without leave_running answered %d %q, want 200, the waiter told of the pause and the session forgotten", status, msg)
	}
}

func TestResume(t *testing.T) {
	argvLog := fakeCriu(t, true)
	if status, _, msg := resume(context.Background(), map[string]any{"ids": []any{"gone"}}); status != http.StatusNotFound {
		t.Fatalf("a missing snapshot answered %d %q, want 404", status, msg)
	}
	folders := map[string]string{}
	for ref, folder := range map[string]string{"one": "ckpt-1", "two": "ckpt-2"} {
		folders[ref] = filepath.Join(install.Dir(), "checkpoints", folder)
		if err := os.MkdirAll(folders[ref], 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folders[ref], "snapshot"), []byte(ref+"\n"+strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resumeAll := func(params map[string]any, want int) []tasks.Task {
		status, body, msg := resume(context.Background(), params)
		if status != http.StatusOK {
			t.Fatalf("resume answered %d %q", status, msg)
		}
		got := body.([]tasks.Task)
		if len(got) != want || len(tasks.Select(sessions.Process)) != 0 {
			t.Fatalf("resume answered %d sessions with %d still listed, want %d and none", len(got), len(tasks.Select(sessions.Process)), want)
		}
		return got
	}
	created := resumeAll(map[string]any{"ids": []any{"one"}, "ref": "payload"}, 1)[0]
	if got := created.Attrs(created)["command"]; created.ID != "payload" || !strings.HasSuffix(got, "criu restore --images-dir "+folders["one"]+" --shell-job --tcp-established --unprivileged") {
		t.Fatalf("the session is %s running %q, want payload running the resume", created.ID, got)
	}
	if created.State != tasks.StateExited || created.Pid != os.Getpid() {
		t.Fatalf("the resumed session ended as %+v, want exited with the tree's pid %d", created, os.Getpid())
	}
	if status, _, msg := resume(context.Background(), map[string]any{"ref": "payload"}); status != http.StatusBadRequest {
		t.Fatalf("a ref over two snapshots answered %d %q, want 400", status, msg)
	}
	resumeAll(map[string]any{"ids": []any{"one", "two"}}, 2)
	resumeAll(map[string]any{}, 2)
	if argv, _ := os.ReadFile(argvLog); strings.Count(string(argv), "restore --images-dir "+folders["one"]) != 3 || strings.Count(string(argv), "restore --images-dir "+folders["two"]) != 2 {
		t.Fatalf("criu ran with %q", argv)
	}
}
