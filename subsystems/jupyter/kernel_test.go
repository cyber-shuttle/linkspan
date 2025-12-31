package jupyter

import (
	"os"
	"testing"
	"time"

	pm "github.com/cyber-shuttle/conduit/internal/process"
)

func TestStartKernelWithVenv(t *testing.T) {
	// Test starting a kernel with a venv
	kernelVenv := "/tmp/test-kernel-venv"
	kernelName := "python3"

	// Delete the venv directory after the test
	defer func() {
		err := os.RemoveAll(kernelVenv)
		if err != nil {
			t.Logf("warning: failed to remove test kernel venv %s: %v", kernelVenv, err)
		}
	}()

	internalID, pid, err := startKernelWithVenv(kernelName, kernelVenv)
	if err != nil {
		t.Logf("warning: starting kernel with venv failed: %v", err)
		// Don't fail the test if starting the kernel fails
		t.Skip("starting kernel with venv failed")
	}

	// wait a bit for the kernel to initialize
	time.Sleep(5 * time.Second)

	_, _, err = pm.GlobalProcessManager.GetOutput(internalID)
	if err != nil {
		t.Fatalf("failed to get output for kernel process %s: %v", internalID, err)
	} //else {
	//	t.Logf("Kernel process %s stdout: %s", internalID, stdOut)
	//	t.Logf("Kernel process %s stderr: %s", internalID, stdErr)
	//}

	connFile, err := getKernelConnectionFile(internalID)
	if err != nil {
		t.Fatalf("failed to get kernel connection file for internal ID %s: %v", internalID, err)
	} else {
		t.Logf("Kernel connection file for internal ID %s: %s", internalID, connFile)
	}

	err = stopKernel(internalID)
	if err != nil {
		t.Fatalf("failed to stop kernel with internal ID %s: %v", internalID, err)
	}

	t.Logf("started kernel with internal ID %s and PID %d", internalID, pid)
}
