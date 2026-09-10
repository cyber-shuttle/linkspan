// Package sessions is what the session subsystems share: the commands that list a kind's sessions and stop one,
// over the task registry, with the wire shapes docs/COMPATIBILITY.md freezes.
//
//	Select  The list command of one kind, ordered by id, [] when none.
//	Stop    Answers 404 for an id the registry does not hold, else the id with state stopped.
package sessions

import (
	"context"
	"net/http"

	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

func Select(kind tasks.Kind) router.Command {
	return func(context.Context, map[string]any) (int, any, string) {
		return http.StatusOK, tasks.Select(kind), ""
	}
}

func Stop(_ context.Context, params map[string]any) (int, any, string) {
	id, _ := params["id"].(string)
	if !tasks.Stop(id) {
		return http.StatusNotFound, nil, "unknown id " + id
	}
	return http.StatusOK, map[string]string{"id": id, "state": "stopped"}, ""
}
