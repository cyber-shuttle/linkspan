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
//	Router           POST /workflow/shell/exec; main mounts no workflow router, a workflow being driven by its
//	                 document.
package workflow

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/cyber-shuttle/linkspan/internal/tunnel"
	"gopkg.in/yaml.v3"
)

var signals = map[string]os.Signal{"SIGUSR1": syscall.SIGUSR1, "SIGUSR2": syscall.SIGUSR2, "SIGHUP": syscall.SIGHUP}

type Step struct {
	Name    string         `yaml:"name"`
	On      string         `yaml:"on"`
	Action  string         `yaml:"action"`
	Params  map[string]any `yaml:"params"`
	Tasks   []Step         `yaml:"tasks"`
	command router.Command
}

var steps []Step

func execute(ctx context.Context, params map[string]any) (int, any, string) {
	command, _ := params["command"].(string)
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return http.StatusBadRequest, nil, "command is required"
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := tasks.Exec(ctx, cmd); err != nil {
		return http.StatusInternalServerError, nil, err.Error()
	}
	return http.StatusOK, nil, ""
}

func Run(ctx context.Context, trigger string) error {
	for i, step := range steps {
		if step.On != trigger {
			continue
		}
		log.Printf("workflow: [%d/%d] %q on %s", i+1, len(steps), step.Name, trigger)
		status, out, msg := step.command(ctx, step.Params)
		if status < 200 || status >= 300 {
			return fmt.Errorf("step %d (%s): %s: %d %s", i+1, step.Name, step.Action, status, msg)
		}
		if out != nil {
			encoded, _ := json.Marshal(out)
			log.Printf("workflow: %s: %s", step.Action, encoded)
		}
	}
	return nil
}

func Start(ctx context.Context) error {
	if err := Run(ctx, "start"); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case <-tunnel.Ready():
	}
	return Run(ctx, "ready")
}

func WatchSignal(name string) func(context.Context) error {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, signals[name])
	return func(ctx context.Context) error {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ch:
				if err := Run(ctx, name); err != nil {
					return err
				}
			}
		}
	}
}

func Signals() []string {
	var out []string
	for _, step := range steps {
		if _, ok := signals[step.On]; ok && !slices.Contains(out, step.On) {
			out = append(out, step.On)
		}
	}
	return out
}

func Load(path string, commands map[string]router.Command) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("workflow: read: %w", err)
	}
	var doc struct {
		Steps []Step `yaml:"steps"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("workflow: parse: %w", err)
	}
	var flat []Step
	for i, step := range doc.Steps {
		on := cmp.Or(step.On, "start")
		if _, ok := signals[on]; !ok && on != "start" && on != "ready" && on != "stop" {
			return fmt.Errorf("workflow: step %d (%s): unknown trigger %q", i+1, step.Name, on)
		}
		list := step.Tasks
		if list == nil {
			list = []Step{step}
		}
		for _, task := range list {
			task.On, task.command = on, commands[task.Action]
			if task.command == nil {
				return fmt.Errorf("workflow: step %d (%s): unknown action %q", i+1, task.Name, task.Action)
			}
			flat = append(flat, task)
		}
	}
	steps = flat
	return nil
}

var Commands = map[string]router.Command{
	"shell.exec": execute,
}

var Router = router.New("/workflow", map[string]router.Command{
	"POST /shell/exec": Commands["shell.exec"],
})
