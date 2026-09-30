// Package servers is what the subsystems behind /sessions routes share: the actions that list a kind's servers and
// stop one, over the task registry, with the wire shapes docs/COMPATIBILITY.md freezes; and, for a plain process,
// starting it and waiting for its end. ref names what a request creates, else Linkspan assigns the id.
//
//	Process   The kind of a plain process: a shell.exec command, or a resumed one.
//	pausing   The processes a pause is ending, so the end is not a failure to whoever waits.
//	Select    The list action of one kind, ordered by id, [] when none.
//	Start     Runs argv as the given task, its id defaulting to the kind's initial and the time; a repeated id
//	          replaces the earlier process. It cannot fail, since a process binds no address.
//	Stop      Answers 404 for an id the registry does not hold, else the id with state stopped.
//	Pausing   Whether a pause is about to end the process.
//	Wait      Blocks until the process ends and is unlisted, and says whether a pause ended it.
//	Run       Starts argv and answers once it ends: 202 when a pause ended it, 500 when it failed, else 200.
//	Serve     Starts a server task and answers 201 with it, or 500.
package servers

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

const Process tasks.Kind = "process"

var pausing sync.Map

func Select(kind tasks.Kind) router.Action {
	return func(context.Context, map[string]any) (int, any, string) {
		return http.StatusOK, tasks.Select(kind), ""
	}
}

func Start(t tasks.Task, argv ...string) tasks.Task {
	t.ID = cmp.Or(t.ID, fmt.Sprintf("%c-%d", t.Kind[0], time.Now().UnixNano()))
	t.Attrs = func(tasks.Task) map[string]string { return map[string]string{"command": strings.Join(argv, " ")} }
	t.Child = func(context.Context) (*exec.Cmd, error) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd, nil
	}
	created, _ := t.Start()
	return created
}

func Stop(_ context.Context, params map[string]any) (int, any, string) {
	id := router.Str(params, "id")
	if !tasks.Stop(id) {
		return http.StatusNotFound, nil, "unknown id " + id
	}
	return http.StatusOK, map[string]string{"id": id, "state": "stopped"}, ""
}

func Pausing(id string, is bool) {
	if is {
		pausing.Store(id, true)
	} else {
		pausing.Delete(id)
	}
}

func Wait(id string) (tasks.Task, bool) {
	ended := tasks.Wait(id)
	_, paused := pausing.LoadAndDelete(id)
	return ended, paused
}

func Run(t tasks.Task, argv ...string) (int, any, string) {
	ended, paused := Wait(Start(t, argv...).ID)
	switch {
	case paused:
		return http.StatusAccepted, ended, ""
	case ended.State != tasks.StateExited:
		return http.StatusInternalServerError, nil, ended.Error
	}
	return http.StatusOK, ended, ""
}

func Serve(t *tasks.Task) (int, any, string) {
	created, err := t.Start()
	if err != nil {
		return http.StatusInternalServerError, nil, err.Error()
	}
	return http.StatusCreated, created, ""
}
