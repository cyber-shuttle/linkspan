package workflow

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"
)

// shell.exec is the only action. Its command is split on whitespace and run
// without a shell, so nothing is expanded.
type Config struct {
	Name  string `yaml:"name"`
	Steps []struct {
		Action string `yaml:"action"`
		Name   string `yaml:"name"`
		Params struct {
			Command string `yaml:"command"`
		} `yaml:"params"`
	} `yaml:"steps"`
}

func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading workflow file: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing workflow YAML: %w", err)
	}
	return &cfg, nil
}

// ctx is checked between steps; the running step is not preempted.
func Run(ctx context.Context, wf *Config) error {
	log.Printf("workflow: starting %q (%d steps)", wf.Name, len(wf.Steps))
	for i, step := range wf.Steps {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("workflow cancelled at step %d (%s): %w", i+1, step.Name, err)
		}
		if step.Action != "shell.exec" {
			return fmt.Errorf("workflow step %d: unknown action %q", i+1, step.Action)
		}
		log.Printf("workflow: [%d/%d] %s", i+1, len(wf.Steps), step.Name)
		if err := shellExec(ctx, step.Params.Command); err != nil {
			return fmt.Errorf("workflow step %d (%s): %w", i+1, step.Name, err)
		}
	}
	log.Printf("workflow: %q finished successfully", wf.Name)
	return nil
}

func shellExec(ctx context.Context, command string) error {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return fmt.Errorf("shell.exec: command is required")
	}
	log.Printf("shell.exec: %s", command)

	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	// Straight to our own stdout and stderr, which are *os.File, so exec hands
	// the descriptors to the child rather than copying through a pipe. A step
	// that daemonises -- cs-control's `setsid --fork python -m jupyter_server`
	// does -- keeps those descriptors, and Wait blocks on a pipe until every
	// holder closes it, which for a server it never does. It also means the
	// step's output is logged as it happens instead of after it ends, so a step
	// killed by a walltime or OOM still leaves its output behind.
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("shell.exec %q: %w", command, err)
	}
	return nil
}
