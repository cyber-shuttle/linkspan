package checkpoint

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/config"
	"github.com/cyber-shuttle/linkspan/internal/controller"
	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

// workloadEntry guards one workload's position in the state machine. The
// mutex is held only long enough to check-and-set the state, never for the
// duration of the actual CRIU call, so a busy workload is rejected
// immediately rather than making the caller block behind it.
type workloadEntry struct {
	mu    sync.Mutex
	state WorkloadState
}

func (w *workloadEntry) transition(from []WorkloadState, to WorkloadState) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, f := range from {
		if w.state == f {
			w.state = to
			return nil
		}
	}
	return fmt.Errorf("%w: current state is %q, expected one of %v", ErrWorkloadBusy, w.state, from)
}

func (w *workloadEntry) setState(s WorkloadState) {
	w.mu.Lock()
	w.state = s
	w.mu.Unlock()
}

func (w *workloadEntry) getState() WorkloadState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// CheckpointService is the single entry point for checkpoint/restore
// orchestration. main.go — and eventually any HTTP/workflow layer — should
// only ever talk to this, never to criuCheckpointer directly; that type is
// unexported specifically to enforce the boundary at compile time.
type CheckpointService struct {
	criu *criuCheckpointer

	mu        sync.Mutex
	workloads map[string]*workloadEntry
}

func NewCheckpointService(c *config.LinkspanConfig) *CheckpointService {
	return &CheckpointService{
		criu:      newCriuCheckpointer(c),
		workloads: make(map[string]*workloadEntry),
	}
}

func (s *CheckpointService) getOrCreateWorkload(workloadID string, defaultState WorkloadState) *workloadEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workloads[workloadID]
	if !ok {
		w = &workloadEntry{state: defaultState}
		s.workloads[workloadID] = w
	}
	return w
}

// WorkloadState reports the currently tracked state of a workload, or ""
// if this service instance has never seen it.
func (s *CheckpointService) WorkloadState(workloadID string) WorkloadState {
	s.mu.Lock()
	w, ok := s.workloads[workloadID]
	s.mu.Unlock()
	if !ok {
		return ""
	}
	return w.getState()
}

// resolveTarget turns a CheckpointTarget into a concrete PID (and, for
// process_id targets, linkspan's internal process id for provenance).
func (s *CheckpointService) resolveTarget(target CheckpointTarget) (pid int, processID string, err error) {
	switch target.Kind {
	case TargetKindProcessID:
		if target.ProcessID == "" {
			return 0, "", fmt.Errorf("process_id target requires a non-empty ProcessID")
		}
		managed, err := pm.GlobalProcessManager.GetInfo(target.ProcessID)
		if err != nil {
			return 0, "", fmt.Errorf("failed to look up process %s: %w", target.ProcessID, err)
		}
		pid := managed.Cmd.Process.Pid
		if pid <= 0 {
			return 0, "", fmt.Errorf("invalid PID for process %s", target.ProcessID)
		}
		return pid, target.ProcessID, nil
	case TargetKindPID:
		if target.PID <= 0 {
			return 0, "", fmt.Errorf("pid target requires a positive PID")
		}
		if err := checkPidExists(target.PID); err != nil {
			return 0, "", err
		}
		return target.PID, "", nil
	default:
		return 0, "", fmt.Errorf("unknown checkpoint target kind %q", target.Kind)
	}
}

// CreateCheckpoint checkpoints target under opts.WorkloadID. Only one
// checkpoint or restore may be in flight for a given workload at a time —
// a concurrent call on the same workload fails fast with ErrWorkloadBusy.
func (s *CheckpointService) CreateCheckpoint(ctx context.Context, target CheckpointTarget, opts CreateOptions) (*CheckpointResult, error) {
	if opts.WorkloadID == "" {
		return nil, fmt.Errorf("WorkloadID is required")
	}
	trigger := opts.Trigger
	if trigger == "" {
		trigger = TriggerManual
	}

	pid, processID, err := s.resolveTarget(target)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve checkpoint target: %w", err)
	}

	w := s.getOrCreateWorkload(opts.WorkloadID, WorkloadRunning)
	if err := w.transition([]WorkloadState{WorkloadRunning, WorkloadCheckpointFailed}, WorkloadCheckpointing); err != nil {
		return nil, fmt.Errorf("cannot checkpoint workload %s: %w", opts.WorkloadID, err)
	}

	result, err := s.criu.checkpoint(ctx, opts.WorkloadID, processID, pid, trigger)
	if err != nil {
		w.setState(WorkloadCheckpointFailed)
		return nil, err
	}
	w.setState(WorkloadCheckpointed)
	return result, nil
}

