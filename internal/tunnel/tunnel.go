// Package tunnel carries the API off the node in the modes --tunnel-mode lists. Each mode is a package of its own that
// parses its --tunnel-<mode>-args value and reads its credential from the environment; this package only frames them,
// so it knows each mode by name and nothing of its flags. Parse validates everything before main binds anything.
//
//	Mode
//	Modes   Each mode by name; main registers --tunnel-<name>-args with its Usage.
//	redial  Reruns a mode's attempt until cancelled, backing off from 1s to 1m, reset once an attempt lasts a minute,
//	        so one mode failing never ends Linkspan or another mode.
//	Parse   One task per listed mode, kinded by its name; a listed mode without args, or args of an unlisted one, is
//	        refused.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/cyber-shuttle/linkspan/internal/tunnel/devtunnel"
	"github.com/cyber-shuttle/linkspan/internal/tunnel/websocket"
)

type Mode struct {
	Usage string
	start func(args string) (func(context.Context) error, error)
}

var Modes = map[string]Mode{
	"websocket": {websocket.Usage, websocket.New},
	"devtunnel": {devtunnel.Usage, devtunnel.New},
}

func redial(name string, attempt func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		for backoff := time.Second; ctx.Err() == nil; backoff = min(2*backoff, time.Minute) {
			started := time.Now()
			if err := attempt(ctx); err != nil && ctx.Err() == nil {
				log.Printf("%s: %v", name, err)
			}
			if time.Since(started) > time.Minute {
				backoff = time.Second
			}
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
		}
		return nil
	}
}

func Parse(enable bool, list string, args map[string]string) ([]*tasks.Task, error) {
	if enable != (list != "") {
		return nil, errors.New("--tunnel-mode is required with --tunnel-enable and refused without it")
	}
	listed := map[string]bool{}
	for name := range strings.SplitSeq(list, ",") {
		if _, ok := Modes[name]; list != "" && (!ok || listed[name]) {
			return nil, fmt.Errorf("--tunnel-mode: %q is not websocket or devtunnel, or is repeated", name)
		}
		listed[name] = true
	}
	var all []*tasks.Task
	for _, name := range slices.Sorted(maps.Keys(Modes)) {
		if listed[name] != (args[name] != "") {
			return nil, fmt.Errorf("--tunnel-%s-args is required with --tunnel-mode=%s and refused without it", name, name)
		}
		if !listed[name] {
			continue
		}
		run, err := Modes[name].start(args[name])
		if err != nil {
			return nil, err
		}
		all = append(all, &tasks.Task{Kind: tasks.Kind(name), Run: redial(name, run)})
	}
	return all, nil
}
