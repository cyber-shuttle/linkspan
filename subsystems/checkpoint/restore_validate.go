package checkpoint

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// RestoreValidation is the outcome of checking a checkpoint against the host
// about to restore it. Errors block the restore; warnings usually still work
// but explain most restores that fail inside CRIU anyway.
type RestoreValidation struct {
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

func (v *RestoreValidation) errorf(format string, args ...any) {
	v.Errors = append(v.Errors, fmt.Sprintf(format, args...))
}

func (v *RestoreValidation) warnf(format string, args ...any) {
	v.Warnings = append(v.Warnings, fmt.Sprintf(format, args...))
}

func (v *RestoreValidation) OK() bool { return len(v.Errors) == 0 }

func (v *RestoreValidation) merge(other *RestoreValidation) {
	v.Errors = append(v.Errors, other.Errors...)
	v.Warnings = append(v.Warnings, other.Warnings...)
}

func (v *RestoreValidation) Err() error {
	if v.OK() {
		return nil
	}
	return fmt.Errorf("restore compatibility check failed:\n  - %s", strings.Join(v.Errors, "\n  - "))
}

// downgrade turns errors into warnings, for --restore-force.
func (v *RestoreValidation) downgrade() {
	v.Warnings = append(v.Warnings, v.Errors...)
	v.Errors = nil
}

func (v *RestoreValidation) log(phase string) {
	for _, w := range v.Warnings {
		log.Printf("[Checkpoint] %s warning: %s", phase, w)
	}
}

// validateCheckpoint checks the checkpoint itself, before any environment is
// reconstructed for it.
func validateCheckpoint(checkpointDir string, m *Manifest) *RestoreValidation {
	v := &RestoreValidation{}
	validateCheckpointStorage(checkpointDir, m, v)
	return v
}

// validateHostCompatibility checks the checkpoint against this host. Runs
// after prepareRestoreEnvironment, so reconstructed mounts and directories
// are in place before the files they hold are checked.
func (c *criuCheckpointer) validateHostCompatibility(ctx context.Context, m *Manifest) *RestoreValidation {
	v := &RestoreValidation{}
	if m == nil {
		return v
	}
	validateOwnership(m, c.AllowedCheckpointUsers, v)
	validateFiles(m, v)
	validatePlatform(m, v)
	c.validateCriuHost(ctx, m, v)
	validateGPU(ctx, m, v)
	return v
}

// validateCheckpointStorage confirms the checkpoint is complete and its
// directory readable and writable — CRIU writes its restore log and pidfile
// back into the work dir.
func validateCheckpointStorage(checkpointDir string, m *Manifest, v *RestoreValidation) {
	if m == nil {
		v.errorf("checkpoint manifest at %s is missing or unreadable", checkpointDir)
		return
	}
	if m.State != StateComplete {
		v.errorf("checkpoint %s is in state %q, not %q", m.CheckpointID, m.State, StateComplete)
	}
	if _, err := os.Stat(filepath.Join(checkpointDir, completeFileName)); err != nil {
		v.errorf("checkpoint %s is missing its %s marker: %v", m.CheckpointID, completeFileName, err)
	}

	imagesDir := imagesDirPath(checkpointDir)
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		v.errorf("checkpoint images at %s are not accessible: %v", imagesDir, err)
		return
	}
	if len(entries) == 0 {
		v.errorf("checkpoint images directory %s is empty", imagesDir)
	}
	if err := checkDirWritable(checkpointDir); err != nil {
		v.errorf("checkpoint directory %s is not writable, but CRIU must write its restore log and pidfile there: %v", checkpointDir, err)
	}
}

// validateOwnership checks we can own the restored process. CRIU restores
// the original uid/gid, so a mismatch fails unless we are root or the
// checkpointed user is explicitly allowed.
func validateOwnership(m *Manifest, allowedUsers []string, v *RestoreValidation) {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		return
	}
	if m.UID != uid && !userAllowed(m.UID, allowedUsers) {
		v.errorf("checkpoint was taken as uid %d but linkspan runs as uid %d; restore as the original user, run as root, or add the uid to --allowed-checkpoint-users", m.UID, uid)
	}
	if m.GID != gid && m.GID != 0 {
		v.warnf("checkpoint was taken with gid %d but linkspan runs with gid %d; files restored with the original gid may be inaccessible", m.GID, gid)
	}
}

