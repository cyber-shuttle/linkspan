package checkpoint

import (
	"errors"
	"time"
)

// CheckpointResult describes the outcome of a successful CRIU dump.
type CheckpointResult struct {
	WorkloadID   string
	CheckpointID string
	ProcessID    string
	Pid          int
	ImagesDir    string // directory containing the CRIU checkpoint images
	ManifestPath string
	LogFile      string // CRIU's own dump log, sibling of ImagesDir
	ExitCode     int
	Stdout       string
	Stderr       string
	StartedAt    time.Time
	FinishedAt   time.Time
}

// RestoreResult describes the outcome of a successful CRIU restore.
type RestoreResult struct {
	WorkloadID   string
	CheckpointID string
	ImagesDir    string
	ManifestPath string
	LogFile      string
	ExitCode     int
	Stdout       string
	Stderr       string
	StartedAt    time.Time
	FinishedAt   time.Time
	Pid          int // restored root task PID, captured via --pidfile
}

// TargetKind identifies how a CheckpointTarget names the process to act on.
type TargetKind string

const (
	// TargetKindProcessID targets a process started through linkspan's
	// ProcessManager, identified by its internal id.
	TargetKindProcessID TargetKind = "process_id"
	// TargetKindPID targets an already-running process by raw OS PID,
	// regardless of whether linkspan started it.
	TargetKindPID TargetKind = "pid"
)

// CheckpointTarget identifies the process a checkpoint operation acts on.
type CheckpointTarget struct {
	Kind      TargetKind
	ProcessID string // set when Kind == TargetKindProcessID
	PID       int    // set when Kind == TargetKindPID
}

func TargetFromProcessID(id string) CheckpointTarget {
	return CheckpointTarget{Kind: TargetKindProcessID, ProcessID: id}
}

func TargetFromPID(pid int) CheckpointTarget {
	return CheckpointTarget{Kind: TargetKindPID, PID: pid}
}

// CreateOptions configures a CreateCheckpoint call.
type CreateOptions struct {
	WorkloadID string
	// Trigger defaults to TriggerManual if left empty.
	Trigger CheckpointTrigger
}

// RestoreOptions configures a RestoreCheckpoint call.
type RestoreOptions struct {
	// ShutdownOnCompletion triggers linkspan shutdown once the restored
	// process exits.
	ShutdownOnCompletion bool
}

// WorkloadState is a workload's position in the in-memory state machine
// CheckpointService uses to serialize checkpoint/restore operations. This
// is distinct from CheckpointState (manifest.go), which records one
// checkpoint's own on-disk lifecycle.
type WorkloadState string

const (
	WorkloadRunning          WorkloadState = "running"
	WorkloadCheckpointing    WorkloadState = "checkpointing"
	WorkloadCheckpointed     WorkloadState = "checkpointed"
	WorkloadRestoring        WorkloadState = "restoring"
	WorkloadCheckpointFailed WorkloadState = "checkpoint_failed"
	WorkloadRestoreFailed    WorkloadState = "restore_failed"
)

// ErrWorkloadBusy is returned (wrapped) when an operation is requested on a
// workload that is not in a state that allows it — e.g. a second checkpoint
// or restore while one is already in flight.
var ErrWorkloadBusy = errors.New("workload is not in a state that allows this operation")
