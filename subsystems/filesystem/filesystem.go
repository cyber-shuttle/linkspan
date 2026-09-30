// Package filesystem is reserved for mounting datasets and moving files between the job and elsewhere. Each
// operation is a route and a workflow action alike, declared with the params it will take and answering 501
// until it does something, so a document naming one is refused for a missing param today as it will be then.
//
//	notImplemented  The placeholder every operation is: 400 without each of its params, else 501 naming itself.
//	Actions         mount, copy and sync take source and target; unmount takes target.
//	Router          POST /filesystem/mount, /unmount, /copy and /sync, each an Actions entry.
package filesystem

import (
	"context"
	"net/http"

	"github.com/cyber-shuttle/linkspan/internal/router"
)

func notImplemented(name string, params ...string) router.Action {
	return func(_ context.Context, given map[string]any) (int, any, string) {
		for _, p := range params {
			if router.Str(given, p) == "" {
				return http.StatusBadRequest, nil, p + " is required"
			}
		}
		return http.StatusNotImplemented, nil, name + " is not implemented"
	}
}

var Actions = map[string]router.Action{
	"mount":   notImplemented("mount", "source", "target"),
	"unmount": notImplemented("unmount", "target"),
	"copy":    notImplemented("copy", "source", "target"),
	"sync":    notImplemented("sync", "source", "target"),
}

var Router = router.New("/filesystem", map[string]router.Action{
	"POST /mount":   Actions["mount"],
	"POST /unmount": Actions["unmount"],
	"POST /copy":    Actions["copy"],
	"POST /sync":    Actions["sync"],
})
