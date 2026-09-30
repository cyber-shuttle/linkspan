// Package tunnel carries the API off the node over the transports --tunnel-mode lists. Each transport is a package of
// its own that parses its --tunnel-<transport>-args value; this package frames them and reads each credential from
// the environment, knowing nothing of their flags. Parse validates everything before main binds anything.
//
//	Transport
//	Transports  Each transport by name; main registers --tunnel-<name>-args with its Usage.
//	redial      Reruns a transport's attempt until cancelled, backing off from 1s to 1m, reset once an attempt lasts a
//	            minute, so one transport failing never ends Linkspan or another transport.
//	Parse       One task per listed transport, kinded by its name; a listed transport without args or its credential,
//	            or args of an unlisted one, is refused.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/cyber-shuttle/linkspan/internal/tunnel/devtunnel"
	"github.com/cyber-shuttle/linkspan/internal/tunnel/link"
)

type Transport struct {
	Usage string
	env   string
	start func(args, token string) (func(context.Context) error, error)
}

var Transports = map[string]Transport{
	"link":      {link.Usage, link.Env, link.New},
	"devtunnel": {devtunnel.Usage, devtunnel.Env, devtunnel.New},
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
		if _, ok := Transports[name]; list != "" && (!ok || listed[name]) {
			return nil, fmt.Errorf("--tunnel-mode: %q is not link or devtunnel, or is repeated", name)
		}
		listed[name] = true
	}
	var all []*tasks.Task
	for _, name := range slices.Sorted(maps.Keys(Transports)) {
		if listed[name] != (args[name] != "") {
			return nil, fmt.Errorf("--tunnel-%s-args is required with --tunnel-mode=%s and refused without it", name, name)
		}
		if !listed[name] {
			continue
		}
		t := Transports[name]
		token := os.Getenv(t.env)
		if token == "" {
			return nil, fmt.Errorf("%s is required with the %s transport", t.env, name)
		}
		run, err := t.start(args[name], token)
		if err != nil {
			return nil, err
		}
		all = append(all, &tasks.Task{Kind: tasks.Kind(name), Run: redial(name, run)})
	}
	return all, nil
}
