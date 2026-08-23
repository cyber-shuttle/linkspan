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

// RestoreResult describes the outcome of a successful CRIU restore. Pid and
// ProcessID identify the restored application, not the CRIU command that
// restored it.
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
	Pid          int      // restored root task, from CRIU's --pidfile
	ProcessID    string   // process id it is registered under
	Warnings     []string // non-fatal findings from the restore checks
}

// TargetKind identifies how a CheckpointTarget names the process to act on.
type TargetKind string

const (
	TargetKindProcessID TargetKind = "process_id" // a process linkspan tracks
	TargetKindPID       TargetKind = "pid"        // any running process, by OS pid
)

// CheckpointTarget identifies the process a checkpoint operation acts on.
type CheckpointTarget struct {
	Kind      TargetKind
	ProcessID string
	PID       int
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
	Trigger    CheckpointTrigger // defaults to TriggerManual
}

// RestoreOptions configures a RestoreCheckpoint call.
type RestoreOptions struct {
	ShutdownOnCompletion bool     // shut linkspan down once the restored process exits
	PreRestoreCommands   []string // rebuild the environment; run in order, a failure aborts
	EnsureDirs           []string // created if missing, alongside the recorded working dir
	RequireFiles         []string // must exist before the restore proceeds
	Force                bool     // downgrade compatibility errors to warnings
}

// WorkloadState is a workload's position in the state machine
// CheckpointService uses to serialize operations. Distinct from
// CheckpointState, which is one checkpoint's own on-disk lifecycle.
type WorkloadState string

const (
	WorkloadRunning          WorkloadState = "running"
	WorkloadCheckpointing    WorkloadState = "checkpointing"
	WorkloadCheckpointed     WorkloadState = "checkpointed"
	WorkloadRestoring        WorkloadState = "restoring"
	WorkloadCheckpointFailed WorkloadState = "checkpoint_failed"
	WorkloadRestoreFailed    WorkloadState = "restore_failed"
)

// ErrWorkloadBusy is returned (wrapped) when a workload is not in a state
// that allows the requested operation.
var ErrWorkloadBusy = errors.New("workload is not in a state that allows this operation")
