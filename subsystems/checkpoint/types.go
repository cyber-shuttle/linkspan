package checkpoint

import "time"

// CheckpointResult describes the outcome of a successful CRIU dump.
type CheckpointResult struct {
	ProcessID  string
	Pid        int
	ImagesDir  string // directory containing the CRIU checkpoint images
	LogFile    string // CRIU's own dump log, inside ImagesDir
	ExitCode   int
	Stdout     string
	Stderr     string
	StartedAt  time.Time
	FinishedAt time.Time
}

// RestoreResult describes the outcome of a successful CRIU restore.
type RestoreResult struct {
	ImagesDir  string
	LogFile    string
	ExitCode   int
	Stdout     string
	Stderr     string
	StartedAt  time.Time
	FinishedAt time.Time
	Pid        int // restored root task PID, captured via --pidfile
}