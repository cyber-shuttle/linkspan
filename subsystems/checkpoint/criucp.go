package checkpoint

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/controller"
	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

type CriuCheckpointer struct {
	CriuPath             string
	SupportGpuCheckpoint bool
	AdditionalCriuOpts   []string
	DumpDirRoot          string
}

func (c *CriuCheckpointer) CRIUCheck() error {
	// Check if CRIU is installed and available in the system path
	if c.CriuPath == "" {
		return fmt.Errorf("CRIU path is not defined in the config")
	}

	// check if the CRIU binary exists at the specified path
	if _, err := os.Stat(c.CriuPath); err != nil {
		return fmt.Errorf("CRIU binary not found at path: %s", c.CriuPath)
	}

	return nil
}

func (c *CriuCheckpointer) CheckpointProcessAfterDelay(internalProcessId string, checkpointAfterDelay int64) error {

	if checkpointAfterDelay <= 0 {
		return fmt.Errorf("checkpointAfterDelay must be greater than 0")
	}

	go func() {
		log.Printf("[Checkpoint] Waiting for %d seconds before checkpointing process %s", checkpointAfterDelay, internalProcessId)
		intProcess, err := pm.GlobalProcessManager.GetInfo(internalProcessId)
		if err != nil {
			log.Printf("[Checkpoint] Error getting process info for %s: %v", internalProcessId, err)
			return
		}
		// Wait for the specified delay before checkpointing
		select {
		case <-time.After(time.Duration(checkpointAfterDelay) * time.Second):
			log.Printf("[Checkpoint] Checkpointing process %s after delay of %d seconds", internalProcessId, checkpointAfterDelay)
		case <-intProcess.Done:
			log.Printf("[Checkpoint] Process %s completed before checkpointing could occur", internalProcessId)
		}

		c.CheckpointProcess(internalProcessId)
	}()

	return nil
}

/*
Supports checkpointing processes that are managed by linkspan's ProcessManager.
The process must be started using the ProcessManager and its internal ID must be provided to this function.
NOTE: Linkspan should be supported to checkpoint processes that are not child processes of linkspan itself.
*/
func (c *CriuCheckpointer) CheckpointProcess(internalProcessId string) (string, error) {
	// Check if CRIU is installed and available in the system path
	if err := c.CRIUCheck(); err != nil {
		return "", err
	}

	managedProcess, err := pm.GlobalProcessManager.GetInfo(internalProcessId)
	if err != nil {
		return "", err
	}

	pid := managedProcess.Cmd.Process.Pid
	if pid <= 0 {
		return "", fmt.Errorf("invalid PID for process %s", internalProcessId)
	}

	dumpDir := fmt.Sprintf("%s/%s", c.DumpDirRoot, internalProcessId)

	if err := os.MkdirAll(dumpDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create dump directory %s: %v", dumpDir, err)
	}

	// Prepare the CRIU command for checkpointing the process
	cmdStr := fmt.Sprintf("%s dump -t %d --shell-job --tcp-established --unprivileged --images-dir %s", c.CriuPath, pid, dumpDir)

	log.Printf("Executing CRIU command: %s", cmdStr)

	cmd := exec.Command("sh", "-c", cmdStr)

	// Execute the CRIU command
	criuProcess, err := pm.GlobalProcessManager.Start(cmd, true)
	if err != nil {
		return "", fmt.Errorf("failed to start CRIU process: %v", err)
	}

	log.Printf("CRIU process started with ID: %s", criuProcess)

	return criuProcess, nil
}

func (c *CriuCheckpointer) RestoreProcess(internalProcessId string, shutdownOnCompletion bool) (string, error) {
	// Check if CRIU is installed and available in the system path
	if err := c.CRIUCheck(); err != nil {
		return "", err
	}

	dumpDir := fmt.Sprintf("%s/%s", c.DumpDirRoot, internalProcessId)

	// Prepare the CRIU command for restoring the process
	cmdStr := fmt.Sprintf("%s restore --shell-job --tcp-established --unprivileged --images-dir %s", c.CriuPath, dumpDir)

	log.Printf("Executing CRIU command: %s", cmdStr)

	cmd := exec.Command("sh", "-c", cmdStr)

	// Execute the CRIU command
	criuProcess, err := pm.GlobalProcessManager.Start(cmd, true)
	if err != nil {
		return "", fmt.Errorf("failed to start CRIU restore process: %v", err)
	}

	log.Printf("CRIU restore process started with ID: %s", criuProcess)

	proc, err := pm.GlobalProcessManager.GetInfo(criuProcess)
	if err != nil {
		return "", fmt.Errorf("failed to get process info: %v", err)
	}

	go func() {
		done := <-proc.Done
		if done != nil {
			log.Printf("Restore process %s completed with error: %v", internalProcessId, done)
		} else {
			log.Printf("Restore process %s completed successfully", internalProcessId)
		}

		if shutdownOnCompletion {
			log.Printf("Restore process %s requested shutdown on completion", internalProcessId)
			controller.TriggerShutdown(fmt.Sprintf("Restore process %s completed", internalProcessId))
		}
	}()

	return criuProcess, nil
}
