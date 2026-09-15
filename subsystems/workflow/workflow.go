// Package workflow runs the steps a YAML document names at the moments of the job's life it chooses: start, once
// the API is up; ready, once the tunnel is hosting, or at once without one; stop, when Linkspan is told to exit;
// or a signal such as SIGUSR1, which Slurm sends ahead of the time limit. A step is one action with its params
// or a list of tasks under one trigger; they run in order, and the first failure stops them. shell.exec runs a
// command without a shell, so nothing expands and a forking command cannot hold the workflow open. Any other
// action is one a subsystem offers, such as vscode.sessions.start, with the params it takes, so a workspace is
// set up from the file. One document is loaded per job.
//
//	signals          The triggers beyond start, ready and stop, by name.
//	Step             On defaults to start; Tasks, when given, are the actions under it, else Action and Params are
//	                 the one.
//	steps            The loaded document, one entry per task, its trigger and its command bound at load.
//	execute          The shell.exec command.
//	Run              The tasks of one trigger in order, each failing on a status outside 2xx; nothing when no
//	                 document is loaded.
//	Start            The task main starts: the start tasks, then the ready tasks once tunnel.Ready is closed.
//	WatchSignal      The task of one signal: its tasks on each arrival.
//	Signals          The signal triggers the document names, each once, so main watches each.
//	Load             Reads and validates the document against the commands main enables, so an invalid one is
//	                 refused at startup.
//	Commands         shell.exec, the package's own action, which main adds unprefixed.
//	Router           POST /workflow/shell/exec, the same command as a route.
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
	"github.com/cyber-shuttle/linkspan/internal/sessions"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/cyber-shuttle/linkspan/internal/tunnel"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

var signals = map[string]os.Signal{"SIGUSR1": syscall.SIGUSR1, "SIGUSR2": syscall.SIGUSR2, "SIGHUP": syscall.SIGHUP}

type Step struct {
	Name    string         `yaml:"name"`
	Ref     string         `yaml:"ref"`
	Action  string         `yaml:"action"`
	Params  map[string]any `yaml:"params"`
	command router.Command
}

type Task struct {
	On    string `yaml:"on"`
	Steps []Step `yaml:"steps"`
}

var loaded []Task

func exec(_ context.Context, params map[string]any) (int, any, string) {
	command, _ := params["command"].(string)
	if strings.TrimSpace(command) == "" {
		return http.StatusBadRequest, nil, "command is required"
	}
	created, err := sessions.Start(tasks.Task{Kind: sessions.Process, ID: sessions.Ref(params)}, "sh", "-c", command)
	if err != nil {
		return http.StatusInternalServerError, nil, err.Error()
	}
	ended, paused := sessions.Wait(created.ID)
	switch {
	case paused:
		return http.StatusAccepted, ended, ""
	case ended.State != tasks.StateExited:
		return http.StatusInternalServerError, nil, ended.Error
	}
	return http.StatusOK, ended, ""
}

func Run(ctx context.Context, trigger string) error {
	var started []string
run:
	for _, task := range loaded {
		if task.On != trigger {
			continue
		}
		for i, step := range task.Steps {
			log.Printf("workflow: [%d/%d] %q on %s", i+1, len(task.Steps), step.Name, trigger)
			status, out, msg := step.command(ctx, step.Params)
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
				log.Printf("workflow: %q paused; no more %s steps", step.Name, trigger)
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
	for _, task := range loaded {
		if sig, ok := signals[task.On]; ok {
			signal.Notify(ch, sig)
		}
	}
	return func(ctx context.Context) error {
		defer signal.Stop(ch)
		setup := make(chan error, 1)
		go func() {
			if err := Run(ctx, "start"); err != nil {
				setup <- err
				return
			}
			select {
			case <-ctx.Done():
			case <-tunnel.Ready():
				setup <- Run(ctx, "ready")
			}
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

func Load(path string, commands map[string]router.Command) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("workflow: read: %w", err)
	}
	var doc struct {
		Name  string `yaml:"name"`
		Tasks []Task `yaml:"tasks"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("workflow: parse: %w", err)
	}
	if len(doc.Tasks) == 0 {
		return errors.New("workflow: no tasks")
	}
	var all []Task
	for i, task := range doc.Tasks {
		task.On = cmp.Or(task.On, "start")
		if _, ok := signals[task.On]; !ok && task.On != "start" && task.On != "ready" && task.On != "stop" {
			return fmt.Errorf("workflow: task %d: unknown trigger %q", i+1, task.On)
		}
		if len(task.Steps) == 0 {
			return fmt.Errorf("workflow: task %d: no steps", i+1)
		}
		for j := range task.Steps {
			step := &task.Steps[j]
			if step.command = commands[step.Action]; step.command == nil {
				return fmt.Errorf("workflow: task %d (%s): unknown action %q", i+1, step.Name, step.Action)
			}
			if step.Ref != "" {
				params := map[string]any{}
				maps.Copy(params, step.Params)
				params["ref"], step.Params = step.Ref, params
			}
		}
		all = append(all, task)
	}
	loaded = all
	return nil
}

var Commands = map[string]router.Command{
	"shell.exec": exec,
}

var Router = router.New("/workflow", map[string]router.Command{
	"POST /shell/exec": Commands["shell.exec"],
})
