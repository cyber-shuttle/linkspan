// Package checkpoint runs a command as a session and checkpoints it with CRIU. A command posted to
// /api/v1/checkpoint/sessions runs under sh with Linkspan's stdio and is listed until it ends; with stop_on_exit its
// end stops Linkspan, so a batch job ends with its payload and the workflow's stop steps run. A checkpoint dumps
// a running session's process tree to an images directory, which ends the session, and a restore runs a dumped
// tree as a new session, so a workflow checkpoints ahead of Slurm's time limit and the next job resumes. CRIU is
// the user's, on PATH or named per request; it runs unprivileged, so the kernel must allow that.
//
//	kind
//	criuArgs      What dump and restore share: a shell job, since the tree leads a process group on Linkspan's
//	              stdio, with its TCP connections, and no root.
//	exit          Stops Linkspan as a signal does; a test swaps it.
//	start         A session running argv, listed as command, with the id p-<nanoseconds> since it binds no port.
//	              Stop removes a task before cancelling it, so a task still listed when its context ends ended on
//	              its own, and that is when stop_on_exit acts.
//	criu          The binary params.criu names, else criu on PATH; absent, the route answers 501.
//	startSession  params.command under sh -c, so it may use the shell.
//	dump          Dumps params.id, or every running session without one, to <params.images_dir>/<id>, the root
//	              defaulting to ~/.cybershuttle/checkpoints, and answers the ids and directories dumped. CRIU
//	              kills what it dumped, so the session ends and stop_on_exit acts.
//	restore       A session running criu restore on params.images_dir; the tree lives as criu's child.
//	Commands      sessions.select and sessions.stop from sessions.
//	Router        /checkpoint/sessions, and dump and restore beside it.
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
