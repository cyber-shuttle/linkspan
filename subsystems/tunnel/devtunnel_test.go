package tunnel

import (
	"testing"

	pm "github.com/cyber-shuttle/conduit/internal/process"
)

func TestDevTunnelSetup(t *testing.T) {
	tunnelCommandId, tunnelConfig, err := DevTunnelSetup(8080, true)
	if err != nil {
		t.Fatalf("failed to set up dev tunnel: %v", err)
	} else {
		t.Logf("dev tunnel set up successfully: %+v", tunnelConfig)
	}

	err = pm.GlobalProcessManager.Kill(tunnelCommandId)
	if err != nil {
		t.Fatalf("failed to stop dev tunnel command with id %s: %v", tunnelCommandId, err)
	} else {
		t.Logf("dev tunnel command with id %s stopped successfully", tunnelCommandId)
	}
}