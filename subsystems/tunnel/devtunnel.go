package tunnel

import (
	"context"
	"fmt"
	"log"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

// DevTunnelCreate creates a tunnel, starts hosting the relay, and forwards the
// given serverPort so the client can communicate with linkspan immediately.
// Additional ports (e.g. SSH) can be added later via DevTunnelForward.
func DevTunnelCreate(tunnelName string, expiration string, authToken string, serverPort int) (DevTunnelConnection, error) {
	if err := InitSDK(authToken); err != nil {
		return DevTunnelConnection{}, fmt.Errorf("devtunnel create: init SDK: %w", err)
	}

	ctx := context.Background()

	// 1. Create the tunnel on the service.
	sdkTunnel, err := SDKCreateTunnel(ctx, tunnelName)
	if err != nil {
		return DevTunnelConnection{}, fmt.Errorf("devtunnel create %q: %w", tunnelName, err)
	}

	info := &DevTunnelInfo{
		TunnelID:   sdkTunnel.TunnelID,
		ClusterID:  sdkTunnel.ClusterID,
		TunnelName: tunnelName,
		AuthToken:  authToken,
	}

	if _, err := GlobalDevTunnelManager.Register(info); err != nil {
		log.Printf("devtunnel create: warning — failed to register %q in manager: %v", tunnelName, err)
	}

	// 2. Register the server port so it is forwarded through the tunnel.
	if serverPort > 0 {
		if err := SDKAddPort(ctx, tunnelName, serverPort); err != nil {
			return DevTunnelConnection{}, fmt.Errorf("devtunnel create: add server port %d to %q: %w", serverPort, tunnelName, err)
		}
		info.Ports = append(info.Ports, serverPort)
	}

	// 3. Obtain host token and start the relay with the server port forwarded.
	hostToken, err := SDKGetHostToken(ctx, tunnelName)
	if err != nil {
		return DevTunnelConnection{}, fmt.Errorf("devtunnel create: get host token for %q: %w", tunnelName, err)
	}

	cmdID, connectionURL, err := CLIHostTunnel(info.TunnelID, hostToken, info.Ports)
	if err != nil {
		return DevTunnelConnection{}, fmt.Errorf("devtunnel create: start host for %q: %w", tunnelName, err)
	}

	info.HostCmdID = cmdID
	info.HostToken = hostToken

	conn := DevTunnelConnection{
		ConnectionURL: connectionURL,
		DevTunnelInfo: info,
	}

	// 4. Get a connect token for the client side.
	connectToken, tokenErr := SDKGetConnectToken(ctx, tunnelName)
	if tokenErr != nil {
		log.Printf("devtunnel create: warning — could not obtain connect token for %q: %v", tunnelName, tokenErr)
	} else {
		conn.Token = connectToken
	}

	log.Printf("devtunnel create: tunnel %q ready (id=%s, url=%s, port=%d)", tunnelName, sdkTunnel.TunnelID, connectionURL, serverPort)
	return conn, nil
}

// DevTunnelForward adds port forwarding to an existing hosted tunnel.
// It registers the port via the SDK, then restarts the host CLI with the
// updated port list so traffic is actually forwarded.
func DevTunnelForward(tunnelName string, port int, authToken string) error {
	if err := InitSDK(authToken); err != nil {
		return fmt.Errorf("devtunnel forward: init SDK: %w", err)
	}

	devTunInfo, err := GlobalDevTunnelManager.Find(tunnelName)
	if err != nil {
		return fmt.Errorf("devtunnel forward: tunnel %q not registered: %w", tunnelName, err)
	}

	// Check if this port is already forwarded.
	for _, p := range devTunInfo.Ports {
		if p == port {
			log.Printf("devtunnel forward: port %d already forwarded on %q", port, tunnelName)
			return nil
		}
	}

	// Register the port on the tunnel service.
	ctx := context.Background()
	if err := SDKAddPort(ctx, tunnelName, port); err != nil {
		return fmt.Errorf("devtunnel forward: add port %d to %q: %w", port, tunnelName, err)
	}

	devTunInfo.Ports = append(devTunInfo.Ports, port)

	// Restart the host CLI with the updated port list.
	if devTunInfo.HostCmdID != "" {
		log.Printf("devtunnel forward: restarting host for %q with ports %v", tunnelName, devTunInfo.Ports)
		_ = pm.GlobalProcessManager.Kill(devTunInfo.HostCmdID)

		hostToken := devTunInfo.HostToken
		if hostToken == "" {
			hostToken, err = SDKGetHostToken(ctx, tunnelName)
			if err != nil {
				return fmt.Errorf("devtunnel forward: get host token for %q: %w", tunnelName, err)
			}
		}

		cmdID, _, err := CLIHostTunnel(devTunInfo.TunnelID, hostToken, devTunInfo.Ports)
		if err != nil {
			return fmt.Errorf("devtunnel forward: restart host for %q: %w", tunnelName, err)
		}
		devTunInfo.HostCmdID = cmdID
	}

	log.Printf("devtunnel forward: port %d now forwarded on %q", port, tunnelName)
	return nil
}

// DevTunnelDelete deletes the tunnel identified by tunnelName via the SDK.
func DevTunnelDelete(tunnelName string, authToken string) error {
	if err := InitSDK(authToken); err != nil {
		return fmt.Errorf("devtunnel delete: init SDK: %w", err)
	}

	ctx := context.Background()
	if err := SDKDeleteTunnel(ctx, tunnelName); err != nil {
		return fmt.Errorf("devtunnel delete %q: %w", tunnelName, err)
	}

	return nil
}

// DevTunnelConnect connects to an existing hosted tunnel, making its forwarded
// ports available on localhost.
func DevTunnelConnect(tunnelID string, accessToken string) (string, error) {
	cmdID, err := CLIConnectTunnel(tunnelID, accessToken)
	if err != nil {
		return "", fmt.Errorf("devtunnel connect %q: %w", tunnelID, err)
	}
	log.Printf("devtunnel connect: connected to tunnel %q (cmd=%s)", tunnelID, cmdID)
	return cmdID, nil
}
