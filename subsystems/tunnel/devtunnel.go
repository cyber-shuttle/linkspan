package tunnel

import (
	"context"
	"fmt"
	"log"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

// DevTunnelCreate creates a tunnel on the service (no ports registered yet)
// and registers it with GlobalDevTunnelManager.
func DevTunnelCreate(tunnelName string, expiration string, authToken string) (DevTunnelInfo, error) {
	if err := InitSDK(authToken); err != nil {
		return DevTunnelInfo{}, fmt.Errorf("devtunnel create: init SDK: %w", err)
	}

	ctx := context.Background()
	sdkTunnel, err := SDKCreateTunnel(ctx, tunnelName)
	if err != nil {
		return DevTunnelInfo{}, fmt.Errorf("devtunnel create %q: %w", tunnelName, err)
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

	log.Printf("devtunnel create: tunnel %q ready (id=%s)", tunnelName, sdkTunnel.TunnelID)
	return *info, nil
}

// DevTunnelHost starts hosting the tunnel relay connection.
// No ports are forwarded yet — use DevTunnelForward to add port forwarding.
func DevTunnelHost(tunnelName string, authToken string) (string, DevTunnelConnection, error) {
	if err := InitSDK(authToken); err != nil {
		return "", DevTunnelConnection{}, fmt.Errorf("devtunnel host: init SDK: %w", err)
	}

	devTunInfo, err := GlobalDevTunnelManager.Find(tunnelName)
	if err != nil {
		return "", DevTunnelConnection{}, fmt.Errorf("devtunnel host: tunnel %q not registered: %w", tunnelName, err)
	}

	ctx := context.Background()

	hostToken, err := SDKGetHostToken(ctx, tunnelName)
	if err != nil {
		return "", DevTunnelConnection{}, fmt.Errorf("devtunnel host: get host token for %q: %w", tunnelName, err)
	}

	// Host without ports — all port forwarding is done later via DevTunnelForward.
	cmdID, connectionURL, err := CLIHostTunnel(devTunInfo.TunnelID, hostToken, nil)
	if err != nil {
		return "", DevTunnelConnection{}, fmt.Errorf("devtunnel host: start CLI for %q: %w", tunnelName, err)
	}

	// Track the host process command ID so DevTunnelForward can restart it.
	devTunInfo.HostCmdID = cmdID
	devTunInfo.HostToken = hostToken

	conn := DevTunnelConnection{
		ConnectionURL: connectionURL,
		DevTunnelInfo: devTunInfo,
	}

	connectToken, tokenErr := SDKGetConnectToken(ctx, tunnelName)
	if tokenErr != nil {
		log.Printf("devtunnel host: warning — could not obtain connect token for %q: %v", tunnelName, tokenErr)
	} else {
		conn.Token = connectToken
	}

	return cmdID, conn, nil
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
			// Re-fetch if not cached (shouldn't happen in normal flow).
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
