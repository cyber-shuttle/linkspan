package workflow

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/cyber-shuttle/linkspan/internal/config"
	pm "github.com/cyber-shuttle/linkspan/internal/process"
	"github.com/cyber-shuttle/linkspan/subsystems/checkpoint"
)

// installCheckpointService points the checkpoint actions at a service backed by
// a scratch checkpoint root. It installs the same package global the REST
// handlers read, which is the whole point: both surfaces act on one service.
func installCheckpointService(t *testing.T, criuPath string) *checkpoint.CheckpointService {
	t.Helper()
	svc := checkpoint.NewCheckpointService(&config.LinkspanConfig{
		CRIUPath:       criuPath,
		CheckpointRoot: t.TempDir(),
		WorkloadID:     "wl-workflow",
	})
	orig := checkpoint.GlobalCheckpointService
	checkpoint.GlobalCheckpointService = svc
	t.Cleanup(func() { checkpoint.GlobalCheckpointService = orig })
	return svc
}

/*
requireUsableCriu skips unless CRIU is installed *and* this host lets it run,
matching requireCriu in the checkpoint package. The path has to be resolved
here: --criu-path is stat'ed as given, not searched for on PATH.
*/
func requireUsableCriu(t *testing.T) *checkpoint.CheckpointService {
	t.Helper()
	path, err := exec.LookPath("criu")
	if err != nil {
		t.Skip("criu is not installed on this host")
	}
	svc := installCheckpointService(t, path)
	if err := svc.Preflight(context.Background()); err != nil {
		t.Skipf("criu is installed but not usable on this host: %v", err)
	}
	return svc
}

func killProcessID(t *testing.T, processID string) {
	t.Helper()
	if processID != "" {
		_ = pm.GlobalProcessManager.Kill(processID)
	}
}

func TestProcessStartRequiresCommand(t *testing.T) {
	if _, err := actionProcessStart(map[string]any{}); err == nil {
		t.Fatalf("expected process.start to require a command")
	}
}

// process.start must return before the application does — that is the whole
// reason it exists next to shell.exec.
func TestProcessStartReturnsWhileApplicationRuns(t *testing.T) {
	result, err := actionProcessStart(map[string]any{"command": "sleep 30"})
	if err != nil {
		t.Fatalf("process.start failed: %v", err)
	}

	processID, _ := (*result)["process_id"].(string)
	pid, _ := (*result)["pid"].(int)
	defer killProcessID(t, processID)

	if processID == "" || pid <= 0 {
		t.Fatalf("expected a process id and pid, got %+v", *result)
	}
	if err := pm.ProcessAlive(pid); err != nil {
		t.Fatalf("expected pid %d to still be running: %v", pid, err)
	}
}

// A process started this way must be checkpointable by process id, which is
// what makes the process.start -> checkpoint.create chain work at all.
func TestProcessStartRegistersACheckpointableTarget(t *testing.T) {
	result, err := actionProcessStart(map[string]any{"command": "sleep 30"})
	if err != nil {
		t.Fatalf("process.start failed: %v", err)
	}
	processID, _ := (*result)["process_id"].(string)
	defer killProcessID(t, processID)

	if _, err := pm.GlobalProcessManager.GetInfo(processID); err != nil {
		t.Fatalf("process.start must register the process so checkpoint.create can resolve it: %v", err)
	}
}

func TestCheckpointActionsRequireConfiguredService(t *testing.T) {
	orig := checkpoint.GlobalCheckpointService
	checkpoint.GlobalCheckpointService = nil
	t.Cleanup(func() { checkpoint.GlobalCheckpointService = orig })

	if _, err := actionCheckpointCreate(map[string]any{"pid": 1}); err == nil {
		t.Fatalf("expected checkpoint.create to refuse without a service")
	}
	if _, err := actionCheckpointRestore(map[string]any{"checkpoint_id": "ckpt-1"}); err == nil {
		t.Fatalf("expected checkpoint.restore to refuse without a service")
	}
}

func TestCheckpointCreateRequiresExactlyOneTarget(t *testing.T) {
	installCheckpointService(t, "/nonexistent/criu")

	if _, err := actionCheckpointCreate(map[string]any{}); err == nil {
		t.Fatalf("expected an error when neither process_id nor pid is given")
	}
	if _, err := actionCheckpointCreate(map[string]any{"process_id": "p-1", "pid": 123}); err == nil {
		t.Fatalf("expected an error when both process_id and pid are given")
	}
}

func TestCheckpointRestoreRequiresCheckpointID(t *testing.T) {
	installCheckpointService(t, "/nonexistent/criu")

	if _, err := actionCheckpointRestore(map[string]any{}); err == nil {
		t.Fatalf("expected checkpoint.restore to require a checkpoint_id")
	}
}

func TestCheckpointCreateRejectsUnknownMode(t *testing.T) {
	installCheckpointService(t, "/nonexistent/criu")

	_, err := actionCheckpointCreate(map[string]any{"pid": 1, "mode": "tpu"})
	if err == nil || !strings.Contains(err.Error(), "unknown checkpoint mode") {
		t.Fatalf("expected an unknown mode to be rejected, got %v", err)
	}
}

