// Package checkpoint is a sidecar over running shell.exec processes: pause writes one into a checkpoint with CRIU
// under ~/.cybershuttle/checkpoints/<id>, and resume runs the checkpoint as a process under the same id, in this
// job or a later one, via the API or a workflow step. A paused process ends, and the step waiting on it answers
// 202, so a workflow pauses its payload under a signal and goes on with the steps after the pause. CRIU is the
// user's, on PATH, allowed to run unprivileged.
//
//	criu      Resolved once; absent, pause and resume answer 501.
//	criuArgs  A shell job on Linkspan's stdio with its TCP connections; --unprivileged unless root, since the
//	          flag needs CRIU 3.18.
//	record    What a resume needs beside the images: the tree's pid, and the names of the stdout and stderr CRIU
//	          dumped, so it hands the tree this job's in their stead.
//	pause     Dumps the running process id into a .part folder, so a failed dump leaves the earlier checkpoint,
//	          and the process ends with it.
//	resume    Runs criu as the process, its pid the tree's, and like shell.exec answers once it ends: 202 when
//	          paused again.
//	Actions, Router  pause and resume, each a route.
package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/cyber-shuttle/linkspan/internal/install"
	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/servers"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

var criu = install.Which("criu")

var criuArgs = func() []string {
	args := []string{"--shell-job", "--tcp-established"}
	if os.Geteuid() != 0 {
		args = append(args, "--unprivileged")
	}
	return args
}()

type record struct {
	Pid    int    `json:"pid"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

func pause(ctx context.Context, params map[string]any) (int, any, string) {
	id, _ := params["id"].(string)
	if criu == "" {
		return http.StatusNotImplemented, nil, "criu is not on PATH"
	}
	listed := tasks.Select(servers.Process)
	i := slices.IndexFunc(listed, func(t tasks.Task) bool { return t.ID == id && t.State == tasks.StateRunning })
	if i < 0 {
		return http.StatusNotFound, nil, "no running process " + id
	}
	t := listed[i]
	var r record
	for fd, name := range []*string{&r.Stdout, &r.Stderr} {
		target, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", t.Pid, fd+1))
		*name = strings.TrimPrefix(target, "/")
	}
	r.Pid = t.Pid
	images := filepath.Join(install.Dir(), "checkpoints", id)
	part := images + ".part"
	cmd := exec.Command(criu, append([]string{"dump", "-t", strconv.Itoa(t.Pid), "--images-dir", part}, criuArgs...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	log.Printf("checkpoint: pausing %s to %s", id, images)
	servers.Pausing(id, true)
	err := os.MkdirAll(part, 0o700)
	if err == nil {
		err = tasks.Exec(ctx, cmd)
	}
	if err == nil {
		data, _ := json.Marshal(r)
		err = os.WriteFile(filepath.Join(part, "snapshot"), data, 0o600)
	}
	if err == nil {
		_ = os.RemoveAll(images)
		err = os.Rename(part, images)
	}
	if err != nil {
		servers.Pausing(id, false)
		_ = os.RemoveAll(part)
		return http.StatusInternalServerError, nil, err.Error()
	}
	return http.StatusOK, map[string]string{"id": id, "dir": images}, ""
}

func resume(_ context.Context, params map[string]any) (int, any, string) {
	id, _ := params["id"].(string)
	if criu == "" {
		return http.StatusNotImplemented, nil, "criu is not on PATH"
	}
	images := filepath.Join(install.Dir(), "checkpoints", id)
	var r record
	if data, err := os.ReadFile(filepath.Join(images, "snapshot")); err != nil || json.Unmarshal(data, &r) != nil {
		return http.StatusNotFound, nil, "no checkpoint " + id
	}
	argv := append([]string{criu, "restore", "--images-dir", images}, criuArgs...)
	for fd, name := range []string{r.Stdout, r.Stderr} {
		if name != "" {
			argv = append(argv, "--inherit-fd", fmt.Sprintf("fd[%d]:%s", fd+1, name))
		}
	}
	created := servers.Start(tasks.Task{Kind: servers.Process, ID: id, Pid: r.Pid}, argv...)
	ended, paused := servers.Wait(created.ID)
	switch {
	case paused:
		return http.StatusAccepted, ended, ""
	case ended.State != tasks.StateExited:
		return http.StatusInternalServerError, nil, ended.Error
	}
	return http.StatusOK, ended, ""
}

var Actions = map[string]router.Action{
	"pause":  pause,
	"resume": resume,
}

var Router = router.New("/checkpoint", map[string]router.Action{
	"POST /pause":  Actions["pause"],
	"POST /resume": Actions["resume"],
})
