package workflow

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"text/template"

	"gopkg.in/yaml.v3"
)

// WorkflowConfig is the top-level YAML structure.
type WorkflowConfig struct {
	Name  string `yaml:"name"`
	Steps []Step `yaml:"steps"`
}

// Step describes a single workflow action.
type Step struct {
	Action  string            `yaml:"action"`
	Name    string            `yaml:"name"`
	Params  map[string]any    `yaml:"params"`
	Outputs map[string]string `yaml:"outputs"`
}

// ActionResult holds key-value outputs produced by an action.
type ActionResult map[string]any

// ActionFunc is the signature every registered action must satisfy.
type ActionFunc func(params map[string]any) (*ActionResult, error)

// LoadFile reads and parses a workflow YAML file.
func LoadFile(path string) (*WorkflowConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading workflow file: %w", err)
	}
	return parse(data)
}

// LoadReader reads and parses a workflow from an io.Reader (e.g. os.Stdin).
func LoadReader(r io.Reader) (*WorkflowConfig, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading workflow from reader: %w", err)
	}
	return parse(data)
}

func parse(data []byte) (*WorkflowConfig, error) {
	var cfg WorkflowConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing workflow YAML: %w", err)
	}
	return &cfg, nil
}

// Engine executes a workflow using its registry and collected variables.
type Engine struct {
	Registry *Registry
	Vars     map[string]any
}

// NewEngine creates an Engine with the given registry and initial variables.
func NewEngine(reg *Registry, vars map[string]any) *Engine {
	if vars == nil {
		vars = make(map[string]any)
	}
	return &Engine{Registry: reg, Vars: vars}
}

// Run executes every step in the workflow sequentially.
// On step failure it logs the error and returns; the caller (main.go) keeps the
// HTTP server running.
func (e *Engine) Run(wf *WorkflowConfig) error {
	log.Printf("workflow: starting %q (%d steps)", wf.Name, len(wf.Steps))

	for i, step := range wf.Steps {
		log.Printf("workflow: [%d/%d] %s (action=%s)", i+1, len(wf.Steps), step.Name, step.Action)

		fn := e.Registry.Get(step.Action)
		if fn == nil {
			return fmt.Errorf("workflow step %d: unknown action %q", i+1, step.Action)
		}

		// Resolve {{.var}} templates in string params.
		resolved, err := e.resolveParams(step.Params)
		if err != nil {
			return fmt.Errorf("workflow step %d (%s): resolving params: %w", i+1, step.Name, err)
		}

		result, err := fn(resolved)
		if err != nil {
			return fmt.Errorf("workflow step %d (%s): %w", i+1, step.Name, err)
		}

		// Map action outputs to workflow variables.
		if result != nil && step.Outputs != nil {
			for field, varName := range step.Outputs {
				val, ok := (*result)[field]
				if !ok {
					log.Printf("workflow: warning: step %d output field %q not found in result", i+1, field)
					continue
				}
				e.Vars[varName] = val
				log.Printf("workflow: captured %s = %v", varName, val)
			}
		}

		log.Printf("workflow: [%d/%d] %s completed", i+1, len(wf.Steps), step.Name)
	}

	log.Printf("workflow: %q finished successfully", wf.Name)
	return nil
}

// resolveParams applies Go text/template substitution on every string value
// (including strings nested inside slices) using the current engine variables.
func (e *Engine) resolveParams(params map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(params))
	for k, v := range params {
		resolved, err := e.resolveValue(v)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}

func (e *Engine) resolveValue(v any) (any, error) {
	switch val := v.(type) {
	case string:
		return e.resolveString(val)
	case []any:
		resolved := make([]any, len(val))
		for i, elem := range val {
			r, err := e.resolveValue(elem)
			if err != nil {
				return nil, err
			}
			resolved[i] = r
		}
		return resolved, nil
	case map[string]any:
		return e.resolveParams(val)
	default:
		return v, nil
	}
}

func (e *Engine) resolveString(s string) (string, error) {
	tmpl, err := template.New("").Parse(s)
	if err != nil {
		return "", fmt.Errorf("template parse: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, e.Vars); err != nil {
		return "", fmt.Errorf("template exec: %w", err)
	}
	return buf.String(), nil
}