// YAML booleans arrive as bool, but a templated value arrives as a string, and
// leave_running has to mean the same thing either way.
func TestBoolParamAcceptsYAMLAndTemplatedValues(t *testing.T) {
	if !toBool(true) || !toBool("true") || !toBool("True") {
		t.Fatalf("expected true from bool and string forms")
	}
	if toBool(false) || toBool("false") || toBool("nonsense") || toBool(nil) {
		t.Fatalf("expected false from false, non-boolean, and missing values")
	}
	if stringSliceParam(map[string]any{}, "absent") != nil {
		t.Fatalf("an absent list must be nil so defaults survive")
	}
	got := stringSliceParam(map[string]any{"cmds": []any{"a", "b"}}, "cmds")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("expected [a b], got %v", got)
	}
}

// The registry is what a workflow YAML actually reaches, so the new actions
// have to be resolvable by the exact names the README will document.
func TestNewActionsAreRegistered(t *testing.T) {
	reg := DefaultRegistry()
	for _, name := range []string{"process.start", "checkpoint.create", "checkpoint.restore"} {
		if reg.Get(name) == nil {
			t.Fatalf("action %q is not registered", name)
		}
	}
}

/*
The stage 6 deliverable, driven the way a real workflow would be: sequential
steps, each one's outputs feeding the next through {{.var}} interpolation.

	process.start -> process_id -> checkpoint.create -> checkpoint_id -> checkpoint.restore

leave_running is false here because CRIU restores a process under its original
pid, so the original has to be gone before the restore can take that pid back.
*/
func TestProcessStartCheckpointRestoreThroughEngine(t *testing.T) {
	svc := requireUsableCriu(t)

	yaml := `
name: checkpoint roundtrip
steps:
  - name: start the application
    action: process.start
    params:
      command: sleep 600
    outputs:
      process_id: app_process_id
      pid: app_pid
  - name: checkpoint the application
    action: checkpoint.create
    params:
      process_id: "{{.app_process_id}}"
      leave_running: false
      reason: stage 6 workflow roundtrip
    outputs:
      checkpoint_id: ckpt_id
      checkpoint_path: ckpt_path
      status: ckpt_status
  - name: restore the application
    action: checkpoint.restore
    params:
      checkpoint_id: "{{.ckpt_id}}"
    outputs:
      process_id: restored_process_id
      pid: restored_pid
`
	wf, err := LoadReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("failed to parse workflow: %v", err)
	}

	engine := NewEngine(DefaultRegistry(), nil)
	if err := engine.Run(context.Background(), wf); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	status := engine.Status()
	if status.State != StateComplete {
		t.Fatalf("expected the workflow to complete, got %q (%s)", status.State, status.Error)
	}

	if got, _ := status.Outputs["ckpt_status"].(string); got != string(checkpoint.StateComplete) {
		t.Fatalf("expected a complete checkpoint, got status %q", got)
	}
	ckptPath, _ := status.Outputs["ckpt_path"].(string)
	if ckptPath == "" || !strings.Contains(ckptPath, "wl-workflow") {
		t.Fatalf("expected checkpoint_path under the workload directory, got %q", ckptPath)
	}

	restoredPID, _ := status.Outputs["restored_pid"].(int)
	restoredProcessID, _ := status.Outputs["restored_process_id"].(string)
	defer func() { _ = syscall.Kill(restoredPID, syscall.SIGKILL) }()

	if restoredProcessID == "" || restoredPID <= 0 {
		t.Fatalf("expected the restored application's identity, got %+v", status.Outputs)
	}
	if err := pm.ProcessAlive(restoredPID); err != nil {
		t.Fatalf("restored pid %d is not running: %v", restoredPID, err)
	}

	// The checkpoint the workflow took must be visible through the same
	// service the REST endpoints use — one service, not two.
	manifests, err := svc.ListCheckpoints()
	if err != nil {
		t.Fatalf("failed to list checkpoints: %v", err)
	}
	ckptID, _ := status.Outputs["ckpt_id"].(string)
	found := false
	for _, m := range manifests {
		if m.CheckpointID == ckptID {
			found = true
			if m.Trigger != checkpoint.TriggerWorkflow {
				t.Fatalf("expected the workflow trigger to be recorded, got %q", m.Trigger)
			}
			if m.Reason != "stage 6 workflow roundtrip" {
				t.Fatalf("expected the reason to be recorded, got %q", m.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("checkpoint %s taken by the workflow is not visible through the shared service", ckptID)
	}
}

// A workflow step and a REST call must not each get their own service, or the
// per-workload state machine stops serializing anything.
func TestWorkflowAndRESTShareOneService(t *testing.T) {
	svc := installCheckpointService(t, "/nonexistent/criu")

	active, err := checkpoint.ActiveService()
	if err != nil {
		t.Fatalf("ActiveService failed: %v", err)
	}
	if active != svc {
		t.Fatalf("expected the workflow actions to resolve the same service instance the REST handlers use")
	}
}
