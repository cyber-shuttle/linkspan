// Package checkpoint is a sidecar over running process sessions: pause writes one into a snapshot with CRIU
// under ~/.cybershuttle/checkpoints, and resume runs the snapshot as a new session, in this job or a later one,
// via the API or a workflow step. A paused session ends, and the step waiting on it answers 202, so a workflow
// pauses its payload under a signal and goes on with the steps after the pause. CRIU is the user's, on PATH,
// allowed to run unprivileged.
//
//	criu       Resolved once; absent, pause and resume answer 501.
//	serial     One pause at a time, since two dumps of one tree would race.
//	criuArgs   A shell job on Linkspan's stdio with its TCP connections; --unprivileged unless root, since the
//	           flag needs CRIU 3.18.
//	snapshots  By ref, from each folder's snapshot file: the folder, the pid, and the stdout and stderr names a
//	           resume hands CRIU, so the tree writes to this job's stdio.
//	check      What pause and resume refuse: no criu, or a ref over more or fewer than one id.
//	pause      The selected running sessions, or all, none unless every selected one is running. The ref defaults
//	           to the session id; a repeated ref replaces the earlier snapshot. The record is written before the
//	           dump, so a failed pause leaves nothing behind. The session ends unless leave_running.
//	resume     The selected snapshots by ref, or all, none unless every one exists. Runs criu as the session, its id
//	           defaulting to the ref and its pid to the tree's, and like shell.exec answers once they end: 202 when
//	           paused again.
//	Commands, Router  pause and resume, each a route.
package checkpoint

import (
	"cmp"
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/cyber-shuttle/linkspan/internal/install"
	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/sessions"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

var criu = install.Which("criu")

var serial sync.Mutex

var criuArgs = func() []string {
	args := []string{"--shell-job", "--tcp-established"}
	if os.Geteuid() != 0 {
		args = append(args, "--unprivileged")
	}
	return args
}()

func snapshots() map[string][]string {
	out := map[string][]string{}
	files, _ := filepath.Glob(filepath.Join(install.Dir(), "checkpoints", "ckpt-*", "snapshot"))
	for _, file := range files {
		if data, err := os.ReadFile(file); err == nil {
			lines := append(strings.Split(string(data), "\n"), "", "", "")
			out[lines[0]] = append([]string{filepath.Dir(file)}, lines[1:4]...)
		}
	}
	return out
}

func check(params map[string]any, ids []string) (int, string) {
	switch {
	case criu == "":
		return http.StatusNotImplemented, "criu is not on PATH"
	case sessions.Ref(params) != "" && len(ids) != 1:
		return http.StatusBadRequest, "ref names one thing; select one id"
	}
	return 0, ""
}

func pause(ctx context.Context, params map[string]any) (int, any, string) {
	serial.Lock()
	defer serial.Unlock()
	ids := sessions.Selected(params)
	if status, msg := check(params, ids); status != 0 {
		return status, nil, msg
	}
	args := criuArgs
	keep, _ := params["leave_running"].(bool)
	if keep {
		args = slices.Concat(args, []string{"--leave-running"})
	}
	running := slices.DeleteFunc(tasks.Select(sessions.Process), func(t tasks.Task) bool {
		return t.State != tasks.StateRunning || (len(ids) > 0 && !slices.Contains(ids, t.ID))
	})
	if len(running) < len(ids) {
		return http.StatusNotFound, nil, fmt.Sprintf("%d of %d selected sessions are not running", len(ids)-len(running), len(ids))
	}
	all, out := snapshots(), []map[string]string{}
	for _, t := range running {
		ref := cmp.Or(sessions.Ref(params), t.ID)
		images := filepath.Join(install.Dir(), "checkpoints", "ckpt-"+strings.ToLower(rand.Text()))
		lines := []string{ref, strconv.Itoa(t.Pid), "", ""}
		for fd := 1; fd <= 2; fd++ {
			if target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", t.Pid, fd)); err == nil {
				lines[fd+1] = strings.TrimPrefix(target, "/")
			}
		}
		earlier := all[ref]
		cmd := exec.Command(criu, append([]string{"dump", "-t", strconv.Itoa(t.Pid), "--images-dir", images}, args...)...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		log.Printf("checkpoint: pausing %s to %s as %s", t.ID, images, ref)
		err := os.MkdirAll(images, 0o700)
		if err == nil {
			err = os.WriteFile(filepath.Join(images, "snapshot"), []byte(strings.Join(lines, "\n")), 0o600)
		}
		if err == nil {
			sessions.Pausing(t.ID, !keep)
			err = tasks.Exec(ctx, cmd)
		}
		if err != nil {
			sessions.Pausing(t.ID, false)
			_ = os.RemoveAll(images)
			return http.StatusInternalServerError, nil, fmt.Sprintf("%s: %v; paused %d of %d", t.ID, err, len(out), len(running))
		}
		if earlier != nil {
			_ = os.RemoveAll(earlier[0])
		}
		out = append(out, map[string]string{"id": t.ID, "snapshot": ref, "dir": images})
	}
	return http.StatusOK, out, ""
}

func resume(_ context.Context, params map[string]any) (int, any, string) {
	all := snapshots()
	ids := sessions.Selected(params)
	if len(ids) == 0 {
		ids = slices.Sorted(maps.Keys(all))
	}
	if status, msg := check(params, ids); status != 0 {
		return status, nil, msg
	}
	for _, ref := range ids {
		if all[ref] == nil {
			return http.StatusNotFound, nil, "no snapshot " + ref
		}
	}
	resumed := []tasks.Task{}
	for _, ref := range ids {
		snapshot := all[ref]
		argv := append([]string{criu, "restore", "--images-dir", snapshot[0]}, criuArgs...)
		for fd := 1; fd <= 2; fd++ {
			if snapshot[fd+1] != "" {
				argv = append(argv, "--inherit-fd", fmt.Sprintf("fd[%d]:%s", fd, snapshot[fd+1]))
			}
		}
		pid, _ := strconv.Atoi(snapshot[1])
		created, err := sessions.Start(tasks.Task{Kind: sessions.Process, ID: cmp.Or(sessions.Ref(params), ref), Pid: pid}, argv...)
		if err != nil {
			return http.StatusInternalServerError, nil, err.Error()
		}
		resumed = append(resumed, created)
	}
	status, failed := http.StatusOK, ""
	for i, t := range resumed {
		ended, paused := sessions.Wait(t.ID)
		resumed[i] = ended
		switch {
		case paused:
			status = http.StatusAccepted
		case ended.State != tasks.StateExited:
			failed = cmp.Or(failed, t.ID+": "+ended.Error)
		}
	}
	if failed != "" {
		return http.StatusInternalServerError, nil, fmt.Sprintf("%s; %d resumed", failed, len(resumed))
	}
	return status, resumed, ""
}

var Commands = map[string]router.Command{
	"pause":  pause,
	"resume": resume,
}

var Router = router.New("/checkpoint", map[string]router.Command{
	"POST /pause":  Commands["pause"],
	"POST /resume": Commands["resume"],
})
