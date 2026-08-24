package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"runtime"
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

/*
checkpointRealProcess starts a real process and checkpoints it through svc,
returning the new checkpoint id. The dump stops the process, so the checkpoint
is all that is left of it afterwards — which is exactly the state a restore
has to start from, and the only one it can start from: CRIU restores a process
under its original pid, so a still-running original would hold that pid.
*/
func checkpointRealProcess(t *testing.T, svc *CheckpointService, workloadID string) string {
	t.Helper()
	pid := startDetachedProcess(t, "600")

	stop := false
	result, err := svc.CreateCheckpoint(context.Background(), TargetFromPID(pid), CreateOptions{
		WorkloadID:   workloadID,
		LeaveRunning: &stop,
	})
	if err != nil {
		t.Fatalf("failed to checkpoint test process %d: %v", pid, err)
	}
	return result.CheckpointID
}

// The deliverable, end to end: a workload checkpointed by one linkspan is
// restored by a completely fresh one that shares nothing but the checkpoint
// root, and the caller gets a handle on the restored application itself.
func TestRestoreAcrossServiceInstances(t *testing.T) {
	criuPath := requireCriu(t)
	root := t.TempDir()

	allocationA := newTestService(t, criuPath, root)
	checkpointID := checkpointRealProcess(t, allocationA, "wl-across")

	// A fresh service stands in for the new allocation's linkspan: no
	// in-memory state carries over, only what is on shared storage.
	allocationB := newTestService(t, criuPath, root)
	if got := allocationB.WorkloadState("wl-across"); got != "" {
		t.Fatalf("expected a fresh service to know nothing about the workload, got %q", got)
	}

	result, err := allocationB.RestoreCheckpoint(context.Background(), checkpointID, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	defer pm.GlobalProcessManager.Kill(result.ProcessID)

	if result.WorkloadID != "wl-across" {
		t.Fatalf("expected the workload id to be resolved from disk, got %q", result.WorkloadID)
	}
	if result.Pid <= 0 {
		t.Fatalf("expected the restored root task pid, got %d", result.Pid)
	}
	if err := checkPidExists(result.Pid); err != nil {
		t.Fatalf("restored pid %d is not running: %v", result.Pid, err)
	}
	if result.ProcessID == "" {
		t.Fatalf("expected the restored process to be registered under a process id")
	}
	if got := allocationB.WorkloadState("wl-across"); got != WorkloadRunning {
		t.Fatalf("expected the workload to be running after restore, got %q", got)
	}
}

// The returned process id must address the restored application, so the new
// allocation can manage and re-checkpoint it like any other process.
func TestRestoredProcessIsRegisteredAndCheckpointable(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-registered")

	result, err := svc.RestoreCheckpoint(context.Background(), checkpointID, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	defer pm.GlobalProcessManager.Kill(result.ProcessID)

	managed, err := pm.GlobalProcessManager.GetInfo(result.ProcessID)
	if err != nil {
		t.Fatalf("restored process is not registered: %v", err)
	}
	if managed.Cmd.Process.Pid != result.Pid {
		t.Fatalf("process %s points at pid %d, expected %d", result.ProcessID, managed.Cmd.Process.Pid, result.Pid)
	}
	if !managed.Adopted {
		t.Fatalf("expected the restored process to be tracked as adopted")
	}

	pid, processID, err := svc.resolveTarget(TargetFromProcessID(result.ProcessID))
	if err != nil {
		t.Fatalf("expected the restored process to resolve as a checkpoint target: %v", err)
	}
	if pid != result.Pid || processID != result.ProcessID {
		t.Fatalf("resolved %d/%s, expected %d/%s", pid, processID, result.Pid, result.ProcessID)
	}
}

// Which allocation last restored a workload has to be answerable from shared
// storage, since the linkspan that did it may be long gone.
func TestRestoreWritesRestoreRecord(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-record")

	result, err := svc.RestoreCheckpoint(context.Background(), checkpointID, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	defer pm.GlobalProcessManager.Kill(result.ProcessID)

	record, err := ReadRestoreRecord(checkpointDirPath(svc.criu.CheckpointRoot, "wl-record", checkpointID))
	if err != nil {
		t.Fatalf("expected a restore record on disk: %v", err)
	}
	if record.Pid != result.Pid || record.ProcessID != result.ProcessID {
		t.Fatalf("restore record does not match the restore: %+v", record)
	}
	if record.CheckpointID != checkpointID {
		t.Fatalf("expected checkpoint id %s in the record, got %s", checkpointID, record.CheckpointID)
	}
}

// Nothing may be reconstructed on the host for a checkpoint that cannot be
// restored, so the checkpoint is validated before any command is run.
func TestRestoreValidatesCheckpointBeforeReconstructingEnvironment(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-order")
	dir := checkpointDirPath(svc.criu.CheckpointRoot, "wl-order", checkpointID)

	if err := os.Remove(dir + "/" + completeFileName); err != nil {
		t.Fatalf("failed to remove completion marker: %v", err)
	}

	reconstructed := t.TempDir() + "/reconstructed"
	_, err := svc.RestoreCheckpoint(context.Background(), checkpointID, RestoreOptions{
		PreRestoreCommands: []string{"touch " + reconstructed},
	})
	if err == nil {
		t.Fatalf("expected a partial checkpoint to be refused")
	}
	if !strings.Contains(err.Error(), "checkpoint validation") {
		t.Fatalf("expected the checkpoint validation phase to fail first, got %v", err)
	}
	if _, statErr := os.Stat(reconstructed); statErr == nil {
		t.Fatalf("environment preparation must not run for an invalid checkpoint")
	}
}

// Environment preparation has to precede the host checks, or the files it
// puts in place would be reported missing.
func TestRestorePreparesEnvironmentBeforeHostChecks(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-prep")
	dir := checkpointDirPath(svc.criu.CheckpointRoot, "wl-prep", checkpointID)

	// The executable is recorded as living somewhere that does not exist
	// yet; only the pre-restore command puts it there.
	staged := t.TempDir() + "/staged-app"
	rewriteManifest(t, dir, func(m *Manifest) { m.Executable = staged })

	result, err := svc.RestoreCheckpoint(context.Background(), checkpointID, RestoreOptions{
		PreRestoreCommands: []string{"cp /bin/sleep " + staged},
	})
	if err != nil {
		t.Fatalf("expected the staged executable to satisfy the host checks: %v", err)
	}
	defer pm.GlobalProcessManager.Kill(result.ProcessID)

	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("expected the pre-restore command to have staged the executable: %v", err)
	}
}

func TestRestoreForceOverridesValidationErrors(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-force")
	dir := checkpointDirPath(svc.criu.CheckpointRoot, "wl-force", checkpointID)

	// A recorded dependency that this host genuinely cannot satisfy.
	rewriteManifest(t, dir, func(m *Manifest) {
		m.Mounts = append(m.Mounts, MountPoint{Source: "nfs:/gone", Target: "/not-mounted-here", FSType: "nfs"})
	})

	if _, err := svc.RestoreCheckpoint(context.Background(), checkpointID, RestoreOptions{}); err == nil {
		t.Fatalf("expected the missing mount to block the restore")
	}

	result, err := svc.RestoreCheckpoint(context.Background(), checkpointID, RestoreOptions{Force: true})
	if err != nil {
		t.Fatalf("expected --restore-force to override the blocked restore: %v", err)
	}
	defer pm.GlobalProcessManager.Kill(result.ProcessID)

	if len(result.Warnings) == 0 {
		t.Fatalf("expected the overridden errors to be reported as warnings")
	}
}

func TestRestoreCheckpointRejectsUnknownCheckpoint(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")

	if _, err := svc.RestoreCheckpoint(context.Background(), "ckpt-does-not-exist", RestoreOptions{}); err == nil {
		t.Fatalf("expected restoring an unknown checkpoint id to fail")
	}
	if _, err := svc.RestoreCheckpoint(context.Background(), "", RestoreOptions{}); err == nil {
		t.Fatalf("expected an empty checkpoint id to fail")
	}
}

// A failed restore must leave the workload retryable rather than wedged, so a
// later allocation can try the same checkpoint again.
func TestRestoreFailureLeavesWorkloadRetryable(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-retry")
	dir := checkpointDirPath(svc.criu.CheckpointRoot, "wl-retry", checkpointID)

	rewriteManifest(t, dir, func(m *Manifest) { m.Arch = "someotherarch" })

	if _, err := svc.RestoreCheckpoint(context.Background(), checkpointID, RestoreOptions{}); err == nil {
		t.Fatalf("expected an architecture mismatch to fail the restore")
	}
	if got := svc.WorkloadState("wl-retry"); got != WorkloadRestoreFailed {
		t.Fatalf("expected state %q, got %q", WorkloadRestoreFailed, got)
	}

	// Repairing the manifest must make the workload restorable again rather
	// than leaving it stuck in restore_failed.
	rewriteManifest(t, dir, func(m *Manifest) { m.Arch = runtime.GOARCH })

	result, err := svc.RestoreCheckpoint(context.Background(), checkpointID, RestoreOptions{})
	if err != nil {
		t.Fatalf("expected a retry after a failed restore to succeed: %v", err)
	}
	defer pm.GlobalProcessManager.Kill(result.ProcessID)
}
