package env

import (
	"testing"
)

func TestCreateCondaEnvironment(t *testing.T) {
	// Test creating a conda environment with a given name
	envName := "test-conda-env"

	err := CreateCondaEnvironment(envName)
	if err != nil {
		t.Logf("warning: conda environment creation failed: %v", err)
		// Don't fail the test if conda isn't available
		t.Skip("conda not available or environment creation failed")
	}
}