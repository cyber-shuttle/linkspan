package tunnel

import (
	"fmt"
	"log"
	"os/exec"
	"time"

	pm "github.com/cyber-shuttle/conduit/internal/process"
	utils "github.com/cyber-shuttle/conduit/utils"
)

func DevTunnelCreate(tunnelName string, expiration string, ports []int) (DevTunnelInfo, error) {

	tunnlelCreateCmd := exec.Command("devtunnel", "create", tunnelName, "--expiration", expiration)
	id, err := pm.GlobalProcessManager.Start(tunnlelCreateCmd)
	if err != nil {
		log.Printf("failed to start devtunnel create command: %v", err)
		return DevTunnelInfo{}, err
	}
	err = pm.GlobalProcessManager.Wait(id)
	if err != nil {
		log.Printf("devtunnel create command failed: %v", err)
		stdOut, stdErr, _ := pm.GlobalProcessManager.GetOutput(id)
		log.Printf("devtunnel create command output - stdout: %s, stderr: %s", stdOut, stdErr)
		return DevTunnelInfo{}, err
	}

	/*
	Example output of devtunnel create command:

	Tunnel ID             : agant-8080-tunnel2.use2
	Description           :
	Labels                :
	Access control        : {}
	Host connections      : 0
	Client connections    : 0
	Current upload rate   : 0 MB/s (limit: 20 MB/s)
	Current download rate : 0 MB/s (limit: 20 MB/s)
	Tunnel Expiration     : 30 days

	Changed default tunnel to agant-8080-tunnel2.use2.
	*/

	stdOut, _, err := pm.GlobalProcessManager.GetOutput(id)
	if err != nil {
		log.Printf("failed to get output for devtunnels create command: %v", err)
		return DevTunnelInfo{}, err
	}

	tunnlelId, err := utils.FindLineInStdout(stdOut, "Tunnel ID             : ") // This is extremely basic and risky parsing, but sufficient for now
	if err != nil {
		log.Printf("failed to find tunnel ID in devtunnels create output: %v for stdout %s", err, stdOut)
		return DevTunnelInfo{}, err
	}

	GlobalDevTunnelManager.Register(&DevTunnelInfo{
		TunnelID: tunnlelId,
		TunnelName: tunnelName,
		Ports: ports,
	})

	for _, port := range ports {
		tunnlAddPortCmd := exec.Command("devtunnel", "port", "create", tunnelName, "-p", fmt.Sprintf("%d", port))
		err := tunnlAddPortCmd.Start()
		if err != nil {
			log.Printf("failed to start devtunnel port create command: %v", err)
			return DevTunnelInfo{}, err
		}
		err = tunnlAddPortCmd.Wait()
		if err != nil {
			log.Printf("devtunnel port create command failed: %v", err)
			return DevTunnelInfo{}, err
		}
	}

	return DevTunnelInfo{
		TunnelID:   tunnlelId,
		TunnelName: tunnelName,
	}, nil
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

func DevTunnelHost(tunnelName string, createToken bool) (string, DevTunnelConnection, error) {

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
