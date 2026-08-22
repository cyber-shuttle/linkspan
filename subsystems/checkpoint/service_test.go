package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

// captureLogs redirects the standard logger to a buffer for the duration of
// the test, so a ScheduleCheckpoint goroutine's branch can be observed
// without a real CRIU binary.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

func startTestProcess(t *testing.T, args ...string) string {
	t.Helper()
	id, err := pm.GlobalProcessManager.Start(exec.Command(args[0], args[1:]...), false)
	if err != nil {
		t.Fatalf("failed to start test process: %v", err)
	}
	return id
}

// newTestService builds a CheckpointService directly (bypassing
// NewCheckpointService/config.LinkspanConfig) so tests can control CriuPath
// and CheckpointRoot precisely. root == "" gets a fresh t.TempDir().
func newTestService(t *testing.T, criuPath, root string) *CheckpointService {
	t.Helper()
	if root == "" {
		root = t.TempDir()
	}
	return &CheckpointService{
		criu: &criuCheckpointer{
			CriuPath:       criuPath,
			CheckpointRoot: root,
		},
		workloads: make(map[string]*workloadEntry),
	}
}

func TestScheduleCheckpoint_TimerFires(t *testing.T) {
	buf := captureLogs(t)

	id := startTestProcess(t, "sleep", "5")
	defer pm.GlobalProcessManager.Kill(id)

	svc := newTestService(t, "/nonexistent/criu", "")
	target := TargetFromProcessID(id)
	opts := CreateOptions{WorkloadID: "wl-test"}

	if err := svc.ScheduleCheckpoint(context.Background(), target, opts, 150*time.Millisecond); err != nil {
		t.Fatalf("ScheduleCheckpoint returned error: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	logs := buf.String()
	if !strings.Contains(logs, "delay elapsed, checkpointing process "+id) {
		t.Fatalf("expected timer-fired log line, got logs:\n%s", logs)
	}
	// CriuPath is bogus, so the checkpoint attempt itself must fail preflight
	// — but it must have been *attempted*.
	if !strings.Contains(logs, "delayed checkpoint of process "+id+" failed") {
		t.Fatalf("expected a failed checkpoint attempt to be logged, got logs:\n%s", logs)
	}
}

func TestScheduleCheckpoint_ProcessCompletesFirst(t *testing.T) {
	buf := captureLogs(t)

	id := startTestProcess(t, "sleep", "0.1")

	svc := newTestService(t, "/nonexistent/criu", "")
	target := TargetFromProcessID(id)
	opts := CreateOptions{WorkloadID: "wl-test"}

	if err := svc.ScheduleCheckpoint(context.Background(), target, opts, 3*time.Second); err != nil {
		t.Fatalf("ScheduleCheckpoint returned error: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	logs := buf.String()
	if !strings.Contains(logs, "completed before the checkpoint delay elapsed; skipping checkpoint") {
		t.Fatalf("expected early-completion log line, got logs:\n%s", logs)
	}
	if strings.Contains(logs, "delay elapsed, checkpointing process") {
		t.Fatalf("checkpoint should not have been attempted once the process exited early, got logs:\n%s", logs)
	}
}

func TestScheduleCheckpoint_ContextCanceled(t *testing.T) {
	buf := captureLogs(t)

	id := startTestProcess(t, "sleep", "5")
	defer pm.GlobalProcessManager.Kill(id)

	ctx, cancel := context.WithCancel(context.Background())

	svc := newTestService(t, "/nonexistent/criu", "")
	target := TargetFromProcessID(id)
	opts := CreateOptions{WorkloadID: "wl-test"}

	if err := svc.ScheduleCheckpoint(ctx, target, opts, 3*time.Second); err != nil {
		t.Fatalf("ScheduleCheckpoint returned error: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(300 * time.Millisecond)

	logs := buf.String()
	if !strings.Contains(logs, "linkspan is shutting down; canceling scheduled checkpoint") {
		t.Fatalf("expected shutdown-cancellation log line, got logs:\n%s", logs)
	}
	if strings.Contains(logs, "delay elapsed, checkpointing process") {
		t.Fatalf("checkpoint should not have been attempted after shutdown, got logs:\n%s", logs)
	}
}

func TestScheduleCheckpoint_InvalidDelay(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")
	target := TargetFromProcessID("whatever")
	if err := svc.ScheduleCheckpoint(context.Background(), target, CreateOptions{WorkloadID: "wl-test"}, 0); err == nil {
		t.Fatalf("expected an error for a non-positive delay")
	}
}

func TestScheduleCheckpoint_UnknownProcess(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")
	target := TargetFromProcessID("not-a-real-id")
	if err := svc.ScheduleCheckpoint(context.Background(), target, CreateOptions{WorkloadID: "wl-test"}, time.Second); err == nil {
		t.Fatalf("expected an error for an unknown process id")
	}
}

func TestScheduleCheckpoint_RequiresProcessIDTarget(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")
	target := TargetFromPID(os.Getpid())
	if err := svc.ScheduleCheckpoint(context.Background(), target, CreateOptions{WorkloadID: "wl-test"}, time.Second); err == nil {
		t.Fatalf("expected an error scheduling a pid target (only process_id targets are supported)")
	}
}

func TestResolveTarget_ProcessID(t *testing.T) {
	id := startTestProcess(t, "sleep", "5")
	defer pm.GlobalProcessManager.Kill(id)

	svc := newTestService(t, "/nonexistent/criu", "")
	pid, processID, err := svc.resolveTarget(TargetFromProcessID(id))
	if err != nil {
		t.Fatalf("resolveTarget failed: %v", err)
	}
	if processID != id {
		t.Fatalf("expected processID %s, got %s", id, processID)
	}
	if pid <= 0 {
		t.Fatalf("expected a positive pid, got %d", pid)
	}
}

func TestResolveTarget_PID(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")
	pid, processID, err := svc.resolveTarget(TargetFromPID(os.Getpid()))
	if err != nil {
		t.Fatalf("resolveTarget failed: %v", err)
	}
	if processID != "" {
		t.Fatalf("expected empty processID for a pid target, got %q", processID)
	}
	if pid != os.Getpid() {
		t.Fatalf("expected pid %d, got %d", os.Getpid(), pid)
	}
}

func TestResolveTarget_UnknownProcessID(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")
	if _, _, err := svc.resolveTarget(TargetFromProcessID("not-a-real-id")); err == nil {
		t.Fatalf("expected an error for an unknown process id")
	}
}

func TestResolveTarget_NonexistentPID(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to run helper process: %v", err)
	}
	svc := newTestService(t, "/nonexistent/criu", "")
	if _, _, err := svc.resolveTarget(TargetFromPID(cmd.Process.Pid)); err == nil {
		t.Fatalf("expected an error for an exited pid")
	}
}

func TestWorkloadStateMachine_PreventsOverlap(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")
	w := svc.getOrCreateWorkload("wl-a", WorkloadRunning)

	if err := w.transition([]WorkloadState{WorkloadRunning}, WorkloadCheckpointing); err != nil {
		t.Fatalf("first checkpoint transition should succeed: %v", err)
	}

	// checkpoint + checkpoint
	if err := w.transition([]WorkloadState{WorkloadRunning, WorkloadCheckpointFailed}, WorkloadCheckpointing); !errors.Is(err, ErrWorkloadBusy) {
		t.Fatalf("expected ErrWorkloadBusy for a concurrent checkpoint, got %v", err)
	}

	// checkpoint + restore
	if err := w.transition([]WorkloadState{WorkloadCheckpointed, WorkloadRestoreFailed}, WorkloadRestoring); !errors.Is(err, ErrWorkloadBusy) {
		t.Fatalf("expected ErrWorkloadBusy for a restore while checkpointing, got %v", err)
	}

	w.setState(WorkloadCheckpointed)

	if err := w.transition([]WorkloadState{WorkloadCheckpointed, WorkloadRestoreFailed}, WorkloadRestoring); err != nil {
		t.Fatalf("restore transition should succeed once checkpointed: %v", err)
	}

	// restore + restore
	if err := w.transition([]WorkloadState{WorkloadCheckpointed, WorkloadRestoreFailed}, WorkloadRestoring); !errors.Is(err, ErrWorkloadBusy) {
		t.Fatalf("expected ErrWorkloadBusy for a concurrent restore, got %v", err)
	}
}

func TestWorkloadStateMachine_DifferentWorkloadsIndependent(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")

	a := svc.getOrCreateWorkload("wl-a", WorkloadRunning)
	if err := a.transition([]WorkloadState{WorkloadRunning}, WorkloadCheckpointing); err != nil {
		t.Fatalf("workload a transition should succeed: %v", err)
	}

	b := svc.getOrCreateWorkload("wl-b", WorkloadRunning)
	if err := b.transition([]WorkloadState{WorkloadRunning}, WorkloadCheckpointing); err != nil {
		t.Fatalf("workload b should be unaffected by workload a's in-flight checkpoint: %v", err)
	}
}

func TestWorkloadStateMachine_RetryAfterFailure(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")
	w := svc.getOrCreateWorkload("wl-a", WorkloadRunning)
	w.setState(WorkloadCheckpointFailed)

	if err := w.transition([]WorkloadState{WorkloadRunning, WorkloadCheckpointFailed}, WorkloadCheckpointing); err != nil {
		t.Fatalf("expected retry from checkpoint_failed to succeed: %v", err)
	}
}

func TestFreshWorkloadDefaults(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")

	if got := svc.WorkloadState("wl-unseen"); got != "" {
		t.Fatalf("expected no tracked state for an unseen workload, got %q", got)
	}

	// CreateCheckpoint's entry point defaults an unseen workload to "running".
	if got := svc.getOrCreateWorkload("wl-a", WorkloadRunning).getState(); got != WorkloadRunning {
		t.Fatalf("expected default state running, got %q", got)
	}

	// RestoreCheckpoint's entry point defaults an unseen workload to
	// "checkpointed" -- this is what makes restoring a durable checkpoint
	// work after a service restart, when the in-memory map starts empty.
	if got := svc.getOrCreateWorkload("wl-b", WorkloadCheckpointed).getState(); got != WorkloadCheckpointed {
		t.Fatalf("expected default state checkpointed, got %q", got)
	}
}

func TestCreateCheckpoint_RejectsOverlap(t *testing.T) {
	id := startTestProcess(t, "sleep", "5")
	defer pm.GlobalProcessManager.Kill(id)

	svc := newTestService(t, "/nonexistent/criu", "")
	target := TargetFromProcessID(id)
	opts := CreateOptions{WorkloadID: "wl-overlap"}

	// Simulate an in-flight operation without needing a real CRIU binary to
	// get there.
	svc.getOrCreateWorkload(opts.WorkloadID, WorkloadRunning).setState(WorkloadCheckpointing)

	if _, err := svc.CreateCheckpoint(context.Background(), target, opts); !errors.Is(err, ErrWorkloadBusy) {
		t.Fatalf("expected ErrWorkloadBusy for a concurrent checkpoint, got %v", err)
	}
}

func TestRestoreCheckpoint_RejectsOverlap(t *testing.T) {
	root := t.TempDir()
	workloadID, checkpointID := "wl-restore-overlap", "ckpt-1"
	dir := checkpointDirPath(root, workloadID, checkpointID)
	if err := os.MkdirAll(imagesDirPath(dir), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeManifest(dir, &Manifest{State: StateComplete}); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	if err := os.WriteFile(dir+"/"+completeFileName, []byte{}, 0644); err != nil {
		t.Fatalf("write COMPLETE: %v", err)
	}

	svc := newTestService(t, "/nonexistent/criu", root)
	svc.getOrCreateWorkload(workloadID, WorkloadCheckpointed).setState(WorkloadRestoring)

	if _, err := svc.RestoreCheckpoint(context.Background(), checkpointID, RestoreOptions{}); !errors.Is(err, ErrWorkloadBusy) {
		t.Fatalf("expected ErrWorkloadBusy for a concurrent restore, got %v", err)
	}
}

func TestRestoreCheckpoint_FreshServiceDefaultsToCheckpointed(t *testing.T) {
	criuPath, err := exec.LookPath("criu")
	if err != nil {
		t.Skip("criu is not installed on this host; skipping end-to-end restore test")
	}

	root := t.TempDir()
	workloadID, checkpointID := "wl-fresh", "ckpt-fresh"
	if err := os.MkdirAll(imagesDirPath(checkpointDirPath(root, workloadID, checkpointID)), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Deliberately no manifest/COMPLETE -- the restore should still be
	// refused (proving the fresh service, with empty in-memory state,
	// correctly falls through to the on-disk completion gate rather than
	// e.g. rejecting for a bad state-machine reason).

	svc := newTestService(t, criuPath, root)

	if _, err := svc.RestoreCheckpoint(context.Background(), checkpointID, RestoreOptions{}); err == nil {
		t.Fatalf("expected restore to be refused for an incomplete checkpoint")
	}

	if got := svc.WorkloadState(workloadID); got != WorkloadRestoreFailed {
		t.Fatalf("expected workload state restore_failed after a refused restore, got %q", got)
	}
}
