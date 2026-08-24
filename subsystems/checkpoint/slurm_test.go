package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestParseSlurmEndTime(t *testing.T) {
	got, err := parseSlurmEndTime("2026-08-24T14:30:00")
	if err != nil {
		t.Fatalf("failed to parse a normal Slurm end time: %v", err)
	}
	want := time.Date(2026, 8, 24, 14, 30, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("expected %s, got %s", want, got)
	}

	// squeue pads its output, and a trailing newline always comes with -h.
	padded, err := parseSlurmEndTime("  2026-08-24T14:30:00 \n")
	if err != nil || !padded.Equal(want) {
		t.Fatalf("expected surrounding whitespace to be tolerated, got %s (%v)", padded, err)
	}
}

// A job with no finite end time must be reported as unknown, not as a parse
// bug, so the guard can fall back to scheduler signals.
func TestParseSlurmEndTimeRejectsPlaceholders(t *testing.T) {
	for _, placeholder := range []string{"", "UNLIMITED", "N/A", "NONE", "Unknown", "  n/a  "} {
		if _, err := parseSlurmEndTime(placeholder); err == nil {
			t.Fatalf("expected %q to be rejected as a time", placeholder)
		}
		if !isUnsetSlurmTime(placeholder) {
			t.Fatalf("expected %q to be recognised as an unset Slurm time", placeholder)
		}
	}
	if isUnsetSlurmTime("2026-08-24T14:30:00") {
		t.Fatalf("a real timestamp must not be treated as unset")
	}
}

func TestExtractScontrolField(t *testing.T) {
	// Trimmed from real `scontrol show job` output.
	out := `JobId=41 JobName=ckpt-stage5
   UserId=exouser(1001) GroupId=exouser(1001) MCS_label=N/A
   RunTime=00:00:12 TimeLimit=00:15:00 TimeMin=N/A
   SubmitTime=2026-08-23T05:49:00 EligibleTime=2026-08-23T05:49:00
   StartTime=2026-08-23T05:49:30 EndTime=2026-08-23T06:04:30 Deadline=N/A`

	got, err := extractScontrolField(out, "EndTime")
	if err != nil {
		t.Fatalf("failed to extract EndTime: %v", err)
	}
	if got != "2026-08-23T06:04:30" {
		t.Fatalf("expected the EndTime value, got %q", got)
	}

	// StartTime shares a suffix with EndTime; the match must be on the whole key.
	start, err := extractScontrolField(out, "StartTime")
	if err != nil || start != "2026-08-23T05:49:30" {
		t.Fatalf("expected StartTime to be matched exactly, got %q (%v)", start, err)
	}
	if _, err := extractScontrolField(out, "NotAField"); err == nil {
		t.Fatalf("expected a missing field to be an error")
	}
}

func TestSlurmDetectionFollowsEnvironment(t *testing.T) {
	t.Setenv("SLURM_JOB_ID", "")
	if InSlurm() || SlurmJobID() != "" {
		t.Fatalf("expected no Slurm allocation when SLURM_JOB_ID is empty")
	}

	t.Setenv("SLURM_JOB_ID", "12345")
	if !InSlurm() || SlurmJobID() != "12345" {
		t.Fatalf("expected the allocation id to be read from the environment")
	}
}

func TestSlurmDeadlineRequiresAJobID(t *testing.T) {
	p := &SlurmDeadlineProvider{JobID: ""}
	if _, err := p.Deadline(context.Background()); err == nil {
		t.Fatalf("expected an error outside a Slurm allocation")
	}
}

// Both tools failing must surface as one error naming both, not a panic or a
// zero time the guard would treat as a deadline in 1970.
func TestSlurmDeadlineReportsBothToolFailures(t *testing.T) {
	p := &SlurmDeadlineProvider{
		JobID:        "12345",
		SqueuePath:   "/nonexistent/squeue",
		ScontrolPath: "/nonexistent/scontrol",
	}
	deadline, err := p.Deadline(context.Background())
	if err == nil {
		t.Fatalf("expected an error when neither squeue nor scontrol can run")
	}
	if !deadline.IsZero() {
		t.Fatalf("expected the zero time alongside the error, got %s", deadline)
	}
}

/*
The real thing: inside a Slurm allocation, ask the live controller for this
job's end time. Skipped outside Slurm, and skipped if squeue is missing, in
keeping with how the CRIU tests gate on a real CRIU.
*/
func TestSlurmDeadlineFromLiveAllocation(t *testing.T) {
	jobID := os.Getenv("SLURM_JOB_ID")
	if jobID == "" {
		t.Skip("not running inside a Slurm allocation")
	}
	if _, err := exec.LookPath("squeue"); err != nil {
		t.Skip("squeue is not available on this host")
	}

	p := NewSlurmDeadlineProvider()
	if p.JobID != jobID {
		t.Fatalf("expected the provider to pick up job %s, got %s", jobID, p.JobID)
	}

	end, err := p.Deadline(context.Background())
	if err != nil {
		t.Fatalf("failed to read the end time of live job %s: %v", jobID, err)
	}
	if !end.After(time.Now()) {
		t.Fatalf("expected job %s to end in the future, got %s", jobID, end)
	}
	t.Logf("live Slurm job %s ends at %s (%s from now)", jobID, end.Format(time.RFC3339), time.Until(end).Truncate(time.Second))
}
