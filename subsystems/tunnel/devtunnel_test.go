package tunnel

import (
	"testing"

	pm "github.com/cyber-shuttle/conduit/internal/process"
)

func TestDevTunnelConnect(t *testing.T) {

	tunnelName := "test-tunnel"

	defer func() {
		err := DevTunnelDelete(tunnelName)
		if err != nil {
			t.Logf("warning: failed to delete dev tunnel %s: %v", tunnelName, err)
		}
	}()

	err := DevTunnelCreate(tunnelName, "1d", []int{8080})
	if err != nil {
		t.Fatalf("failed to create dev tunnel: %v", err)
	} else {
		t.Logf("dev tunnel created successfully")
	}

	tunnelCommandId, tunnelConnection, err := DevTunnelConnect(tunnelName, true)
	if err != nil {
		t.Fatalf("failed to set up dev tunnel: %v", err)
	} else {
		t.Logf("dev tunnel set up successfully: %+v", tunnelConnection)
	}

	err = pm.GlobalProcessManager.Kill(tunnelCommandId)
	if err != nil {
		t.Fatalf("failed to stop dev tunnel command with id %s: %v", tunnelCommandId, err)
	} else {
		t.Logf("dev tunnel command with id %s stopped successfully", tunnelCommandId)
	}
}

func TestDevTunnelCreate(t *testing.T) {
	err := DevTunnelCreate("test-tunnel", "1d", []int{8080, 9090})
	if err != nil {
		t.Fatalf("failed to create dev tunnel: %v", err)
	} else {
		t.Logf("dev tunnel created successfully")
	}

	err = DevTunnelDelete("test-tunnel")
	if err != nil {
		t.Fatalf("failed to delete dev tunnel: %v", err)
	} else {
		t.Logf("dev tunnel deleted successfully")
	}	
}