package checkpoint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// slurmTimeLayout is how squeue and scontrol both render a job end time.
const slurmTimeLayout = "2006-01-02T15:04:05"

// slurmQueryTimeout bounds a squeue/scontrol call: the controller can be slow
// or unreachable, and a deadline lookup must never block the allocation.
const slurmQueryTimeout = 15 * time.Second

// SlurmJobID is the allocation linkspan is running inside, or "" when it is
// not running under Slurm at all.
func SlurmJobID() string {
	return os.Getenv("SLURM_JOB_ID")
}

// InSlurm reports whether this process is running inside a Slurm allocation.
func InSlurm() bool {
	return SlurmJobID() != ""
}

/*
DeadlineProvider reports when the current allocation is expected to end.

It is an interface so the walltime guard can be driven by a fixed time in
tests and by the real scheduler in production — and so a non-Slurm scheduler
can be added later without touching the guard.
*/
type DeadlineProvider interface {
	Deadline(ctx context.Context) (time.Time, error)
}

// DeadlineFunc adapts a plain function to DeadlineProvider.
type DeadlineFunc func(ctx context.Context) (time.Time, error)

func (f DeadlineFunc) Deadline(ctx context.Context) (time.Time, error) { return f(ctx) }

// SlurmDeadlineProvider asks Slurm for the job's expected end time, preferring
// squeue and falling back to scontrol when squeue is unavailable or silent.
type SlurmDeadlineProvider struct {
	JobID        string
	SqueuePath   string // defaults to "squeue" on PATH
	ScontrolPath string // defaults to "scontrol" on PATH
}

// NewSlurmDeadlineProvider builds a provider for the current allocation.
func NewSlurmDeadlineProvider() *SlurmDeadlineProvider {
	return &SlurmDeadlineProvider{JobID: SlurmJobID()}
}

func (p *SlurmDeadlineProvider) squeue() string {
	if p.SqueuePath != "" {
		return p.SqueuePath
	}
	return "squeue"
}

func (p *SlurmDeadlineProvider) scontrol() string {
	if p.ScontrolPath != "" {
		return p.ScontrolPath
	}
	return "scontrol"
}

/*
Deadline returns the allocation's expected end time.

Both tools are tried because they fail in different ways: squeue is the cheap
path but returns nothing once a job leaves the queue, while scontrol still
answers for a job the controller is finishing up.
*/
func (p *SlurmDeadlineProvider) Deadline(ctx context.Context) (time.Time, error) {
	if p.JobID == "" {
		return time.Time{}, fmt.Errorf("not running inside a Slurm allocation (SLURM_JOB_ID is unset)")
	}

	ctx, cancel := context.WithTimeout(ctx, slurmQueryTimeout)
	defer cancel()

	squeueOut, squeueErr := exec.CommandContext(ctx, p.squeue(), "-h", "-j", p.JobID, "-o", "%e").Output()
	if squeueErr == nil {
		if end, err := parseSlurmEndTime(string(squeueOut)); err == nil {
			return end, nil
		} else if !isUnsetSlurmTime(string(squeueOut)) {
			return time.Time{}, fmt.Errorf("squeue reported an end time for job %s that could not be parsed: %w", p.JobID, err)
		}
	}

	scontrolOut, scontrolErr := exec.CommandContext(ctx, p.scontrol(), "show", "job", p.JobID).Output()
	if scontrolErr != nil {
		return time.Time{}, fmt.Errorf("could not determine the end time of Slurm job %s (squeue: %v; scontrol: %v)", p.JobID, squeueErr, scontrolErr)
	}

	field, err := extractScontrolField(string(scontrolOut), "EndTime")
	if err != nil {
		return time.Time{}, fmt.Errorf("scontrol output for job %s has no EndTime: %w", p.JobID, err)
	}
	end, err := parseSlurmEndTime(field)
	if err != nil {
		return time.Time{}, fmt.Errorf("scontrol reported EndTime=%q for job %s: %w", strings.TrimSpace(field), p.JobID, err)
	}
	return end, nil
}

// isUnsetSlurmTime recognises the placeholders Slurm prints when a job has no
// finite end time, so they are reported as "unknown" rather than a parse bug.
func isUnsetSlurmTime(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "", "UNLIMITED", "N/A", "NONE", "UNKNOWN":
		return true
	}
	return false
}

// parseSlurmEndTime parses a Slurm timestamp in the local zone, which is what
// the controller renders it in.
func parseSlurmEndTime(s string) (time.Time, error) {
	trimmed := strings.TrimSpace(s)
	if isUnsetSlurmTime(trimmed) {
		return time.Time{}, fmt.Errorf("job has no finite end time (%q)", trimmed)
	}
	t, err := time.ParseInLocation(slurmTimeLayout, trimmed, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("unrecognised Slurm time %q: %w", trimmed, err)
	}
	return t, nil
}

// extractScontrolField pulls one Key=Value out of scontrol's space-separated
// output. Values never contain spaces for the time fields we read.
func extractScontrolField(out, key string) (string, error) {
	for _, field := range strings.Fields(out) {
		name, value, found := strings.Cut(field, "=")
		if found && name == key {
			return value, nil
		}
	}
	return "", fmt.Errorf("field %s not present", key)
}
