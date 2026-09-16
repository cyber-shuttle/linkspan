// Package sessions is what the session subsystems share: the commands that list a kind's sessions and stop one,
// over the task registry, with the wire shapes docs/COMPATIBILITY.md freezes; and, for a session that is a plain
// process, starting it and waiting for its end. ref names what a request creates, else Linkspan assigns the id.
//
//	Process   The kind of a plain-process session: a shell.exec command, or a resumed one.
//	pausing   The sessions a pause is ending, so the end is not a failure to whoever waits.
//	Ref       The id a request names for what it creates.
//	Select    The list command of one kind, ordered by id, [] when none.
//	Start     Runs argv as the given task, its id defaulting to the kind's initial and the time; a repeated id
//	          replaces the earlier session. It cannot fail, since a process binds no address.
//	Stop      Answers 404 for an id the registry does not hold, else the id with state stopped.
//	Pausing   Whether a pause is about to end the session.
//	Wait      Blocks until the session ends and is unlisted, and says whether a pause ended it.
package sessions

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

func Ref(params map[string]any) string {
	ref, _ := params["ref"].(string)
	return ref
}

func Select(kind tasks.Kind) router.Command {
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
	id, _ := params["id"].(string)
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