// userAllowed applies the --allowed-checkpoint-users policy to a uid.
func userAllowed(uid int, allowed []string) bool {
	return checkAllowedUserID(uid, allowed) == nil
}

// validateFiles confirms the executable and working directory still exist
// here. Open files only warn: they legitimately disappear, and CRIU restores
// their contents from the images.
func validateFiles(m *Manifest, v *RestoreValidation) {
	if m.Executable != "" {
		if _, err := os.Stat(m.Executable); err != nil {
			v.errorf("executable %s is not present on this host: %v", m.Executable, err)
		}
	}
	if m.WorkingDir != "" {
		if info, err := os.Stat(m.WorkingDir); err != nil {
			v.errorf("working directory %s is not present on this host: %v", m.WorkingDir, err)
		} else if !info.IsDir() {
			v.errorf("working directory %s exists but is not a directory", m.WorkingDir)
		}
	}
	for _, f := range m.OpenFiles {
		if _, err := os.Stat(f); err != nil {
			v.warnf("file %s that was open at checkpoint time is missing on this host", f)
		}
	}
}

func validatePlatform(m *Manifest, v *RestoreValidation) {
	if m.Arch != "" && m.Arch != runtime.GOARCH {
		v.errorf("checkpoint was taken on %s but this host is %s; CRIU images are not portable across architectures", m.Arch, runtime.GOARCH)
	}
	if m.OS != "" && m.OS != runtime.GOOS {
		v.errorf("checkpoint was taken on %s but this host is %s", m.OS, runtime.GOOS)
	}
	// A different CPU can still restore, but the process may execute
	// instructions the new one lacks; CRIU's --cpu-cap governs this.
	if current, err := cpuInfo(); err == nil && m.CPUInfo != "" && m.CPUInfo != current {
		v.warnf("CPU differs from checkpoint time (%q → %q); if the restore traps on an illegal instruction, add --cpu-cap to --additional-criu-opts", m.CPUInfo, current)
	}
}

// validateCriuHost runs CRIU's own feature check, then compares CRIU and
// kernel versions against the ones that produced the checkpoint.
func (c *criuCheckpointer) validateCriuHost(ctx context.Context, m *Manifest, v *RestoreValidation) {
	if err := c.CRIUCheck(ctx); err != nil {
		v.errorf("%v", err)
		return
	}
	current, err := criuVersion(ctx, c.CriuPath)
	if err != nil {
		v.warnf("could not determine this host's CRIU version: %v", err)
		return
	}
	if m.CRIUVersion != "" && m.CRIUVersion != current {
		v.warnf("checkpoint was taken with %q but this host has %q; images are not guaranteed compatible across CRIU versions", m.CRIUVersion, current)
	}
	if kernel, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		if k := strings.TrimSpace(string(kernel)); m.Kernel != "" && m.Kernel != k {
			v.warnf("kernel differs from checkpoint time (%s → %s)", m.Kernel, k)
		}
	}
}

// validateGPU applies only to GPU-mode checkpoints; a CPU-only one restores
// fine with or without GPUs present.
func validateGPU(ctx context.Context, m *Manifest, v *RestoreValidation) {
	if !m.GPUMode {
		return
	}
	if err := checkGpuPrerequisites(ctx); err != nil {
		v.errorf("checkpoint was taken in GPU mode but this host fails GPU prerequisites: %v", err)
		return
	}
	current := gpuInfo(ctx)
	if len(m.GPUInfo) == 0 {
		return
	}
	if len(current) < len(m.GPUInfo) {
		v.errorf("checkpoint used %d GPU(s) but this host exposes %d", len(m.GPUInfo), len(current))
		return
	}
	if gpuModel(m.GPUInfo[0]) != gpuModel(current[0]) {
		v.errorf("checkpoint was taken on %q but this host has %q; CUDA state cannot be restored onto a different GPU model", gpuModel(m.GPUInfo[0]), gpuModel(current[0]))
	}
}

// gpuModel strips the index and UUID from an `nvidia-smi -L` line so two
// hosts can be compared.
func gpuModel(line string) string {
	_, rest, found := strings.Cut(line, ": ")
	if !found {
		rest = line
	}
	if idx := strings.Index(rest, " (UUID:"); idx >= 0 {
		rest = rest[:idx]
	}
	return strings.TrimSpace(rest)
}
