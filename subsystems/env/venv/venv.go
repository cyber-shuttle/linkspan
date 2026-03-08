package env

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

const defaultPythonBinary = "/usr/bin/python3"

// CreateVirtualEnvironment creates a Python virtual environment at the specified path.
// It uses python3 to run `python -m venv`.
func CreatePythonVirtualEnvironment(venvPath string) error {
	pythonBinary := defaultPythonBinary

	// Ensure the venv directory exists (parent directory)
	venvDir := filepath.Dir(venvPath)
	if err := os.MkdirAll(venvDir, 0755); err != nil {
		return fmt.Errorf("failed to create venv directory: %w", err)
	}

	log.Printf("creating virtual environment at %s using %s", venvPath, pythonBinary)

	// Run: python -m venv <venvPath>
	cmd := exec.Command(pythonBinary, "-m", "venv", venvPath)

	// Capture stdout and stderr
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to create venv: %w, output: %s", err, string(output))
	}

	log.Printf("virtual environment created successfully: %s. Command output: %s", venvPath, string(output))
	return nil
}

// GetVenvPythonBinary returns the path to the python executable inside a venv.
// On macOS/Linux: <venvPath>/bin/python
// On Windows: <venvPath>/Scripts/python.exe
func GetPythonVenvBinary(venvPath string) string {
	// For cross-platform support, check if running on Windows
	if os.PathSeparator == '\\' {
		// Windows
		return filepath.Join(venvPath, "Scripts", "python.exe")
	}
	// macOS and Linux
	return filepath.Join(venvPath, "bin", "python")
}

// InstallPackageInVenv installs a pip package in a virtual environment.
func InstallPackageInPythonVenv(venvPath, packageName string) error {
	pythonBinary := GetPythonVenvBinary(venvPath)

	log.Printf("installing package %s in venv %s", packageName, venvPath)

	// Run: <pythonBinary> -m pip install <packageName>
	cmd := exec.Command(pythonBinary, "-m", "pip", "install", packageName)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to install package %s: %w, output: %s", packageName, err, string(output))
	}

	log.Printf("package %s installed successfully", packageName)
	return nil
}
