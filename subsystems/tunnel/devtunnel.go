package tunnel

import (
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"

	pm "github.com/cyber-shuttle/conduit/internal/process"
	utils "github.com/cyber-shuttle/conduit/utils"
)

type DevTunnelInfo struct {
	TunnelID      string
	TunnelName    string
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

func DevTunnelCreate(tunnelName string, expiration string, ports []int) error {

	tunnlelCreateCmd := exec.Command("devtunnel", "create", tunnelName, "--expiration", expiration)
	id, err := pm.GlobalProcessManager.Start(tunnlelCreateCmd)
	if err != nil {
		log.Printf("failed to start devtunnel create command: %v", err)
		return err
	}
	err = pm.GlobalProcessManager.Wait(id)
	if err != nil {
		log.Printf("devtunnel create command failed: %v", err)
		stdOut, stdErr, _ := pm.GlobalProcessManager.GetOutput(id)
		log.Printf("devtunnel create command output - stdout: %s, stderr: %s", stdOut, stdErr)
		return err
	}

	GlobalDevTunnelManager.Register(&DevTunnelInfo{
		TunnelID: tunnelName,
		TunnelName: tunnelName,
	})

	for _, port := range ports {
		tunnlAddPortCmd := exec.Command("devtunnel", "port", "create", tunnelName, "-p", fmt.Sprintf("%d", port))
		err := tunnlAddPortCmd.Start()
		if err != nil {
			log.Printf("failed to start devtunnel port create command: %v", err)
			return err
		}
		err = tunnlAddPortCmd.Wait()
		if err != nil {
			log.Printf("devtunnel port create command failed: %v", err)
			return err
		}
	}

	return nil
}

func DevTunnelDelete(tunnelName string) error {
	tunnlelDeleteCmd := exec.Command("devtunnel", "delete", tunnelName, "-f")
	id, err := pm.GlobalProcessManager.Start(tunnlelDeleteCmd)
	if err != nil {
		log.Printf("failed to start devtunnel delete command: %v", err)
		return err
	}
	err = pm.GlobalProcessManager.Wait(id)
	if err != nil {
		log.Printf("devtunnel delete command failed: %v", err)
		stdOut, stdErr, _ := pm.GlobalProcessManager.GetOutput(id)
		log.Printf("devtunnel delete command output - stdout: %s, stderr: %s", stdOut, stdErr)
		return err
	}
	return nil
}

func DevTunnelConnect(tunnelName string, createToken bool) (string, DevTunnelConnection, error) {

	devTunInfo, err := GlobalDevTunnelManager.Find(tunnelName)
	if err != nil {
		return "", DevTunnelConnection{}, err
	}

	tunnelCommand := exec.Command("devtunnel", "host", tunnelName)
	tunnelCommandId, err := pm.GlobalProcessManager.Start(tunnelCommand)
	if err != nil {
		log.Printf("failed to host devtunnel command: %v", err)
		return "", DevTunnelConnection{}, err
	}

	time.Sleep(3 * time.Second) // wait for tunnel to initialize
	stdOut, _, err := pm.GlobalProcessManager.GetOutput(tunnelCommandId)
	if err != nil {
		log.Printf("failed to get output for devtunnels command: %v", err)
		return "", DevTunnelConnection{}, err
	}

	connectionUrl, err := utils.FindLineInStdout(stdOut, "Connect via browser:")

	if err != nil {
		log.Printf("failed to find connection URL in devtunnels output: %v", err)
		return "", DevTunnelConnection{}, err
	}

	tunnelId, err := utils.FindLineInStdout(stdOut, "Ready to accept connections for tunnel: ")
	if err != nil {
		log.Printf("failed to find tunnel ID in devtunnels output: %v", err)
		return "", DevTunnelConnection{}, err
	}

	if createToken {
		// create a token for the tunnel
		tokenCommand := exec.Command("devtunnel", "token", tunnelId, "--scopes", "connect")
		tokenCommandId, err := pm.GlobalProcessManager.Start(tokenCommand)
		if err != nil {
			log.Printf("failed to start devtunnels token command: %v", err)
			return "", DevTunnelConnection{}, err
		}

		time.Sleep(3 * time.Second) // wait for token command to complete
		stdOut, _, err := pm.GlobalProcessManager.GetOutput(tokenCommandId)
		if err != nil {
			log.Printf("failed to get output for devtunnels token command: %v", err)
			return "", DevTunnelConnection{}, err
		}

		token, err := utils.FindLineInStdout(stdOut, "Token:")
		if err != nil {
			log.Printf("failed to find token in devtunnels output: %v", err)
			return "", DevTunnelConnection{}, err
		}

		return tunnelCommandId, DevTunnelConnection{
			ConnectionURL: connectionUrl,
			DevTunnelInfo:      devTunInfo,
			Token:         token,
		}, nil

	} else {
		return tunnelCommandId, DevTunnelConnection{
			ConnectionURL: connectionUrl,
			DevTunnelInfo:      devTunInfo,
		}, nil
	}
}
