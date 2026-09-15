// Package sessions is what the session subsystems share: the commands that list a kind's sessions and stop one,
// over the task registry, with the wire shapes docs/COMPATIBILITY.md freezes.
//
//	Select  The list command of one kind, ordered by id, [] when none.
//	Stop    Answers 404 for an id the registry does not hold, else the id with state stopped.
package sessions

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
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

func Selected(params map[string]any) []string {
	ids, _ := params["ids"].([]any)
	out := []string{}
	for _, v := range ids {
		if id, ok := v.(string); ok {
			out = append(out, id)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(out)))
}

func Select(kind tasks.Kind) router.Command {
	return func(context.Context, map[string]any) (int, any, string) {
		return http.StatusOK, tasks.Select(kind), ""
	}
}

func Start(t tasks.Task, argv ...string) (tasks.Task, error) {
	t.ID = cmp.Or(t.ID, fmt.Sprintf("%c-%d", t.Kind[0], time.Now().UnixNano()))
	t.Attrs = func(tasks.Task) map[string]string { return map[string]string{"command": strings.Join(argv, " ")} }
	t.Child = func(context.Context) (*exec.Cmd, error) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd, nil
	}
	return t.Start()
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
