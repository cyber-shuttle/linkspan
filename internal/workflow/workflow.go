// Package workflow runs the setup steps cs-control ships with a job: YAML
// shell.exec commands, in order, stopping at the first failure. Commands are
// split on whitespace and run without a shell. Steps inherit Linkspan's stdout
// and stderr, so a forking step cannot hold the workflow open.
//
//	Workflow  Its argv is filled at load, so run cannot fail before its first
//	          step. Each step goes through procmgr.Exec, and Start registers run
//	          under procmgr as "workflow".
//	New       It reads and validates the document, so an invalid one is refused
//	          at startup.
package workflow

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/cyber-shuttle/linkspan/internal/procmgr"
	"gopkg.in/yaml.v3"
)

type Workflow struct {
	Name  string `yaml:"name"`
	Steps []struct {
		Action string `yaml:"action"`
		Name   string `yaml:"name"`
		Params struct {
			Command string `yaml:"command"`
		} `yaml:"params"`
		argv []string
	} `yaml:"steps"`
}

func (w *Workflow) run(ctx context.Context) error {
	log.Printf("workflow: starting %q (%d steps)", w.Name, len(w.Steps))
	for i, step := range w.Steps {
		log.Printf("workflow: [%d/%d] %q: %q", i+1, len(w.Steps), step.Name, step.Params.Command)
		cmd := exec.Command(step.argv[0], step.argv[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := procmgr.Exec(ctx, cmd); err != nil {
			return fmt.Errorf("step %d (%s): %w", i+1, step.Name, err)
		}
	}
	log.Printf("workflow: %q finished", w.Name)
	return nil
}

func New(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("workflow: read: %w", err)
	}
	var w Workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("workflow: parse: %w", err)
	}
	for i := range w.Steps {
		step := &w.Steps[i]
		if step.Action != "shell.exec" {
			return nil, fmt.Errorf("workflow: step %d: unknown action %q", i+1, step.Action)
		}
		if step.argv = strings.Fields(step.Params.Command); len(step.argv) == 0 {
			return nil, fmt.Errorf("workflow: step %d (%s): command is required", i+1, step.Name)
		}
	}
	return &w, nil
}

func (w *Workflow) Start() {
	procmgr.Start(procmgr.KindWorkflow, "workflow", "", w.run)
}