// RestoreCheckpoint restores checkpointID, resolving its owning workload
// internally, and registers the restored application so it has a durable
// identity in this allocation. Only one operation may be in flight per
// workload at a time.
func (s *CheckpointService) RestoreCheckpoint(ctx context.Context, checkpointID string, opts RestoreOptions) (*RestoreResult, error) {
	if checkpointID == "" {
		return nil, fmt.Errorf("checkpointID is required")
	}
	workloadID, err := findWorkloadForCheckpoint(s.criu.CheckpointRoot, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to locate checkpoint %s: %w", checkpointID, err)
	}
	checkpointDir := checkpointDirPath(s.criu.CheckpointRoot, workloadID, checkpointID)

	// A workload this instance has never seen defaults to "checkpointed":
	// a durable checkpoint on disk implies exactly that.
	w := s.getOrCreateWorkload(workloadID, WorkloadCheckpointed)
	if err := w.transition([]WorkloadState{WorkloadCheckpointed, WorkloadRestoreFailed}, WorkloadRestoring); err != nil {
		return nil, fmt.Errorf("cannot restore workload %s: %w", workloadID, err)
	}

	// validateCheckpoint reports a nil manifest as a fatal finding.
	manifest, err := ReadManifest(checkpointDir)
	if err != nil {
		manifest = nil
	}

	warnings, err := s.runRestorePhases(ctx, checkpointDir, manifest, opts)
	if err != nil {
		w.setState(WorkloadRestoreFailed)
		return nil, err
	}

	result, err := s.criu.restore(ctx, workloadID, checkpointID)
	if err != nil {
		w.setState(WorkloadRestoreFailed)
		return nil, err
	}
	result.Warnings = warnings

	// Registered so callers hold the application itself, not the CRIU
	// command that restored it and has already exited.
	processID, err := pm.GlobalProcessManager.Adopt(result.Pid)
	if err != nil {
		w.setState(WorkloadRestoreFailed)
		return nil, fmt.Errorf("restore of %s succeeded (pid %d) but the restored process could not be registered: %w", checkpointID, result.Pid, err)
	}
	result.ProcessID = processID

	if err := writeRestoreRecord(checkpointDir, newRestoreRecord(result, s.criu.LinkspanVersion)); err != nil {
		log.Printf("[Checkpoint] warning: failed to record restore of %s: %v", checkpointID, err)
	}

	w.setState(WorkloadRunning)
	log.Printf("[Checkpoint] workload %s restored from %s as process %s (pid %d)", workloadID, checkpointID, processID, result.Pid)

	if opts.ShutdownOnCompletion {
		go shutdownWhenProcessExits(processID, fmt.Sprintf("restored process %d exited", result.Pid))
	}

	return result, nil
}

// runRestorePhases runs the pre-CRIU phases, returning the warnings they
// gathered. Force downgrades a phase's errors rather than skipping the phase,
// so a forced restore still logs what it overrode.
func (s *CheckpointService) runRestorePhases(ctx context.Context, checkpointDir string, manifest *Manifest, opts RestoreOptions) ([]string, error) {
	var warnings []string

	// Ordered: a bad checkpoint stops everything before the host is touched,
	// and the host checks must see what environment preparation rebuilt.
	phases := []struct {
		name string
		run  func() *RestoreValidation
	}{
		{"checkpoint validation", func() *RestoreValidation { return validateCheckpoint(checkpointDir, manifest) }},
		{"environment preparation", func() *RestoreValidation { return prepareRestoreEnvironment(ctx, manifest, opts) }},
		{"host compatibility", func() *RestoreValidation { return s.criu.validateHostCompatibility(ctx, manifest) }},
	}

	for _, phase := range phases {
		v := phase.run()
		if opts.Force && !v.OK() {
			log.Printf("[Checkpoint] --restore-force: overriding %d %s error(s)", len(v.Errors), phase.name)
			v.downgrade()
		}
		v.log(phase.name)
		warnings = append(warnings, v.Warnings...)
		if err := v.Err(); err != nil {
			return warnings, fmt.Errorf("%s: %w", phase.name, err)
		}
	}
	return warnings, nil
}

