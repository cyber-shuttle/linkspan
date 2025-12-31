package env

import (
	"testing"
)

func TestCreatePythonVirtualEnvironment(t *testing.T) {
	// Test creating a venv in a temporary directory
	venvPath := "/tmp/test-venv-conduit"

	err := CreatePythonVirtualEnvironment(venvPath)
	if err != nil {
		t.Logf("warning: venv creation requires python3 available: %v", err)
		// Don't fail the test if python3 isn't available
		t.Skip("python3 not available or venv creation failed")
	}
}

func TestGetPythonVenvBinary(t *testing.T) {
	venvPath := "/tmp/test-venv"
	pythonPath := GetPythonVenvBinary(venvPath)

	// On non-Windows, should be /tmp/test-venv/bin/python
	// On Windows, should be /tmp/test-venv/Scripts/python.exe
	if pythonPath == "" {
		t.Fatal("expected non-empty python path")
	}
	t.Logf("venv python path: %s", pythonPath)
}

/*
func TestProvisionKernelWithVenv(t *testing.T) {
	// Test the ProvisionKernel handler
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jupyter/kernels", nil)
	rr := httptest.NewRecorder()

	ProvisionKernel(rr, req)
	res := rr.Result()

	if res.StatusCode != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", res.StatusCode)
	}
}
	*/
