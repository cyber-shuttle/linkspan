package checkpoint

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const ManifestSchema = "linkspan.checkpoint/v1"

type CheckpointState string

const (
	StateCreating CheckpointState = "creating"
	StateComplete CheckpointState = "complete"
	StateFailed   CheckpointState = "failed"
)

type CheckpointTrigger string

const (
	TriggerManual   CheckpointTrigger = "manual"
	TriggerWorkflow CheckpointTrigger = "workflow"
	TriggerWalltime CheckpointTrigger = "walltime"
	TriggerSignal   CheckpointTrigger = "signal"
)

// Manifest records enough provenance about a checkpoint to validate and
// restore it independently of the linkspan process that created it.
type Manifest struct {
	Schema          string            `json:"schema"`
	CheckpointID    string            `json:"checkpoint_id"`
	WorkloadID      string            `json:"workload_id"`
	CreatedAt       time.Time         `json:"created_at"`
	CompletedAt     time.Time         `json:"completed_at,omitempty"`
	Trigger         CheckpointTrigger `json:"trigger"`
	ProcessID       string            `json:"process_id"`
	OriginalPID     int               `json:"original_pid"`
	Command         string            `json:"command"`
	Args            []string          `json:"args"`
	WorkingDir      string            `json:"working_dir"`
	UID             int               `json:"uid"`
	GID             int               `json:"gid"`
	LinkspanVersion string            `json:"linkspan_version"`
	LinkspanCommit  string            `json:"linkspan_commit"`
	CRIUVersion     string            `json:"criu_version"`
	OS              string            `json:"os"`
	Kernel          string            `json:"kernel"`
	Arch            string            `json:"arch"`
	CPUInfo         string            `json:"cpu_info"`
	GPUMode         bool              `json:"gpu_mode"`
	GPUInfo         []string          `json:"gpu_info,omitempty"`
	SlurmJobID      string            `json:"slurm_job_id,omitempty"`
	SlurmNode       string            `json:"slurm_node,omitempty"`
	CRIUOptions     []string          `json:"criu_options"`
	ExitCode        int               `json:"exit_code"`
	State           CheckpointState   `json:"state"`
}

const manifestFileName = "manifest.json"
const completeFileName = "COMPLETE"

// Writes data to path via a temp file in the same directory followed by a rename, so readers never observe a partially-written file.
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

// Loads and parses the manifest for a checkpoint directory, so a checkpoint can be inspected independently of the linkspan process that created it.
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

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is effectively unheard of on any real system;
		// fall back to something still unique rather than panicking.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Mints a durable, sortable-by-humans workload identifier.
func NewWorkloadID() string {
	return fmt.Sprintf("wl-%s-%s", time.Now().UTC().Format("20060102T150405Z"), randomHex(4))
}

// Mints a durable, sortable-by-humans checkpoint identifier.
func NewCheckpointID() string {
	return fmt.Sprintf("ckpt-%s-%s", time.Now().UTC().Format("20060102T150405Z"), randomHex(4))
}

// Builds the initial manifest for a checkpoint about to be taken.
func (c *CriuCheckpointer) gatherManifest(ctx context.Context, internalProcessId string, cmd *exec.Cmd, pid int, checkpointID string, trigger CheckpointTrigger) *Manifest {
	m := &Manifest{
		Schema:          ManifestSchema,
		CheckpointID:    checkpointID,
		WorkloadID:      c.WorkloadID,
		CreatedAt:       time.Now().UTC(),
		Trigger:         trigger,
		ProcessID:       internalProcessId,
		OriginalPID:     pid,
		WorkingDir:      cmd.Dir,
		LinkspanVersion: c.LinkspanVersion,
		LinkspanCommit:  c.LinkspanCommit,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		GPUMode:         c.SupportGpuCheckpoint,
		State:           StateCreating,
	}

	if cmd.Path != "" {
		m.Command = cmd.Path
	}
	if len(cmd.Args) > 0 {
		m.Args = cmd.Args
	}

	if link, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		m.WorkingDir = link
	}

	if uid, err := processOwnerUID(pid); err == nil {
		m.UID = uid
	} else {
		log.Printf("[Checkpoint] warning: could not determine owning uid for pid %d: %v", pid, err)
	}
	if gid, err := processOwnerGID(pid); err == nil {
		m.GID = gid
	} else {
		log.Printf("[Checkpoint] warning: could not determine owning gid for pid %d: %v", pid, err)
	}

	if v, err := criuVersion(ctx, c.CriuPath); err == nil {
		m.CRIUVersion = v
	} else {
		log.Printf("[Checkpoint] warning: could not determine CRIU version: %v", err)
	}

	if kernel, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		m.Kernel = strings.TrimSpace(string(kernel))
	}

	if cpu, err := cpuInfo(); err == nil {
		m.CPUInfo = cpu
	}

	if c.SupportGpuCheckpoint {
		m.GPUInfo = gpuInfo(ctx)
	}

	m.SlurmJobID = os.Getenv("SLURM_JOB_ID")
	m.SlurmNode = os.Getenv("SLURMD_NODENAME")

	return m
}

func criuVersion(ctx context.Context, criuPath string) (string, error) {
	if criuPath == "" {
		return "", fmt.Errorf("CRIU path is not configured")
	}
	out, err := exec.CommandContext(ctx, criuPath, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func cpuInfo() (string, error) {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", err
	}
	model := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				model = strings.TrimSpace(parts[1])
			}
			break
		}
	}
	if model == "" {
		return "", fmt.Errorf("model name not found in /proc/cpuinfo")
	}
	return fmt.Sprintf("%s (%d logical cores)", model, runtime.NumCPU()), nil
}

func gpuInfo(ctx context.Context) []string {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, path, "-L").Output()
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
