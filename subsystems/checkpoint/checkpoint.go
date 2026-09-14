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
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/install"
	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/sessions"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

const kind tasks.Kind = "process"

var criuArgs = []string{"--shell-job", "--tcp-established", "--unprivileged"}

var exit = func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) }

func start(command string, stopOnExit bool, argv ...string) (int, any, string) {
	id := fmt.Sprintf("p-%d", time.Now().UnixNano())
	created, err := (&tasks.Task{ID: id, Kind: kind, Attrs: func(tasks.Task) map[string]string {
		return map[string]string{"command": command, "stop_on_exit": strconv.FormatBool(stopOnExit)}
	}, Child: func(ctx context.Context) (*exec.Cmd, error) {
		context.AfterFunc(ctx, func() {
			if stopOnExit && slices.ContainsFunc(tasks.Select(kind), func(t tasks.Task) bool { return t.ID == id }) {
				log.Printf("checkpoint: %s ended, stopping", id)
				exit()
			}
		})
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd, nil
	}}).Start()
	if err != nil {
		return http.StatusInternalServerError, nil, err.Error()
	}
	return http.StatusCreated, created, ""
}

func criu(params map[string]any) (string, error) {
	name, _ := params["criu"].(string)
	return exec.LookPath(cmp.Or(name, "criu"))
}

func startSession(_ context.Context, params map[string]any) (int, any, string) {
	command, _ := params["command"].(string)
	if command == "" {
		return http.StatusBadRequest, nil, "command is required"
	}
	stopOnExit, _ := params["stop_on_exit"].(bool)
	return start(command, stopOnExit, "sh", "-c", command)
}

func dump(ctx context.Context, params map[string]any) (int, any, string) {
	bin, err := criu(params)
	if err != nil {
		return http.StatusNotImplemented, nil, err.Error()
	}
	id, _ := params["id"].(string)
	root, _ := params["images_dir"].(string)
	dumped := []map[string]string{}
	for _, t := range tasks.Select(kind) {
		if t.State != tasks.StateRunning || (id != "" && t.ID != id) {
			continue
		}
		images := filepath.Join(cmp.Or(root, filepath.Join(install.Dir(), "checkpoints")), t.ID)
		if err := os.MkdirAll(images, 0o700); err != nil {
			return http.StatusInternalServerError, nil, err.Error()
		}
		cmd := exec.Command(bin, append([]string{"dump", "-t", strconv.Itoa(t.Pid), "--images-dir", images}, criuArgs...)...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		log.Printf("checkpoint: dumping %s to %s", t.ID, images)
		if err := tasks.Exec(ctx, cmd); err != nil {
			return http.StatusInternalServerError, nil, fmt.Sprintf("%s: %v", t.ID, err)
		}
		dumped = append(dumped, map[string]string{"id": t.ID, "images_dir": images})
	}
	if id != "" && len(dumped) == 0 {
		return http.StatusNotFound, nil, "no running session " + id
	}
	return http.StatusOK, dumped, ""
}

func restore(_ context.Context, params map[string]any) (int, any, string) {
	bin, err := criu(params)
	if err != nil {
		return http.StatusNotImplemented, nil, err.Error()
	}
	images, _ := params["images_dir"].(string)
	if images == "" {
		return http.StatusBadRequest, nil, "images_dir is required"
	}
	stopOnExit, _ := params["stop_on_exit"].(bool)
	argv := append([]string{bin, "restore", "--images-dir", images}, criuArgs...)
	return start(strings.Join(argv, " "), stopOnExit, argv...)
}

var Commands = map[string]router.Command{
	"sessions.select": sessions.Select(kind),
	"sessions.start":  startSession,
	"sessions.stop":   sessions.Stop,
	"dump":            dump,
	"restore":         restore,
}

var Router = router.New("/checkpoint", map[string]router.Command{
	"GET /sessions":         Commands["sessions.select"],
	"POST /sessions":        Commands["sessions.start"],
	"DELETE /sessions/{id}": Commands["sessions.stop"],
	"POST /dump":            Commands["dump"],
	"POST /restore":         Commands["restore"],
})
