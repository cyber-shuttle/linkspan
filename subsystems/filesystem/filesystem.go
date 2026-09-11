// Package filesystem is reserved for mounting datasets and moving files between the job and elsewhere. Each
// operation is a route and a workflow command alike, declared and answering 501 until it does something.
//
//	notImplemented  The placeholder every operation is, naming itself.
//	Commands        mount, unmount, copy and sync.
//	Router          POST /filesystem/mount, /unmount, /copy and /sync, each a Commands entry.
package filesystem

import (
	"context"
	"net/http"

	"github.com/cyber-shuttle/linkspan/internal/router"
)

func notImplemented(name string) router.Command {
	return func(context.Context, map[string]any) (int, any, string) {
		return http.StatusNotImplemented, nil, name + " is not implemented"
	}
}

var Commands = map[string]router.Command{
	"mount":   notImplemented("mount"),
	"unmount": notImplemented("unmount"),
	"copy":    notImplemented("copy"),
	"sync":    notImplemented("sync"),
}

var Router = router.New("/filesystem", map[string]router.Command{
	"POST /mount":   Commands["mount"],
	"POST /unmount": Commands["unmount"],
	"POST /copy":    Commands["copy"],
	"POST /sync":    Commands["sync"],
})
