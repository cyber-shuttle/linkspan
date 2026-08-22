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

	"github.com/cyber-shuttle/linkspan/internal/config"
)

// criuCheckpointer is the low-level CRIU mechanics: building argv, invoking
// the binary, and writing the durable checkpoint layout. It is unexported
// on purpose — CheckpointService (service.go) is the only thing allowed to
// construct or call it, so CRIU mechanics can never leak into main.go.
type criuCheckpointer struct {
	CriuPath               string
	SupportGpuCheckpoint   bool
	AdditionalCriuOpts     []string
	CheckpointRoot         string
	AllowedCheckpointUsers []string
	LinkspanVersion        string
	LinkspanCommit         string
}

func newCriuCheckpointer(c *config.LinkspanConfig) *criuCheckpointer {
	return &criuCheckpointer{
		CriuPath:               c.CRIUPath,
		SupportGpuCheckpoint:   c.SupportGpuCheckpoint,
		AdditionalCriuOpts:     c.AdditionalCriuOpts,
		CheckpointRoot:         c.CheckpointRoot,
		AllowedCheckpointUsers: c.AllowedCheckpointUsers,
		LinkspanVersion:        c.Version,
		LinkspanCommit:         c.Commit,
	}
}

func buildDumpArgs(pid int, imagesDir, workDir, logFile string, extra []string) []string {
	args := []string{
		"dump",
		"-t", strconv.Itoa(pid),
		"--shell-job",
		"--tcp-established",
		"--unprivileged",
		"--images-dir", imagesDir,
		"--work-dir", workDir,
		"--log-file", logFile,
	}
	return append(args, extra...)
}

