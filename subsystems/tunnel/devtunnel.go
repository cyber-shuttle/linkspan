package tunnel

import (
	"fmt"
	"log"
	"os/exec"
	"time"

	utils "github.com/cyber-shuttle/conduit/utils"
	pm "github.com/cyber-shuttle/conduit/internal/process"
)

type DevTunnelInfo struct {
	ConnectionURL string
	TunnelID     string
	Token        string
}

func DevTunnelSetup(port int, createToken bool) (string, DevTunnelInfo, error) {

	tunnelCommand := exec.Command("devtunnel", "host", "-p", fmt.Sprintf("%d", port))
	tunnelCommandId, err := pm.GlobalProcessManager.Start(tunnelCommand)
	if err != nil {
		log.Printf("failed to start devtunnels command: %v", err)
	} else {
		//log.Printf("devtunnels command started with PID %d", tunnelCommand.Process.Pid)
	}

	time.Sleep(2 * time.Second) // wait for tunnel to initialize
	stdOut, _, err := pm.GlobalProcessManager.GetOutput(tunnelCommandId)
	if err != nil {
		log.Printf("failed to get output for devtunnels command: %v", err)
		return "", DevTunnelInfo{}, err
	} else {
		//log.Printf("devtunnels command output - stdout: %s, stderr: %s", stdOut, stdErr)
	}
	connectionUrl, err := utils.FindLineInStdout(stdOut, "Connect via browser:")

	if err != nil {
		log.Printf("failed to find connection URL in devtunnels output: %v", err)
		return "", DevTunnelInfo{}, err
	} else {
		//log.Printf("devtunnels connection URL: %s", connectionUrl)
	}

	tunnelId, err := utils.FindLineInStdout(stdOut, "Ready to accept connections for tunnel: ")
	if err != nil {
		log.Printf("failed to find tunnel ID in devtunnels output: %v", err)
		return "", DevTunnelInfo{}, err
	} else {
		//log.Printf("devtunnels tunnel ID: %s", tunnelId)
	}

	if createToken {
		// create a token for the tunnel
		tokenCommand := exec.Command("devtunnel", "token", tunnelId, "--scopes", "connect")
		tokenCommandId, err := pm.GlobalProcessManager.Start(tokenCommand)
		if err != nil {
			log.Printf("failed to start devtunnels token command: %v", err)
		} else {
			//log.Printf("devtunnels token command started with PID %d", tokenCommand.Process.Pid)
		}

		time.Sleep(2 * time.Second) // wait for token command to complete
		stdOut, _, err := pm.GlobalProcessManager.GetOutput(tokenCommandId)
		if err != nil {
			log.Printf("failed to get output for devtunnels token command: %v", err)
		} else {
			//log.Printf("devtunnels token command output - stdout: %s, stderr: %s", stdOut, stdErr)
		}

		token, err := utils.FindLineInStdout(stdOut, "Token:")
		if err != nil {
			log.Printf("failed to find token in devtunnels output: %v", err)
			return "", DevTunnelInfo{}, err
		} else {
			//log.Printf("devtunnels token: %s", token)
		}	
		
		return tunnelCommandId, DevTunnelInfo{
			ConnectionURL: connectionUrl,
			TunnelID:     tunnelId,
			Token:        token,
		}, nil
		
	} else {	
		return tunnelCommandId, DevTunnelInfo{
			ConnectionURL: connectionUrl,
			TunnelID:     tunnelId,
		}, nil
	}
}