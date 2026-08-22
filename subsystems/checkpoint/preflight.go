package checkpoint

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

// Linux capability bit numbers (stable ABI values from linux/capability.h).
const (
	capSysPtrace         = 19
	capCheckpointRestore = 40
)

/*
CRIUCheck validates that CRIU is actually usable on this host, beyond just
the binary existing: it runs CRIU's own "check" subcommand, verifies the
checkpoint root is writable, checks for the Linux capabilities CRIU needs
in unprivileged mode, and (when GPU checkpointing is requested) validates
GPU prerequisites separately.
*/
func (c *CriuCheckpointer) CRIUCheck(ctx context.Context) error {
	if err := c.checkBinary(); err != nil {
		return err
	}
	if c.CheckpointRoot == "" {
		return fmt.Errorf("CheckpointRoot is not configured; set --checkpoint-root to a durable, shared storage path (e.g. Lustre, GPFS, NFS, or project scratch)")
	}
	if strings.HasPrefix(c.CheckpointRoot, "/tmp") {
		log.Printf("[Checkpoint] warning: checkpoint root %s is under /tmp, which typically does not persist across compute-node allocations; use shared storage for production", c.CheckpointRoot)
	}
	if err := checkCapabilities(); err != nil {
		return err
	}
	if err := checkDirWritable(c.CheckpointRoot); err != nil {
		return err
	}
	if err := c.runCriuCheck(ctx); err != nil {
		return err
	}
	if c.SupportGpuCheckpoint {
		if err := checkGpuPrerequisites(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (c *CriuCheckpointer) checkBinary() error {
	if c.CriuPath == "" {
		return fmt.Errorf("CRIU path is not configured")
	}
	info, err := os.Stat(c.CriuPath)
	if err != nil {
		return fmt.Errorf("CRIU binary not found at %s: %w", c.CriuPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("CRIU path %s is a directory, not an executable", c.CriuPath)
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("CRIU binary at %s is not executable", c.CriuPath)
	}
	return nil
}

func (c *CriuCheckpointer) runCriuCheck(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.CriuPath, "check", "--unprivileged")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("criu check failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func checkDirWritable(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create checkpoint root %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".linkspan-writable-check-*")
	if err != nil {
		return fmt.Errorf("checkpoint root %s is not writable: %w", dir, err)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}

func checkCapabilities() error {
	if os.Geteuid() == 0 {
		return nil
	}

	ptrace, err := hasEffectiveCapability(capSysPtrace)
	if err != nil {
		log.Printf("[Checkpoint] warning: unable to inspect process capabilities: %v", err)
		return nil
	}
	checkpointRestore, _ := hasEffectiveCapability(capCheckpointRestore)

	if !ptrace && !checkpointRestore {
		return fmt.Errorf("process has neither CAP_SYS_PTRACE nor CAP_CHECKPOINT_RESTORE and is not running as root; CRIU unprivileged mode requires one of these")
	}
	return nil
}

func hasEffectiveCapability(bit uint) (bool, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		hexStr := strings.TrimSpace(strings.TrimPrefix(line, "CapEff:"))
		mask, err := strconv.ParseUint(hexStr, 16, 64)
		if err != nil {
			return false, fmt.Errorf("unexpected CapEff format %q: %w", hexStr, err)
		}
		return mask&(1<<bit) != 0, nil
	}
	return false, fmt.Errorf("CapEff not found in /proc/self/status")
}

func checkGpuPrerequisites(ctx context.Context) error {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return fmt.Errorf("GPU checkpoint requested but nvidia-smi was not found on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, "-L")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("GPU checkpoint requested but nvidia-smi failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func checkPidExists(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid PID %d", pid)
	}
	// ESRCH means the process is gone; EPERM (or nil) means it exists but we may not have permission to signal it, which is a separate concern handled by checkAllowedUser.
	if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
		return fmt.Errorf("target process %d does not exist: %w", pid, err)
	}
	return nil
}

func checkAllowedUser(pid int, allowed []string) error {
	ownerUID, err := processOwnerUID(pid)
	if err != nil {
		return fmt.Errorf("failed to determine owner of process %d: %w", pid, err)
	}

	if len(allowed) == 0 {
		if ownerUID != os.Getuid() {
			return fmt.Errorf("process %d is owned by uid %d, not linkspan's own uid %d; set AllowedCheckpointUsers to permit cross-user checkpointing", pid, ownerUID, os.Getuid())
		}
		return nil
	}

	for _, want := range allowed {
		if want == "*" {
			return nil
		}
		if u, err := user.Lookup(want); err == nil {
			if uid, convErr := strconv.Atoi(u.Uid); convErr == nil && uid == ownerUID {
				return nil
			}
			continue
		}
		if uid, err := strconv.Atoi(want); err == nil && uid == ownerUID {
			return nil
		}
	}
	return fmt.Errorf("process %d owner (uid %d) is not in AllowedCheckpointUsers", pid, ownerUID)
}

func processOwnerUID(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return -1, fmt.Errorf("unexpected Uid line format: %q", line)
		}
		return strconv.Atoi(fields[1])
	}
	return -1, fmt.Errorf("Uid not found in /proc/%d/status", pid)
}

func processOwnerGID(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Gid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return -1, fmt.Errorf("unexpected Gid line format: %q", line)
		}
		return strconv.Atoi(fields[1])
	}
	return -1, fmt.Errorf("Gid not found in /proc/%d/status", pid)
}
