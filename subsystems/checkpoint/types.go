package checkpoint

import "time"

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
