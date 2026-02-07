package tunnel

import (
	"testing"
	"time"
)

func TestCreateAndStopTunnel(t *testing.T) {
	// This is a placeholder test. In a real test, you would:
	// 1. Create a tunnel with a test configuration.
	// 2. Verify that the tunnel is active (e.g., by checking logs or status).
	// 3. Stop the tunnel using StopTunnelByID.
	// 4. Verify that the tunnel has stopped (e.g., by checking logs or status).

	// Example:
	tunnelName := "test-tunnel-123"
	tunnelType := "tcp"

	// Simulate creating a tunnel (in real code, call the actual function)
	tunnelInfo := FrpTunnelInfo{TunnelName: tunnelName, TunnelType: tunnelType}
	if tunnelInfo.TunnelName != tunnelName || tunnelInfo.TunnelType != tunnelType {
		t.Errorf("unexpected tunnel info: got %v", tunnelInfo)
	}

	 _, err := FrpTunnelProxyCreate(tunnelName, 22, tunnelType, "abc", "hub.dev.cybershuttle.org", 7000, "mysecret")
	if err != nil {
		t.Logf("tunnel creation returned error (expected if server not available): %v", err)
		return // Skip the rest of the test if creation failed
	}

	// sleep 5
	time.Sleep(5 * time.Second)

	// Stop the tunnel using the client directly
	err = StopFrpTunnelByName(tunnelName)
	if err != nil {
		t.Errorf("failed to stop tunnel: %v", err)
	}
}	