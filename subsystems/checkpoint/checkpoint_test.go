// Tests for the session's life and the CRIU wiring, with a fake criu so no kernel support is needed. Each test
// stops what it starts.
//
//	exited      What exit sends, so a test sees Linkspan asked to stop without a signal.
//	init
//	fakeCriu    An executable that records its argv and exits 0.
//	await       The session by id once it reaches state, within 5s.
//	startOne
//	TestSessionRunsAndStopsLinkspan   A command that ends on its own is listed exited with its pid, and asks Linkspan to
//	                                   stop only with stop_on_exit.
//	TestStopDoesNotStopLinkspan        A session Stop ends did so at the caller's request, so nothing stops Linkspan.
//	TestDumpWithoutCRIU          501 when the binary is absent, for dump and restore alike.
//	TestDumpTakesTheRunning      The running session's pid reaches criu dump under the images root; an unknown
//	                                   id is 404 and an empty registry dumps nothing.
//	TestRestoreIsASession              The restore runs as a session whose command is the criu invocation.
package checkpoint

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

var exited = make(chan struct{}, 8)

func init() { exit = func() { exited <- struct{}{} } }

func fakeCriu(t *testing.T) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	bin, argvLog = filepath.Join(dir, "criu"), filepath.Join(dir, "argv")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$@\" >> \""+argvLog+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog
}

func await(t *testing.T, id string, state tasks.State) tasks.Task {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if i := slices.IndexFunc(tasks.Select(kind), func(t tasks.Task) bool { return t.ID == id }); i >= 0 {
			if got := tasks.Select(kind)[i]; got.State == state {
				return got
			}
		}
	}
	t.Fatalf("%s never reached %s", id, state)
	return tasks.Task{}
}

func startOne(t *testing.T, command string, stopOnExit bool) tasks.Task {
	t.Helper()
	status, body, msg := startSession(context.Background(), map[string]any{"command": command, "stop_on_exit": stopOnExit})
	if status != http.StatusCreated {
		t.Fatalf("start answered %d %q", status, msg)
	}
	created := body.(tasks.Task)
	t.Cleanup(func() { tasks.Stop(created.ID) })
	return created
}

func TestSessionRunsAndStopsLinkspan(t *testing.T) {
	for _, stopOnExit := range []bool{false, true} {
		t.Run(strconv.FormatBool(stopOnExit), func(t *testing.T) {
			created := startOne(t, "exit 0", stopOnExit)
			if !strings.HasPrefix(created.ID, "p-") || created.State != tasks.StateStarting {
				t.Fatalf("created %+v, want a starting p- session", created)
			}
			ended := await(t, created.ID, tasks.StateExited)
			if ended.Pid == 0 {
				t.Fatalf("an ended session keeps its pid, got %+v", ended)
			}
			select {
			case <-exited:
				if !stopOnExit {
					t.Fatal("Linkspan was asked to stop without stop_on_exit")
				}
			case <-time.After(time.Second):
				if stopOnExit {
					t.Fatal("Linkspan was not asked to stop")
				}
			}
		})
	}
}

func TestStopDoesNotStopLinkspan(t *testing.T) {
	created := startOne(t, "sleep 30", true)
	await(t, created.ID, tasks.StateRunning)
	tasks.Stop(created.ID)
	select {
	case <-exited:
		t.Fatal("a stopped session must not stop Linkspan")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestDumpWithoutCRIU(t *testing.T) {
	params := map[string]any{"criu": filepath.Join(t.TempDir(), "criu"), "images_dir": t.TempDir()}
	if status, _, msg := dump(context.Background(), params); status != http.StatusNotImplemented {
		t.Fatalf("dump answered %d %q, want 501", status, msg)
	}
	if status, _, msg := restore(context.Background(), params); status != http.StatusNotImplemented {
		t.Fatalf("restore answered %d %q, want 501", status, msg)
	}
}

func TestDumpTakesTheRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake criu is a shell script")
	}
	bin, argvLog := fakeCriu(t)
	root := t.TempDir()
	params := map[string]any{"criu": bin, "images_dir": root}
	if status, body, msg := dump(context.Background(), params); status != http.StatusOK || len(body.([]map[string]string)) != 0 {
		t.Fatalf("an empty registry answered %d %v %q, want 200 []", status, body, msg)
	}
	created := startOne(t, "sleep 30", false)
	running := await(t, created.ID, tasks.StateRunning)
	status, body, msg := dump(context.Background(), params)
	if status != http.StatusOK {
		t.Fatalf("dump answered %d %q", status, msg)
	}
	want := []map[string]string{{"id": created.ID, "images_dir": filepath.Join(root, created.ID)}}
	if got := body.([]map[string]string); !slices.EqualFunc(got, want, func(a, b map[string]string) bool { return a["id"] == b["id"] && a["images_dir"] == b["images_dir"] }) {
		t.Fatalf("dump answered %v, want %v", got, want)
	}
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "dump -t "+strconv.Itoa(running.Pid)+" --images-dir "+want[0]["images_dir"]+" --shell-job") {
		t.Fatalf("criu ran with %q", argv)
	}
	if _, err := os.Stat(want[0]["images_dir"]); err != nil {
		t.Fatalf("the images directory was not created: %v", err)
	}
	params["id"] = "p-0"
	if status, _, msg := dump(context.Background(), params); status != http.StatusNotFound {
		t.Fatalf("an unknown id answered %d %q, want 404", status, msg)
	}
}

func TestRestoreIsASession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake criu is a shell script")
	}
	bin, argvLog := fakeCriu(t)
	if status, _, msg := restore(context.Background(), map[string]any{"criu": bin}); status != http.StatusBadRequest {
		t.Fatalf("a restore without images_dir answered %d %q, want 400", status, msg)
	}
	images := t.TempDir()
	status, body, msg := restore(context.Background(), map[string]any{"criu": bin, "images_dir": images})
	if status != http.StatusCreated {
		t.Fatalf("restore answered %d %q", status, msg)
	}
	created := body.(tasks.Task)
	t.Cleanup(func() { tasks.Stop(created.ID) })
	if got := created.Attrs(created)["command"]; got != bin+" restore --images-dir "+images+" --shell-job --tcp-established --unprivileged" {
		t.Fatalf("the session's command is %q", got)
	}
	await(t, created.ID, tasks.StateExited)
	if argv, _ := os.ReadFile(argvLog); !strings.Contains(string(argv), "restore --images-dir "+images) {
		t.Fatalf("criu ran with %q", argv)
	}
}
