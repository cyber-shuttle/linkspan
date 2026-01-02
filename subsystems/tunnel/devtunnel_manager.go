package tunnel

import (
	"fmt"
	"log"
	"sync"
)

type DevTunnelInfo struct {
	TunnelID      string
	TunnelName    string
	Ports []int
}

type DevTunnelConnection struct {
	ConnectionURL       string
	Token               string
	DevTunnelInfo       *DevTunnelInfo
}

type DevTunnelManager struct {
	mu    sync.Mutex
	tunnels map[string]*DevTunnelInfo
}

func newDevTunnelManager() *DevTunnelManager {
	return &DevTunnelManager{tunnels: make(map[string]*DevTunnelInfo)}
}

var GlobalDevTunnelManager = newDevTunnelManager()

func (tm *DevTunnelManager) Register(tunnel *DevTunnelInfo) (string, error) {
	tm.mu.Lock()
	tm.tunnels[tunnel.TunnelName] = tunnel
	tm.mu.Unlock()
	return tunnel.TunnelName, nil
}

func (tm *DevTunnelManager) Find(tunnelName string) (*DevTunnelInfo, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tunnel, exists := tm.tunnels[tunnelName]
	if !exists {
		return nil, fmt.Errorf("tunnel %s not found", tunnelName)
	}
	return tunnel, nil
}

func (tm *DevTunnelManager) GetAll() ([]*DevTunnelInfo, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tunnels := make([]*DevTunnelInfo, 0, len(tm.tunnels))
	for _, tunnel := range tm.tunnels {
		tunnels = append(tunnels, tunnel)
	}
	return tunnels, nil
}

func (tm *DevTunnelManager) CleanAll() error {
	tm.mu.Lock()
	for id := range tm.tunnels {
		log.Printf("cleaning up dev tunnel %s", tm.tunnels[id].TunnelName)
		err := DevTunnelDelete(tm.tunnels[id].TunnelName)
		if err != nil {
			log.Printf("failed to delete dev tunnel %s: %v", tm.tunnels[id].TunnelName, err)
		}
	}
	tm.mu.Unlock()
	return nil
}