package tunnel

import (
	"encoding/json"
	"strings"
	"testing"
)

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

// GET /tunnels/devtunnels serves this struct, so no token may survive marshalling.
func TestDevTunnelInfoHidesTokens(t *testing.T) {
	out, err := json.Marshal(&DevTunnelInfo{TunnelID: "t", HostToken: "host-secret", AuthToken: "entra-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "secret") {
		t.Fatalf("token leaked into JSON: %s", out)
	}
}
