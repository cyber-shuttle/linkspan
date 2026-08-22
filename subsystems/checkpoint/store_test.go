package checkpoint

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		Schema:       ManifestSchema,
		CheckpointID: "ckpt-test",
		WorkloadID:   "wl-test",
		CreatedAt:    time.Now().UTC().Truncate(time.Second),
		Trigger:      TriggerManual,
		ProcessID:    "p-123",
		OriginalPID:  4242,
		Command:      "/bin/sleep",
		Args:         []string{"sleep", "100"},
		CRIUOptions:  []string{"dump", "-t", "4242"},
		State:        StateCreating,
	}

	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest failed: %v", err)
	}

	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest failed: %v", err)
	}
	if got.CheckpointID != m.CheckpointID || got.WorkloadID != m.WorkloadID || got.State != m.State {
		t.Fatalf("round-tripped manifest mismatch: got %+v, want %+v", got, m)
	}
	if !got.CreatedAt.Equal(m.CreatedAt) {
		t.Fatalf("CreatedAt mismatch: got %v, want %v", got.CreatedAt, m.CreatedAt)
	}
	if len(got.Args) != len(m.Args) || got.Args[0] != m.Args[0] {
		t.Fatalf("Args mismatch: got %v, want %v", got.Args, m.Args)
	}
}

func TestIsCheckpointComplete(t *testing.T) {
	dir := t.TempDir()

	if isCheckpointComplete(dir) {
		t.Fatalf("expected incomplete: no manifest, no marker")
	}

	m := &Manifest{State: StateCreating}
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest failed: %v", err)
	}
	if isCheckpointComplete(dir) {
		t.Fatalf("expected incomplete: manifest present with state=creating, no COMPLETE marker")
	}

	if err := os.WriteFile(dir+"/"+completeFileName, []byte{}, 0644); err != nil {
		t.Fatalf("failed to write COMPLETE marker: %v", err)
	}
	if isCheckpointComplete(dir) {
		t.Fatalf("expected incomplete: COMPLETE marker present but manifest state is still creating")
	}

	m.State = StateComplete
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest failed: %v", err)
	}
	if !isCheckpointComplete(dir) {
		t.Fatalf("expected complete: COMPLETE marker present and manifest state is complete")
	}
}

func TestFindWorkloadForCheckpoint(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(checkpointDirPath(root, "wl-a", "ckpt-1"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := findWorkloadForCheckpoint(root, "ckpt-1")
	if err != nil {
		t.Fatalf("findWorkloadForCheckpoint failed: %v", err)
	}
	if got != "wl-a" {
		t.Fatalf("expected workload wl-a, got %s", got)
	}

	if _, err := findWorkloadForCheckpoint(root, "does-not-exist"); err == nil {
		t.Fatalf("expected an error for an unknown checkpoint id")
	}
}

func TestListManifests(t *testing.T) {
	root := t.TempDir()
	for _, cp := range []struct{ workload, checkpoint string }{
		{"wl-a", "ckpt-1"},
		{"wl-a", "ckpt-2"},
		{"wl-b", "ckpt-3"},
	} {
		dir := checkpointDirPath(root, cp.workload, cp.checkpoint)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		m := &Manifest{WorkloadID: cp.workload, CheckpointID: cp.checkpoint, State: StateComplete}
		if err := writeManifest(dir, m); err != nil {
			t.Fatalf("writeManifest: %v", err)
		}
	}

	manifests, err := listManifests(root)
	if err != nil {
		t.Fatalf("listManifests failed: %v", err)
	}
	if len(manifests) != 3 {
		t.Fatalf("expected 3 manifests, got %d", len(manifests))
	}
}

func TestNewIDsAreUniqueAndPrefixed(t *testing.T) {
	w1, w2 := NewWorkloadID(), NewWorkloadID()
	if w1 == w2 {
		t.Fatalf("expected distinct workload ids, got %q twice", w1)
	}
	if !strings.HasPrefix(w1, "wl-") || !strings.HasPrefix(w2, "wl-") {
		t.Fatalf("expected workload ids to start with \"wl-\", got %q and %q", w1, w2)
	}

	c1, c2 := NewCheckpointID(), NewCheckpointID()
	if c1 == c2 {
		t.Fatalf("expected distinct checkpoint ids, got %q twice", c1)
	}
	if !strings.HasPrefix(c1, "ckpt-") || !strings.HasPrefix(c2, "ckpt-") {
		t.Fatalf("expected checkpoint ids to start with \"ckpt-\", got %q and %q", c1, c2)
	}
}
