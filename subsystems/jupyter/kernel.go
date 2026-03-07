package jupyter

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
	venv "github.com/cyber-shuttle/linkspan/subsystems/env/venv"
	utils "github.com/cyber-shuttle/linkspan/utils"
)

func startKernelWithVenv(kernelName string, venvPath string) (string, int, error) {
	// placeholder: start a Jupyter kernel process
	err := venv.CreatePythonVirtualEnvironment(venvPath)
	if err != nil {
		log.Printf("Error creating Python virtual environment: %v", err)
		return "", 0, err
	}

	err = venv.InstallPackageInPythonVenv(venvPath, "cspyk")
	if err != nil {
		log.Printf("Error installing package cspyk in Python virtual environment: %v", err)
		return "", 0, err
	}

	err = venv.InstallPackageInPythonVenv(venvPath, "jupyter")
	if err != nil {
		log.Printf("Error installing package jupyter in Python virtual environment: %v", err)
		return "", 0, err
	}

	// Run command " cspyk-kernel --kernel python3 " inside the venv to start the kernel
	kernelBin := filepath.Join(venvPath, "bin", "cspyk-kernel")

	// ensure the binary exists
	if _, err := os.Stat(kernelBin); err != nil {
		log.Printf("cspyk-kernel not found in venv: %v", err)
		return "", 0, err
	}

	// prepare command to run inside the venv
	cmd := exec.Command(kernelBin, "--kernel", kernelName)
	// ensure the venv's bin is first on PATH and set VIRTUAL_ENV
	cmd.Env = append(os.Environ(),
		"VIRTUAL_ENV="+venvPath,
		"PATH="+filepath.Join(venvPath, "bin")+":"+os.Getenv("PATH"),
	)

	// start the kernel process (register with GlobalProcessManager so we can control it later)
	id, err := pm.GlobalProcessManager.Start(cmd)
	if err != nil {
		log.Printf("failed to start kernel process: %v", err)
		return "", 0, err
	}
	
	log.Printf("cspyk-kernel started with id=%s pid=%d", id, cmd.Process.Pid)

	time.Sleep(2 * time.Second)

	stdOut, stdErr, err := pm.GlobalProcessManager.GetOutput(id)
	if err != nil {
		log.Fatalf("failed to get output for kernel process %s: %v", id, err)
	} else {
		log.Printf("Kernel process %s stdout: %s", id, stdOut)
		log.Printf("Kernel process %s stderr: %s", id, stdErr)
	}

	return id, cmd.Process.Pid, nil
}

func getKernelConnectionFile(kernelInternalID string) (string, error) {
	_, stdErr, err := pm.GlobalProcessManager.GetOutput(kernelInternalID)
	if err != nil {
		log.Printf("failed to get output for kernel process %s: %v", kernelInternalID, err)
		return "", err
	}
	// Find [CSKernelApp] Connection file: /Users/dwannipurage3/Library/Jupyter/runtime/kernel-13477a7c-5485-4321-ac21-78809de4fd6c.json in the stdout
	// look for "[CSKernelApp] Connection file: <path>" in stdout
	const marker = "[CSKernelApp] Connection file:"
	path, err := utils.FindLineInStdout(stdErr, marker)
	if err == nil {
		return path, nil
	}
	return "", fmt.Errorf("connection file not found in kernel output: %v", err)
}

func getKernelStatus(kernelInternalID string) (string, error) {
	// placeholder: get the Jupyter kernel process status
	info, err := pm.GlobalProcessManager.GetInfo(kernelInternalID)
	if err != nil {
		return "", err
	}
	if info.Completed {
		if info.ProcessError != nil {
			return "error: " + info.ProcessError.Error(), nil
		}
		return "stopped", nil
	}
	return "running", nil
}

func stopKernel(kernelInternalID string) error {
	// placeholder: stop the Jupyter kernel process
	log.Printf("Stopping kernel with internal ID %s", kernelInternalID)
	err := pm.GlobalProcessManager.Kill(kernelInternalID)
	return err
}
