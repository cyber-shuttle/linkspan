package checkpoint

import (
	"fmt"
	"log"
	"os"
	"os/exec"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

type CriuCheckpointer struct {
	criuPath             string
	supportGpuCheckpoint bool
	additionalCriuOpts   []string
	dumpDirRoot          string
}

func (c *CriuCheckpointer) CRIUCheck() error {
	// Check if CRIU is installed and available in the system path
	if c.criuPath == "" {
		return fmt.Errorf("CRIU path is not defined in the config")
	}

	// check if the CRIU binary exists at the specified path
	if _, err := os.Stat(c.criuPath); err != nil {
		return fmt.Errorf("CRIU binary not found at path: %s", c.criuPath)
	}

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

	dumpDir := fmt.Sprintf("%s/%s", c.dumpDirRoot, internalProcessId)

	if err := os.MkdirAll(dumpDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create dump directory %s: %v", dumpDir, err)
	}

	// Prepare the CRIU command for checkpointing the process
	cmdStr := fmt.Sprintf("%s dump -t %d --shell-job --tcp-established --unprivileged --images-dir %s", c.criuPath, pid, dumpDir)

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

func (c *CriuCheckpointer) RestoreProcess(internalProcessId string) (string, error) {
	// Check if CRIU is installed and available in the system path
	if err := c.CRIUCheck(); err != nil {
		return "", err
	}

	dumpDir := fmt.Sprintf("%s/%s", c.dumpDirRoot, internalProcessId)

	// Prepare the CRIU command for restoring the process
	cmdStr := fmt.Sprintf("%s restore --shell-job --tcp-established --unprivileged --images-dir %s", c.criuPath, dumpDir)

	log.Printf("Executing CRIU command: %s", cmdStr)

	cmd := exec.Command("sh", "-c", cmdStr)

	// Execute the CRIU command
	criuProcess, err := pm.GlobalProcessManager.Start(cmd, true)
	if err != nil {
		return "", fmt.Errorf("failed to start CRIU restore process: %v", err)
	}

	log.Printf("CRIU restore process started with ID: %s", criuProcess)

	return criuProcess, nil
}