func buildRestoreArgs(imagesDir, workDir, logFile, pidFile string, extra []string) []string {
	args := []string{
		"restore",
		"--shell-job",
		"--tcp-established",
		"--unprivileged",
		"--restore-detached",
		"--images-dir", imagesDir,
		"--work-dir", workDir,
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
checkpoint checkpoints the process at pid, identified for provenance by
workloadID and (optionally, when the target came from linkspan's
ProcessManager) processID. It runs CRIU directly (no shell), waits for the
dump to finish, and only returns success once CRIU has actually completed
the checkpoint and a manifest + completion marker have been durably written
under CheckpointRoot/workloadID/<new checkpoint id>/.
*/
func (c *criuCheckpointer) checkpoint(ctx context.Context, workloadID, processID string, pid int, trigger CheckpointTrigger) (*CheckpointResult, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid PID %d", pid)
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
	if workloadID == "" {
		return nil, fmt.Errorf("workloadID is required")
	}

	checkpointID := NewCheckpointID()
	checkpointDir := checkpointDirPath(c.CheckpointRoot, workloadID, checkpointID)
	imagesDir := imagesDirPath(checkpointDir)
	if err := os.MkdirAll(imagesDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create checkpoint directory %s: %w", imagesDir, err)
	}

	manifest := gatherManifest(ctx, manifestParams{
		CriuPath:        c.CriuPath,
		WorkloadID:      workloadID,
		LinkspanVersion: c.LinkspanVersion,
		LinkspanCommit:  c.LinkspanCommit,
		GPUMode:         c.SupportGpuCheckpoint,
		ProcessID:       processID,
		PID:             pid,
		CheckpointID:    checkpointID,
		Trigger:         trigger,
	})
	if err := writeManifest(checkpointDir, manifest); err != nil {
		return nil, fmt.Errorf("failed to write manifest for checkpoint %s: %w", checkpointID, err)
	}

	args := buildDumpArgs(pid, imagesDir, checkpointDir, "dump.log", c.AdditionalCriuOpts)
	log.Printf("[Checkpoint] executing: %s %s", c.CriuPath, strings.Join(args, " "))

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.CriuPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	startedAt := time.Now()
	runErr := cmd.Run()
	finishedAt := time.Now()
	exitCode := exitCodeOf(cmd, runErr)

	manifest.CompletedAt = finishedAt.UTC()
	manifest.ExitCode = exitCode
	manifest.CRIUOptions = args

	if ctx.Err() != nil {
		manifest.State = StateFailed
		if werr := writeManifest(checkpointDir, manifest); werr != nil {
			log.Printf("[Checkpoint] warning: failed to record failed state for checkpoint %s: %v", checkpointID, werr)
		}
		return nil, fmt.Errorf("checkpoint of workload %s canceled: %w", workloadID, ctx.Err())
	}
	if runErr != nil {
		manifest.State = StateFailed
		if werr := writeManifest(checkpointDir, manifest); werr != nil {
			log.Printf("[Checkpoint] warning: failed to record failed state for checkpoint %s: %v", checkpointID, werr)
		}
		return nil, fmt.Errorf("criu dump failed for workload %s (exit code %d): %w: %s", workloadID, exitCode, runErr, strings.TrimSpace(stderr.String()))
	}

	manifest.State = StateComplete
	if err := writeManifest(checkpointDir, manifest); err != nil {
		return nil, fmt.Errorf("checkpoint %s: criu dump succeeded but failed to finalize manifest: %w", checkpointID, err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, completeFileName), []byte{}, 0644); err != nil {
		return nil, fmt.Errorf("checkpoint %s: criu dump succeeded but failed to write completion marker: %w", checkpointID, err)
	}

	log.Printf("[Checkpoint] workload %s checkpointed successfully to %s (checkpoint=%s)", workloadID, checkpointDir, checkpointID)

	return &CheckpointResult{
		WorkloadID:   workloadID,
		CheckpointID: checkpointID,
		ProcessID:    processID,
		Pid:          pid,
		ImagesDir:    imagesDir,
		ManifestPath: filepath.Join(checkpointDir, manifestFileName),
		LogFile:      filepath.Join(checkpointDir, "dump.log"),
		ExitCode:     exitCode,
		Stdout:       stdout.String(),
		Stderr:       stderr.String(),
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
	}, nil
}

/*
restore restores a process from the checkpoint images stored under
CheckpointRoot/workloadID/checkpointID. CRIU is run directly (no shell) in
detached mode (--restore-detached), so restore returns as soon as CRIU
confirms the restore succeeded or failed, rather than blocking for the
restored process's entire remaining lifetime. restore refuses to run against
a checkpoint directory that is missing its completion marker or whose
manifest doesn't record a "complete" state — see isCheckpointComplete.
*/
func (c *criuCheckpointer) restore(ctx context.Context, workloadID, checkpointID string) (*RestoreResult, error) {
	if err := c.CRIUCheck(ctx); err != nil {
		return nil, fmt.Errorf("CRIU preflight check failed: %w", err)
	}
	if workloadID == "" || checkpointID == "" {
		return nil, fmt.Errorf("both workloadID and checkpointID are required to restore")
	}

	checkpointDir := checkpointDirPath(c.CheckpointRoot, workloadID, checkpointID)
	if info, err := os.Stat(checkpointDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("checkpoint not found at %s", checkpointDir)
	}
	if !isCheckpointComplete(checkpointDir) {
		return nil, fmt.Errorf("checkpoint %s/%s is missing its completion marker or its manifest state is not %q; refusing to restore a possibly-partial checkpoint", workloadID, checkpointID, StateComplete)
	}

	imagesDir := imagesDirPath(checkpointDir)
	args := buildRestoreArgs(imagesDir, checkpointDir, "restore.log", "restore.pid", c.AdditionalCriuOpts)
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
		return nil, fmt.Errorf("restore of %s/%s canceled: %w", workloadID, checkpointID, ctx.Err())
	}
	if runErr != nil {
		return nil, fmt.Errorf("criu restore failed for %s/%s (exit code %d): %w: %s", workloadID, checkpointID, exitCode, runErr, strings.TrimSpace(stderr.String()))
	}

	pidFilePath := filepath.Join(checkpointDir, "restore.pid")
	restoredPid, err := readPidFile(pidFilePath)
	if err != nil {
		log.Printf("[Checkpoint] warning: restore of %s/%s succeeded but pidfile could not be read: %v", workloadID, checkpointID, err)
	}

	log.Printf("[Checkpoint] restore of %s/%s completed successfully (pid=%d)", workloadID, checkpointID, restoredPid)

	return &RestoreResult{
		WorkloadID:   workloadID,
		CheckpointID: checkpointID,
		ImagesDir:    imagesDir,
		ManifestPath: filepath.Join(checkpointDir, manifestFileName),
		LogFile:      filepath.Join(checkpointDir, "restore.log"),
		ExitCode:     exitCode,
		Stdout:       stdout.String(),
		Stderr:       stderr.String(),
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
		Pid:          restoredPid,
	}, nil
}
