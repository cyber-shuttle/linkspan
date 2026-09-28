// Package workflow runs the steps a YAML document names at the moments of the job's life it chooses: start, once
// the API is up; ready, once start's run is complete; stop, when Linkspan is told to exit; or a signal such as
// SIGUSR1, which Slurm sends ahead of the walltime. Each entry of the document's tasks key is a trigger, one moment
// and its steps, each an action with its params; they run in order, and the first failure stops them. shell.exec
// runs a command under sh as a process named by the step's ref, which checkpoint.pause can end; a paused step ends
// its trigger's run, so the rest waits for a resume. A run then waits on the servers and processes its steps
// started, and the job ends once start and ready are complete, so a Slurm job ends with its payload and an
// interactive one with its servers. Any other action is one a subsystem offers, such as vscode.sessions.start, with
// the params it takes, so the job's servers are set up from the file. One document is loaded per job.
//
//	signals          The moments beyond start, ready and stop, by name.
//	Step             One action with its params; Ref names what the action creates.
//	Trigger          On defaults to start; Steps are the steps under it, one at least.
//	loaded           The document's triggers, each with its steps' actions bound at load.
//	exec             The shell.exec action.
//	Run              The steps of each trigger at that moment, in order, each failing on a status outside 2xx and
//	                 ending the run on 202, then a wait on every server or process a step answered with. Nothing when
//	                 no document is loaded.
//	Start            The task main starts, built after Load: the start steps, then the ready steps, then SIGTERM to
//	                 Linkspan, the job being done; beside them each signal's steps as it arrives, so one reaches a
//	                 running step, and the job's end waits for a signal's steps.
//	Load             Reads and validates the document against the actions main offers, so an invalid one is
//	                 refused at startup, as is one without tasks or with a field it does not know; a step's ref
//	                 goes to its action as the ref param.
//	Actions          shell.exec, the package's own action, which main adds unprefixed.
//	Router           POST /workflow/shell/exec, the same action as a route.
package workflow

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/servers"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

var signals = map[string]os.Signal{"SIGUSR1": syscall.SIGUSR1, "SIGUSR2": syscall.SIGUSR2, "SIGHUP": syscall.SIGHUP}

type Step struct {
	Name   string         `yaml:"name"`
	Ref    string         `yaml:"ref"`
	Action string         `yaml:"action"`
	Params map[string]any `yaml:"params"`
	run    router.Action
}

type Trigger struct {
	On    string `yaml:"on"`
	Steps []Step `yaml:"steps"`
}

var loaded []Trigger

func exec(_ context.Context, params map[string]any) (int, any, string) {
	command, _ := params["command"].(string)
	if strings.TrimSpace(command) == "" {
		return http.StatusBadRequest, nil, "command is required"
	}
	created := servers.Start(tasks.Task{Kind: servers.Process, ID: servers.Ref(params)}, "sh", "-c", command)
	ended, paused := servers.Wait(created.ID)
	switch {
	case paused:
		return http.StatusAccepted, ended, ""
	case ended.State != tasks.StateExited:
		return http.StatusInternalServerError, nil, ended.Error
	}
	return http.StatusOK, ended, ""
}

func Run(ctx context.Context, moment string) error {
	var started []string
run:
	for _, t := range loaded {
		if t.On != moment {
			continue
		}
		for i, step := range t.Steps {
			log.Printf("workflow: [%d/%d] %q on %s", i+1, len(t.Steps), step.Name, moment)
			status, out, msg := step.run(ctx, step.Params)
			if status < 200 || status >= 300 {
				return fmt.Errorf("step %d (%s): %s: %d %s", i+1, step.Name, step.Action, status, msg)
			}
			if out != nil {
				encoded, _ := json.Marshal(out)
				log.Printf("workflow: %s: %s", step.Action, encoded)
				var created struct{ ID string }
				if json.Unmarshal(encoded, &created) == nil && created.ID != "" {
					started = append(started, created.ID)
				}
			}
			if status == http.StatusAccepted {
				log.Printf("workflow: %q paused; no more %s steps", step.Name, moment)
				break run
			}
		}
	}
	for _, id := range started {
		tasks.Wait(id)
	}
	return nil
}

func Start() func(context.Context) error {
	ch := make(chan os.Signal, 1)
	for _, t := range loaded {
		if sig, ok := signals[t.On]; ok {
			signal.Notify(ch, sig)
		}
	}
	return func(ctx context.Context) error {
		defer signal.Stop(ch)
		setup := make(chan error, 1)
		go func() {
			err := Run(ctx, "start")
			if err == nil {
				err = Run(ctx, "ready")
			}
			setup <- err
		}()
		for {
			select {
			case <-ctx.Done():
				return nil
			case err := <-setup:
				if err != nil {
					return err
				}
				log.Print("workflow: start and ready done, ending the job")
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
				setup = nil
			case sig := <-ch:
				if err := Run(ctx, unix.SignalName(sig.(syscall.Signal))); err != nil {
					return err
				}
			}
		}
	}
}

func Load(path string, actions map[string]router.Action) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("workflow: read: %w", err)
	}
	var doc struct {
		Name  string    `yaml:"name"`
		Tasks []Trigger `yaml:"tasks"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("workflow: parse: %w", err)
	}
	if len(doc.Tasks) == 0 {
		return errors.New("workflow: no tasks")
	}
	var all []Trigger
	for i, t := range doc.Tasks {
		t.On = cmp.Or(t.On, "start")
		if _, ok := signals[t.On]; !ok && t.On != "start" && t.On != "ready" && t.On != "stop" {
			return fmt.Errorf("workflow: trigger %d: unknown %q", i+1, t.On)
		}
		if len(t.Steps) == 0 {
			return fmt.Errorf("workflow: trigger %d: no steps", i+1)
		}
		for j := range t.Steps {
			step := &t.Steps[j]
			if step.run = actions[step.Action]; step.run == nil {
				return fmt.Errorf("workflow: trigger %d (%s): unknown action %q", i+1, step.Name, step.Action)
			}
			if step.Ref != "" {
				params := map[string]any{}
				maps.Copy(params, step.Params)
				params["ref"], step.Params = step.Ref, params
			}
		}
		all = append(all, t)
	}
	loaded = all
	return nil
}

var Actions = map[string]router.Action{
	"shell.exec": exec,
}

var Router = router.New("/workflow", map[string]router.Action{
	"POST /shell/exec": Actions["shell.exec"],
})