// ValidateRestore reports whether checkpointID could be restored here,
// without restoring it.
func (s *CheckpointService) ValidateRestore(ctx context.Context, checkpointID string) (*RestoreValidation, error) {
	if checkpointID == "" {
		return nil, fmt.Errorf("checkpointID is required")
	}
	workloadID, err := findWorkloadForCheckpoint(s.criu.CheckpointRoot, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to locate checkpoint %s: %w", checkpointID, err)
	}
	checkpointDir := checkpointDirPath(s.criu.CheckpointRoot, workloadID, checkpointID)

	manifest, err := ReadManifest(checkpointDir)
	if err != nil {
		manifest = nil
	}

	v := validateCheckpoint(checkpointDir, manifest)
	v.merge(s.criu.validateHostCompatibility(ctx, manifest))
	return v, nil
}

// GetCheckpoint returns a checkpoint's manifest regardless of its state
// (including failed attempts) — the manifest's own State field communicates
// whether it's usable.
func (s *CheckpointService) GetCheckpoint(checkpointID string) (*Manifest, error) {
	if checkpointID == "" {
		return nil, fmt.Errorf("checkpointID is required")
	}
	workloadID, err := findWorkloadForCheckpoint(s.criu.CheckpointRoot, checkpointID)
	if err != nil {
		return nil, err
	}
	return ReadManifest(checkpointDirPath(s.criu.CheckpointRoot, workloadID, checkpointID))
}

// ListCheckpoints returns every checkpoint's manifest across all workloads;
// callers filter/group by Manifest.WorkloadID as needed.
func (s *CheckpointService) ListCheckpoints() ([]*Manifest, error) {
	return listManifests(s.criu.CheckpointRoot)
}

// ScheduleCheckpoint waits for delay to elapse, then creates a checkpoint —
// unless the target process completes first, or ctx is canceled (e.g.
// linkspan shutting down) first, in which case the checkpoint is never
// attempted. Currently only process_id targets support scheduling, since
// only those have a ProcessManager Done channel to race against.
func (s *CheckpointService) ScheduleCheckpoint(ctx context.Context, target CheckpointTarget, opts CreateOptions, delay time.Duration) error {
	if delay <= 0 {
		return fmt.Errorf("delay must be greater than 0")
	}
	if target.Kind != TargetKindProcessID {
		return fmt.Errorf("ScheduleCheckpoint currently only supports process_id targets")
	}

	intProcess, err := pm.GlobalProcessManager.GetInfo(target.ProcessID)
	if err != nil {
		return fmt.Errorf("failed to look up process %s: %w", target.ProcessID, err)
	}

	go func() {
		log.Printf("[Checkpoint] waiting %s before checkpointing process %s", delay, target.ProcessID)

		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
			log.Printf("[Checkpoint] delay elapsed, checkpointing process %s", target.ProcessID)
		case <-intProcess.Done:
			log.Printf("[Checkpoint] process %s completed before the checkpoint delay elapsed; skipping checkpoint", target.ProcessID)
			return
		case <-ctx.Done():
			log.Printf("[Checkpoint] linkspan is shutting down; canceling scheduled checkpoint for process %s", target.ProcessID)
			return
		}

		if _, err := s.CreateCheckpoint(ctx, target, opts); err != nil {
			log.Printf("[Checkpoint] delayed checkpoint of process %s failed: %v", target.ProcessID, err)
		}
	}()

	return nil
}

// shutdownWhenProcessExits shuts linkspan down once the restored process
// exits. Not our child, so the exit comes from the adopted-process poll.
func shutdownWhenProcessExits(processID, reason string) {
	if err := pm.GlobalProcessManager.Wait(processID); err != nil {
		log.Printf("[Checkpoint] restored process %s ended with error: %v", processID, err)
	}
	controller.TriggerShutdown(reason)
}
