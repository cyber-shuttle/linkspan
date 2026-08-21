package checkpoint

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

type CriuCheckpointer struct {
	CriuPath               string
	SupportGpuCheckpoint   bool
	AdditionalCriuOpts     []string
	DumpDirRoot            string
	AllowedCheckpointUsers []string
}

func buildDumpArgs(pid int, imagesDir, logFile string, extra []string) []string {
	args := []string{
		"dump",
		"-t", strconv.Itoa(pid),
		"--shell-job",
		"--tcp-established",
		"--unprivileged",
		"--images-dir", imagesDir,
		"--log-file", logFile,
	}
	return append(args, extra...)
}

func buildRestoreArgs(imagesDir, logFile, pidFile string, extra []string) []string {
	args := []string{
		"restore",
		"--shell-job",
		"--tcp-established",
		"--unprivileged",
		"--restore-detached",
		"--images-dir", imagesDir,
		"--log-file", logFile,
		"--pidfile", pidFile,
	}
	return append(args, extra...)
}

func exitCodeOf(cmd *exec.Cmd, runErr error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if runErr != nil {
		return -1
	}
	return 0
}

func readPidFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

/*
Checkpoint checkpoints a process managed by linkspan's ProcessManager,
identified by its internal ID. It runs CRIU directly (no shell), waits for
the dump to finish, and only returns success once CRIU has actually
completed the checkpoint.

NOTE: Linkspan should be supported to checkpoint processes that are not child
processes of linkspan itself.
*/
func (c *CriuCheckpointer) Checkpoint(ctx context.Context, internalProcessId string) (*CheckpointResult, error) {
	managedProcess, err := pm.GlobalProcessManager.GetInfo(internalProcessId)
	if err != nil {
		return nil, fmt.Errorf("failed to look up process %s: %w", internalProcessId, err)
	}

	pid := managedProcess.Cmd.Process.Pid
	if pid <= 0 {
		return nil, fmt.Errorf("invalid PID for process %s", internalProcessId)
	}

	if err := c.CRIUCheck(ctx); err != nil {
		return nil, fmt.Errorf("CRIU preflight check failed: %w", err)
	}
	if err := checkPidExists(pid); err != nil {
		return nil, fmt.Errorf("CRIU preflight check failed: %w", err)
	}
	if err := checkAllowedUser(pid, c.AllowedCheckpointUsers); err != nil {
		return nil, fmt.Errorf("CRIU preflight check failed: %w", err)
	}

	imagesDir := filepath.Join(c.DumpDirRoot, internalProcessId)
	if err := os.MkdirAll(imagesDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create dump directory %s: %w", imagesDir, err)
	}

	args := buildDumpArgs(pid, imagesDir, "dump.log", c.AdditionalCriuOpts)
	log.Printf("[Checkpoint] executing: %s %s", c.CriuPath, strings.Join(args, " "))

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.CriuPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	startedAt := time.Now()
	runErr := cmd.Run()
	finishedAt := time.Now()
	exitCode := exitCodeOf(cmd, runErr)

	if ctx.Err() != nil {
		return nil, fmt.Errorf("checkpoint of process %s canceled: %w", internalProcessId, ctx.Err())
	}
	if runErr != nil {
		return nil, fmt.Errorf("criu dump failed for process %s (exit code %d): %w: %s", internalProcessId, exitCode, runErr, strings.TrimSpace(stderr.String()))
	}

	log.Printf("[Checkpoint] process %s checkpointed successfully to %s", internalProcessId, imagesDir)

	return &CheckpointResult{
		ProcessID:  internalProcessId,
		Pid:        pid,
		ImagesDir:  imagesDir,
		LogFile:    filepath.Join(imagesDir, "dump.log"),
		ExitCode:   exitCode,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
	}, nil
}

/*
CheckpointProcessAfterDelay schedules a checkpoint of internalProcessId after
delay has elapsed. The scheduled checkpoint is canceled (never attempted) if
the target process exits before the delay elapses, or if ctx is canceled
(e.g. linkspan shutting down) first.
*/
func (c *CriuCheckpointer) CheckpointProcessAfterDelay(ctx context.Context, internalProcessId string, delay time.Duration) error {
	if delay <= 0 {
		return fmt.Errorf("delay must be greater than 0")
	}

	intProcess, err := pm.GlobalProcessManager.GetInfo(internalProcessId)
	if err != nil {
		return fmt.Errorf("failed to look up process %s: %w", internalProcessId, err)
	}

	go func() {
		log.Printf("[Checkpoint] waiting %s before checkpointing process %s", delay, internalProcessId)

		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
			log.Printf("[Checkpoint] delay elapsed, checkpointing process %s", internalProcessId)
		case <-intProcess.Done:
			log.Printf("[Checkpoint] process %s completed before the checkpoint delay elapsed; skipping checkpoint", internalProcessId)
			return
		case <-ctx.Done():
			log.Printf("[Checkpoint] linkspan is shutting down; canceling scheduled checkpoint for process %s", internalProcessId)
			return
		}

		if _, err := c.Checkpoint(ctx, internalProcessId); err != nil {
			log.Printf("[Checkpoint] delayed checkpoint of process %s failed: %v", internalProcessId, err)
		}
	}()

	return nil
}

/*
Restore restores a process from the checkpoint images stored under
checkpointId (a subdirectory of DumpDirRoot). CRIU is run directly (no
shell) in detached mode (--restore-detached), so Restore returns as soon as
CRIU confirms the restore succeeded or failed, rather than blocking for the
restored process's entire remaining lifetime. The restored root task's PID
(captured via --pidfile) is returned so callers can continue to manage it,
e.g. schedule another checkpoint or watch for it to exit.
*/
func (c *CriuCheckpointer) Restore(ctx context.Context, checkpointId string) (*RestoreResult, error) {
	if err := c.CRIUCheck(ctx); err != nil {
		return nil, fmt.Errorf("CRIU preflight check failed: %w", err)
	}

	imagesDir := filepath.Join(c.DumpDirRoot, checkpointId)
	if info, err := os.Stat(imagesDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("checkpoint images not found at %s", imagesDir)
	}

	args := buildRestoreArgs(imagesDir, "restore.log", "restore.pid", c.AdditionalCriuOpts)
	log.Printf("[Checkpoint] executing: %s %s", c.CriuPath, strings.Join(args, " "))

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.CriuPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	startedAt := time.Now()
	runErr := cmd.Run()
	finishedAt := time.Now()
	exitCode := exitCodeOf(cmd, runErr)

	if ctx.Err() != nil {
		return nil, fmt.Errorf("restore of %s canceled: %w", checkpointId, ctx.Err())
	}
	if runErr != nil {
		return nil, fmt.Errorf("criu restore failed for %s (exit code %d): %w: %s", checkpointId, exitCode, runErr, strings.TrimSpace(stderr.String()))
	}

	pidFilePath := filepath.Join(imagesDir, "restore.pid")
	restoredPid, err := readPidFile(pidFilePath)
	if err != nil {
		log.Printf("[Checkpoint] warning: restore of %s succeeded but pidfile could not be read: %v", checkpointId, err)
	}

	log.Printf("[Checkpoint] restore of %s completed successfully (pid=%d)", checkpointId, restoredPid)

	return &RestoreResult{
		ImagesDir:  imagesDir,
		LogFile:    filepath.Join(imagesDir, "restore.log"),
		ExitCode:   exitCode,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		Pid:        restoredPid,
	}, nil
}