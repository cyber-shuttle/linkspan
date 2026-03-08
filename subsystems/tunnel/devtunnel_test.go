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

func TestDevTunnelCreateAndHost(t *testing.T) {
	authToken := authTokenForTest(t)
	tunnelName := "test-tunnel"

	defer func() {
		err := DevTunnelDelete(tunnelName, authToken)
		if err != nil {
			t.Logf("warning: failed to delete dev tunnel %s: %v", tunnelName, err)
		}
	}()

	conn, err := DevTunnelCreate(tunnelName, "1d", authToken, 8080, 0)
	if err != nil {
		t.Fatalf("failed to create dev tunnel: %v", err)
	}
	t.Logf("dev tunnel created+hosted: url=%s token=%s", conn.ConnectionURL, conn.Token)

	if conn.DevTunnelInfo.HostCmdID == "" {
		t.Fatal("expected host command ID to be set")
	}

	err = pm.GlobalProcessManager.Kill(conn.DevTunnelInfo.HostCmdID)
	if err != nil {
		t.Fatalf("failed to stop dev tunnel host: %v", err)
	}
	t.Logf("dev tunnel host stopped successfully")
}

func TestDevTunnelCreateNoPort(t *testing.T) {
	authToken := authTokenForTest(t)
	tunnelName := "test-tunnel-noport"

	conn, err := DevTunnelCreate(tunnelName, "1d", authToken, 0, 0)
	if err != nil {
		t.Fatalf("failed to create dev tunnel: %v", err)
	}
	t.Logf("dev tunnel created: url=%s", conn.ConnectionURL)

	if conn.DevTunnelInfo.HostCmdID != "" {
		_ = pm.GlobalProcessManager.Kill(conn.DevTunnelInfo.HostCmdID)
	}

	err = DevTunnelDelete(tunnelName, authToken)
	if err != nil {
		t.Fatalf("failed to delete dev tunnel: %v", err)
	}
	t.Logf("dev tunnel deleted successfully")
}
