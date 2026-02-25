package tunnel

import (
	"os"
	"testing"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

// authTokenForTest reads the dev-tunnel auth token from the DEVTUNNEL_AUTH_TOKEN
// environment variable.  Tests that require a real token are skipped when the
// variable is not set so that CI stays green without live credentials.
func authTokenForTest(t *testing.T) string {
	t.Helper()
	token := os.Getenv("DEVTUNNEL_AUTH_TOKEN")
	if token == "" {
		t.Skip("DEVTUNNEL_AUTH_TOKEN not set — skipping integration test")
	}
	return token
}

func TestDevTunnelConnect(t *testing.T) {
	authToken := authTokenForTest(t)
	tunnelName := "test-tunnel"

	defer func() {
		err := DevTunnelDelete(tunnelName, authToken)
		if err != nil {
			t.Logf("warning: failed to delete dev tunnel %s: %v", tunnelName, err)
		}
	}()

	_, err := DevTunnelCreate(tunnelName, "1d", []int{8080}, authToken)
	if err != nil {
		t.Fatalf("failed to create dev tunnel: %v", err)
	} else {
		t.Logf("dev tunnel created successfully")
	}

	tunnelCommandId, tunnelConnection, err := DevTunnelHost(tunnelName, authToken)
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
	authToken := authTokenForTest(t)

	_, err := DevTunnelCreate("test-tunnel", "1d", []int{8080, 9090}, authToken)
	if err != nil {
		t.Fatalf("failed to create dev tunnel: %v", err)
	} else {
		t.Logf("dev tunnel created successfully")
	}

	err = DevTunnelDelete("test-tunnel", authToken)
	if err != nil {
		t.Fatalf("failed to delete dev tunnel: %v", err)
	} else {
		t.Logf("dev tunnel deleted successfully")
	}
}
