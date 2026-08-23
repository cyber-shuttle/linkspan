package checkpoint

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ManifestSchema identifies the shape of Manifest for forward compatibility.
const ManifestSchema = "linkspan.checkpoint/v1"

// CheckpointState is one checkpoint's own on-disk lifecycle. Not to be
// confused with WorkloadState (types.go), which tracks a workload's
// in-memory position across possibly many checkpoints.
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
	Executable      string            `json:"executable,omitempty"`
	WorkingDir      string            `json:"working_dir"`
	OpenFiles       []string          `json:"open_files,omitempty"`
	Mounts          []MountPoint      `json:"mounts,omitempty"`
	Environment     map[string]string `json:"environment,omitempty"`
	Modules         []string          `json:"modules,omitempty"`
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

// MountPoint is a filesystem the process depended on, recorded so a
// restoring allocation can verify it was reconstructed first.
type MountPoint struct {
	Source string `json:"source"`
	Target string `json:"target"`
	FSType string `json:"fstype"`
}

// manifestParams carries what gatherManifest needs from the checkpointer
// and the call site, without coupling manifest.go to criuCheckpointer.
type manifestParams struct {
	CriuPath        string
	WorkloadID      string
	LinkspanVersion string
	LinkspanCommit  string
	GPUMode         bool
	ProcessID       string
	PID             int
	CheckpointID    string
	Trigger         CheckpointTrigger
}

// gatherManifest builds the initial manifest for a checkpoint about to be
// taken. Every field here is best-effort provenance, not correctness-critical
// Process info is read from /proc/<pid> rather than an *exec.Cmd, so this works
// identically whether the target was spawned by linkspan or not.
func gatherManifest(ctx context.Context, p manifestParams) *Manifest {
	m := &Manifest{
		Schema:          ManifestSchema,
		CheckpointID:    p.CheckpointID,
		WorkloadID:      p.WorkloadID,
		CreatedAt:       time.Now().UTC(),
		Trigger:         p.Trigger,
		ProcessID:       p.ProcessID,
		OriginalPID:     p.PID,
		LinkspanVersion: p.LinkspanVersion,
		LinkspanCommit:  p.LinkspanCommit,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		GPUMode:         p.GPUMode,
		State:           StateCreating,
	}

	if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", p.PID)); err == nil {
		parts := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if len(parts) > 0 && parts[0] != "" {
			m.Command = parts[0]
			m.Args = parts
		}
	}

	if link, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", p.PID)); err == nil {
		m.WorkingDir = link
	}

	if link, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", p.PID)); err == nil {
		m.Executable = link
	}

	m.OpenFiles = openFiles(p.PID)
	m.Environment = capturedEnvironment(p.PID)
	m.Modules = loadedModules(m.Environment)
	m.Mounts = dependencyMounts(p.PID, append([]string{m.Executable, m.WorkingDir}, m.OpenFiles...))

	if uid, err := processOwnerUID(p.PID); err == nil {
		m.UID = uid
	} else {
		log.Printf("[Checkpoint] warning: could not determine owning uid for pid %d: %v", p.PID, err)
	}
	if gid, err := processOwnerGID(p.PID); err == nil {
		m.GID = gid
	} else {
		log.Printf("[Checkpoint] warning: could not determine owning gid for pid %d: %v", p.PID, err)
	}

	if v, err := criuVersion(ctx, p.CriuPath); err == nil {
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

	if p.GPUMode {
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
