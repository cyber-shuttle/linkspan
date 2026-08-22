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
	Ports      []int  // ports currently being forwarded
	HostCmdID  string // ProcessManager ID of the running host CLI process
	// Tokens stay in the process: this struct is served by GET /tunnels/devtunnels.
	HostToken string `json:"-"` // cached host-scoped access token for restarts
	AuthToken string `json:"-"` // Microsoft Entra ID bearer token used to create the tunnel
	External  bool   // created by the client (cs-bridge); linkspan hosts but never deletes it
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

// Find retrieves a copy of tunnel metadata by name.
// The returned value is safe to read without synchronization.
func (tm *DevTunnelManager) Find(tunnelName string) (*DevTunnelInfo, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tunnel, exists := tm.tunnels[tunnelName]
	if !exists {
		return nil, fmt.Errorf("tunnel %s not found", tunnelName)
	}
	cp := *tunnel
	cp.Ports = make([]int, len(tunnel.Ports))
	copy(cp.Ports, tunnel.Ports)
	return &cp, nil
}

// AddPort records a newly-forwarded port on the named tunnel.
func (tm *DevTunnelManager) AddPort(tunnelName string, port int) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tunnel, exists := tm.tunnels[tunnelName]
	if !exists {
		return fmt.Errorf("tunnel %s not found", tunnelName)
	}
	tunnel.Ports = append(tunnel.Ports, port)
	return nil
}

// UpdateHostCmd atomically updates the host CLI process ID on the named tunnel.
func (tm *DevTunnelManager) UpdateHostCmd(tunnelName, cmdID string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tunnel, exists := tm.tunnels[tunnelName]
	if !exists {
		return fmt.Errorf("tunnel %s not found", tunnelName)
	}
	tunnel.HostCmdID = cmdID
	return nil
}

// Remove deletes a tunnel from the manager by name.
func (tm *DevTunnelManager) Remove(tunnelName string) {
	tm.mu.Lock()
	delete(tm.tunnels, tunnelName)
	tm.mu.Unlock()
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

// CleanAll deletes every tunnel this process created, each with the token it was
// created with. Client-owned tunnels are left alone; the client deletes those.
func (tm *DevTunnelManager) CleanAll() error {
	tm.mu.Lock()
	owned := make(map[string]string, len(tm.tunnels))
	for name, t := range tm.tunnels {
		if !t.External {
			owned[name] = t.AuthToken
		}
	}
	tm.mu.Unlock()

	for name, authToken := range owned {
		log.Printf("devtunnel manager: cleaning up tunnel %s", name)
		if err := DevTunnelDelete(name, authToken); err != nil {
			log.Printf("devtunnel manager: failed to delete tunnel %s: %v", name, err)
		} else {
			tm.mu.Lock()
			delete(tm.tunnels, name)
			tm.mu.Unlock()
		}
	}
	log.Printf("Dev tunnels deleted")
	return nil
}
