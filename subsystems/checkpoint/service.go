package checkpoint

import (
	"context"
	"fmt"
	"log"
	"sync"
	"syscall"
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
// internally (checkpoint ids are globally unique). Only one checkpoint or
// restore may be in flight for that workload at a time.
func (s *CheckpointService) RestoreCheckpoint(ctx context.Context, checkpointID string, opts RestoreOptions) (*RestoreResult, error) {
	if checkpointID == "" {
		return nil, fmt.Errorf("checkpointID is required")
	}
	workloadID, err := findWorkloadForCheckpoint(s.criu.CheckpointRoot, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("failed to locate checkpoint %s: %w", checkpointID, err)
	}

	// An unseen workload defaults to "checkpointed" here (not "running"):
	// this may be the first time this service instance has heard of it,
	// e.g. after a restart, and a durable checkpoint on disk implies
	// exactly that state.
	w := s.getOrCreateWorkload(workloadID, WorkloadCheckpointed)
	if err := w.transition([]WorkloadState{WorkloadCheckpointed, WorkloadRestoreFailed}, WorkloadRestoring); err != nil {
		return nil, fmt.Errorf("cannot restore workload %s: %w", workloadID, err)
	}

	result, err := s.criu.restore(ctx, workloadID, checkpointID)
	if err != nil {
		w.setState(WorkloadRestoreFailed)
		return nil, err
	}
	w.setState(WorkloadRunning)

	if opts.ShutdownOnCompletion && result.Pid > 0 {
		go watchPidAndShutdown(result.Pid, fmt.Sprintf("restored process %d exited", result.Pid))
	}

	return result, nil
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

// watchPidAndShutdown polls pid until it exits, then triggers linkspan
// shutdown. Used after a restore, where the restored process is detached
// from CRIU and therefore not a child linkspan can Wait() on.
func watchPidAndShutdown(pid int, reason string) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
			controller.TriggerShutdown(reason)
			return
		}
	}
}
