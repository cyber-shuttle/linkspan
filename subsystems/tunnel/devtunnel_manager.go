package tunnel

import (
	"fmt"
	"log"
	"sync"
)

// DevTunnelInfo holds the identifying information for a managed dev tunnel.
type DevTunnelInfo struct {
	TunnelID   string
	ClusterID  string
	TunnelName string
	Ports      []int
}

// QualifiedID returns the cluster-qualified tunnel ID (e.g. "ls-48.use2")
// required by the devtunnel CLI for connect operations.
func (d *DevTunnelInfo) QualifiedID() string {
	if d.ClusterID != "" {
		return d.TunnelID + "." + d.ClusterID
	}
	return d.TunnelID
}

// DevTunnelConnection bundles a connection URL with its associated token (if any)
// and the backing tunnel metadata.
type DevTunnelConnection struct {
	ConnectionURL string
	Token         string
	DevTunnelInfo *DevTunnelInfo
}

// DevTunnelManager tracks dev tunnels created during the current process lifetime
// so they can be enumerated and cleaned up on shutdown.
type DevTunnelManager struct {
	mu      sync.Mutex
	tunnels map[string]*DevTunnelInfo
}

func newDevTunnelManager() *DevTunnelManager {
	return &DevTunnelManager{tunnels: make(map[string]*DevTunnelInfo)}
}

// GlobalDevTunnelManager is the package-level singleton.
var GlobalDevTunnelManager = newDevTunnelManager()

// Register stores the tunnel metadata.  Returns the tunnel name and a nil error
// on success; the error return is kept for future validation hooks.
func (tm *DevTunnelManager) Register(tunnel *DevTunnelInfo) (string, error) {
	tm.mu.Lock()
	tm.tunnels[tunnel.TunnelName] = tunnel
	tm.mu.Unlock()
	return tunnel.TunnelName, nil
}

// Find retrieves tunnel metadata by name.
func (tm *DevTunnelManager) Find(tunnelName string) (*DevTunnelInfo, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tunnel, exists := tm.tunnels[tunnelName]
	if !exists {
		return nil, fmt.Errorf("tunnel %s not found", tunnelName)
	}
	return tunnel, nil
}

// GetAll returns a snapshot of all tracked tunnels.
func (tm *DevTunnelManager) GetAll() ([]*DevTunnelInfo, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	out := make([]*DevTunnelInfo, 0, len(tm.tunnels))
	for _, t := range tm.tunnels {
		out = append(out, t)
	}
	return out, nil
}

// CleanAll deletes every tracked tunnel via the SDK.  authToken must be the same
// Microsoft Entra ID token that was used when the tunnels were created.
func (tm *DevTunnelManager) CleanAll(authToken string) error {
	tm.mu.Lock()
	names := make([]string, 0, len(tm.tunnels))
	for name := range tm.tunnels {
		names = append(names, name)
	}
	tm.mu.Unlock()

	for _, name := range names {
		log.Printf("devtunnel manager: cleaning up tunnel %s", name)
		if err := DevTunnelDelete(name, authToken); err != nil {
			log.Printf("devtunnel manager: failed to delete tunnel %s: %v", name, err)
		} else {
			tm.mu.Lock()
			delete(tm.tunnels, name)
			tm.mu.Unlock()
		}
	}
	return nil
}
