package tunnel

import (
	"fmt"
	"sync"
)

var (
	providersMu sync.RWMutex
	providers   = map[string]TunnelProvider{
		"devtunnel": &DevTunnelProvider{},
		"frp":       NewFRPTunnelProvider(),
	}
	// activeConnections tracks connectionID -> provider name for Disconnect routing.
	connectionsMu    sync.RWMutex
	activeConnections = make(map[string]string) // connectionID -> provider name
)

// RegisterProvider adds a TunnelProvider under the given name.
func RegisterProvider(name string, p TunnelProvider) {
	providersMu.Lock()
	providers[name] = p
	providersMu.Unlock()
}

// GetProvider returns the TunnelProvider for the given name.
func GetProvider(name string) (TunnelProvider, error) {
	providersMu.RLock()
	p, ok := providers[name]
	providersMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown tunnel provider: %s", name)
	}
	return p, nil
}

// TrackConnection records which provider owns a connection for Disconnect routing.
func TrackConnection(connectionID, providerName string) {
	connectionsMu.Lock()
	activeConnections[connectionID] = providerName
	connectionsMu.Unlock()
}

// UntrackConnection removes a connection from tracking.
func UntrackConnection(connectionID string) {
	connectionsMu.Lock()
	delete(activeConnections, connectionID)
	connectionsMu.Unlock()
}

// ConnectionProvider returns the provider name that owns a connection.
func ConnectionProvider(connectionID string) (string, bool) {
	connectionsMu.RLock()
	name, ok := activeConnections[connectionID]
	connectionsMu.RUnlock()
	return name, ok
}
