package checkpoint

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const manifestFileName = "manifest.json"
const completeFileName = "COMPLETE"
const restoreRecordFileName = "last_restore.json"

// RestoreRecord is written next to a checkpoint after it is restored, so
// which allocation last brought a workload back is answerable from shared
// storage alone.
type RestoreRecord struct {
	CheckpointID    string    `json:"checkpoint_id"`
	WorkloadID      string    `json:"workload_id"`
	ProcessID       string    `json:"process_id"`
	Pid             int       `json:"pid"`
	RestoredAt      time.Time `json:"restored_at"`
	Hostname        string    `json:"hostname,omitempty"`
	SlurmJobID      string    `json:"slurm_job_id,omitempty"`
	SlurmNode       string    `json:"slurm_node,omitempty"`
	LinkspanVersion string    `json:"linkspan_version,omitempty"`
	Warnings        []string  `json:"warnings,omitempty"`
}

func newRestoreRecord(r *RestoreResult, linkspanVersion string) *RestoreRecord {
	hostname, _ := os.Hostname()
	return &RestoreRecord{
		CheckpointID:    r.CheckpointID,
		WorkloadID:      r.WorkloadID,
		ProcessID:       r.ProcessID,
		Pid:             r.Pid,
		RestoredAt:      r.FinishedAt.UTC(),
		Hostname:        hostname,
		SlurmJobID:      os.Getenv("SLURM_JOB_ID"),
		SlurmNode:       os.Getenv("SLURMD_NODENAME"),
		LinkspanVersion: linkspanVersion,
		Warnings:        r.Warnings,
	}
}

func writeRestoreRecord(checkpointDir string, r *RestoreRecord) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal restore record: %w", err)
	}
	return atomicWriteFile(filepath.Join(checkpointDir, restoreRecordFileName), data, 0644)
}

// ReadRestoreRecord loads a checkpoint's most recent restore.
func ReadRestoreRecord(checkpointDir string) (*RestoreRecord, error) {
	data, err := os.ReadFile(filepath.Join(checkpointDir, restoreRecordFileName))
	if err != nil {
		return nil, err
	}
	var r RestoreRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("corrupt restore record at %s: %w", checkpointDir, err)
	}
	return &r, nil
}

func checkpointDirPath(root, workloadID, checkpointID string) string {
	return filepath.Join(root, workloadID, checkpointID)
}

func imagesDirPath(checkpointDir string) string {
	return filepath.Join(checkpointDir, "images")
}

// atomicWriteFile writes data to path via a temp file in the same directory
// followed by a rename, so readers never observe a partially-written file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func writeManifest(checkpointDir string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}
	return atomicWriteFile(filepath.Join(checkpointDir, manifestFileName), data, 0644)
}

// ReadManifest loads and parses the manifest for a checkpoint directory, so
// a checkpoint can be inspected independently of the linkspan process that
// created it.
func ReadManifest(checkpointDir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(checkpointDir, manifestFileName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("corrupt manifest at %s: %w", checkpointDir, err)
	}
	return &m, nil
}

// isCheckpointComplete reports whether checkpointDir holds a checkpoint that
// finished successfully: both the COMPLETE marker and a manifest with
// state "complete" must be present.
func isCheckpointComplete(checkpointDir string) bool {
	if _, err := os.Stat(filepath.Join(checkpointDir, completeFileName)); err != nil {
		return false
	}
	m, err := ReadManifest(checkpointDir)
	if err != nil {
		return false
	}
	return m.State == StateComplete
}

// ErrCheckpointNotFound separates an unknown checkpoint id from a failure to
// search for one, so the REST layer can answer 404 rather than 500.
var ErrCheckpointNotFound = errors.New("checkpoint not found")

// checkpointIDPattern is deliberately strict: an id becomes both a path
// element and a glob pattern below, and ids now arrive from HTTP clients.
var checkpointIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateCheckpointID rejects ids that would escape the checkpoint root or
// widen the glob that resolves them.
func ValidateCheckpointID(id string) error {
	if id == "" {
		return fmt.Errorf("checkpoint id is required")
	}
	if id == "." || id == ".." || strings.Contains(id, "..") {
		return fmt.Errorf("invalid checkpoint id %q", id)
	}
	if !checkpointIDPattern.MatchString(id) {
		return fmt.Errorf("invalid checkpoint id %q: only letters, digits, '.', '_' and '-' are allowed", id)
	}
	return nil
}

// findWorkloadForCheckpoint resolves which workload a globally-unique
// checkpoint id belongs to by searching root/*/<checkpointID>, so
// RestoreCheckpoint/GetCheckpoint can take a bare checkpoint id.
func findWorkloadForCheckpoint(root, checkpointID string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("checkpoint root is not configured")
	}
	if err := ValidateCheckpointID(checkpointID); err != nil {
		return "", err
	}

	matches, err := filepath.Glob(filepath.Join(root, "*", checkpointID))
	if err != nil {
		return "", fmt.Errorf("failed to search for checkpoint %s: %w", checkpointID, err)
	}

	var found []string
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			found = append(found, m)
		}
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w: %s under %s", ErrCheckpointNotFound, checkpointID, root)
	case 1:
		return filepath.Base(filepath.Dir(found[0])), nil
	default:
		return "", fmt.Errorf("checkpoint id %s is ambiguous: found under multiple workloads %v", checkpointID, found)
	}
}

// listManifests returns every checkpoint's manifest found under root,
// across all workloads, in any state. Corrupt or unreadable manifests are
// logged and skipped rather than failing the whole listing.
func listManifests(root string) ([]*Manifest, error) {
	if root == "" {
		return nil, fmt.Errorf("checkpoint root is not configured")
	}

	matches, err := filepath.Glob(filepath.Join(root, "*", "*", manifestFileName))
	if err != nil {
		return nil, fmt.Errorf("failed to list checkpoints under %s: %w", root, err)
	}

	var manifests []*Manifest
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[Checkpoint] warning: failed to read manifest %s: %v", path, err)
			continue
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			log.Printf("[Checkpoint] warning: corrupt manifest %s: %v", path, err)
			continue
		}
		manifests = append(manifests, &m)
	}
	return manifests, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is effectively unheard of on any real system;
		// fall back to something still unique rather than panicking.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// NewWorkloadID mints a durable, sortable-by-humans workload identifier.
func NewWorkloadID() string {
	return fmt.Sprintf("wl-%s-%s", time.Now().UTC().Format("20060102T150405Z"), randomHex(4))
}

// NewCheckpointID mints a durable, sortable-by-humans checkpoint identifier.
func NewCheckpointID() string {
	return fmt.Sprintf("ckpt-%s-%s", time.Now().UTC().Format("20060102T150405Z"), randomHex(4))
}
