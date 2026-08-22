package checkpoint

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const manifestFileName = "manifest.json"
const completeFileName = "COMPLETE"

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

// findWorkloadForCheckpoint resolves which workload a globally-unique
// checkpoint id belongs to by searching root/*/<checkpointID>, so
// RestoreCheckpoint/GetCheckpoint can take a bare checkpoint id.
func findWorkloadForCheckpoint(root, checkpointID string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("checkpoint root is not configured")
	}
	if checkpointID == "" {
		return "", fmt.Errorf("checkpoint id is required")
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
		return "", fmt.Errorf("checkpoint %s not found under %s", checkpointID, root)
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
