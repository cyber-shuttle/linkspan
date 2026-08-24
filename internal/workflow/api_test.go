package workflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getStatus(t *testing.T) (int, Status) {
	t.Helper()
	rr := httptest.NewRecorder()
	StatusHandler(rr, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))

	var s Status
	if err := json.Unmarshal(rr.Body.Bytes(), &s); err != nil {
		t.Fatalf("failed to decode status %q: %v", rr.Body.String(), err)
	}
	return rr.Code, s
}

func installEngine(t *testing.T, e *Engine) {
	t.Helper()
	orig := GlobalEngine
	GlobalEngine = e
	t.Cleanup(func() { GlobalEngine = orig })
}

// An allocation started without --workflow still has to answer /status, or a
// client polling for progress cannot tell "no workflow" from "server down".
func TestStatusReportsIdleWithoutAWorkflow(t *testing.T) {
	installEngine(t, nil)

	code, s := getStatus(t)
	if code != http.StatusOK {
		t.Fatalf("expected 200 with no workflow, got %d", code)
	}
	if s.State != StateIdle {
		t.Fatalf("expected state %q, got %q", StateIdle, s.State)
	}
	if s.Outputs == nil {
		t.Fatalf("expected an empty outputs object rather than null")
	}
}

// /status is what an operator watches while a workflow runs, so it has to
// report the step that ran, the outputs it captured, and the final state.
func TestStatusReportsProgressAndOutputs(t *testing.T) {
	engine := NewEngine(DefaultRegistry(), nil)
	installEngine(t, engine)

	if _, s := getStatus(t); s.State != StateIdle {
		t.Fatalf("a fresh engine should report idle, got %q", s.State)
	}

	wf, err := LoadReader(strings.NewReader(`
name: status probe
steps:
  - name: say hello
    action: shell.exec
    params:
      command: echo hello-from-workflow
    outputs:
      output: greeting
`))
	if err != nil {
		t.Fatalf("failed to parse workflow: %v", err)
	}
	if err := engine.Run(context.Background(), wf); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	code, s := getStatus(t)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if s.State != StateComplete {
		t.Fatalf("expected state %q, got %q (%s)", StateComplete, s.State, s.Error)
	}
	if s.TotalSteps != 1 || s.CurrentStep != 1 {
		t.Fatalf("expected 1/1 steps, got %d/%d", s.CurrentStep, s.TotalSteps)
	}
	if got, _ := s.Outputs["greeting"].(string); got != "hello-from-workflow" {
		t.Fatalf("expected the captured output over /status, got %q", got)
	}
}

// A failed step must be visible over /status rather than only in the log.
func TestStatusReportsAFailedWorkflow(t *testing.T) {
	engine := NewEngine(DefaultRegistry(), nil)
	installEngine(t, engine)

	wf, err := LoadReader(strings.NewReader(`
name: doomed
steps:
  - name: run something that is not an action
    action: nope.does_not_exist
`))
	if err != nil {
		t.Fatalf("failed to parse workflow: %v", err)
	}
	if err := engine.Run(context.Background(), wf); err == nil {
		t.Fatalf("expected the workflow to fail on an unknown action")
	}

	_, s := getStatus(t)
	if s.State != StateFailed {
		t.Fatalf("expected state %q, got %q", StateFailed, s.State)
	}
	if !strings.Contains(s.Error, "unknown action") {
		t.Fatalf("expected the error to name the unknown action, got %q", s.Error)
	}
}

// Reading status must not hand out the engine's own map: a client mutating it
// would corrupt workflow variables mid-run.
func TestStatusOutputsAreACopy(t *testing.T) {
	engine := NewEngine(DefaultRegistry(), map[string]any{})
	installEngine(t, engine)

	wf, _ := LoadReader(strings.NewReader(`
name: copy check
steps:
  - name: emit
    action: shell.exec
    params:
      command: echo value
    outputs:
      output: captured
`))
	if err := engine.Run(context.Background(), wf); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	first := engine.Status()
	first.Outputs["captured"] = "tampered"

	if got, _ := engine.Status().Outputs["captured"].(string); got != "value" {
		t.Fatalf("mutating a status snapshot changed engine state, got %q", got)
	}
}
