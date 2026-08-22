package tunnel

import "testing"

// The client owns the tunnel it asked linkspan to host, so shutdown must leave it alone.
func TestCleanAllSkipsExternalTunnels(t *testing.T) {
	const name = "test-external-tunnel"
	GlobalDevTunnelManager.Register(&DevTunnelInfo{TunnelName: name, TunnelID: name, External: true})
	defer GlobalDevTunnelManager.Remove(name)

	if err := GlobalDevTunnelManager.CleanAll(); err != nil {
		t.Fatalf("CleanAll: %v", err)
	}
	if _, err := GlobalDevTunnelManager.Find(name); err != nil {
		t.Error("external tunnel was removed by CleanAll; it must be left for the client to delete")
	}
}
