package tunnel

import (
	"context"
	"fmt"
	"log"

	tunnels "github.com/microsoft/dev-tunnels/go/tunnels"
)

// DevTunnelSetup creates a tunnel (or resolves a client-created one in clusterID when external is
// true) and hosts the relay, forwarding the given ports. An external tunnel is hosted but never
// deleted.
func DevTunnelSetup(tunnelName string, expiration string, authToken string, external bool, clusterID string, portsToOpen ...int) (DevTunnelConnection, error) {
	log.Printf("devtunnel setup: tunnel %q (external=%v, cluster=%q) with expiration %q and ports %v", tunnelName, external, clusterID, expiration, portsToOpen)
	if err := InitSDK(authToken); err != nil {
		return DevTunnelConnection{}, fmt.Errorf("devtunnel setup: init SDK: %w", err)
	}

	ctx := context.Background()

	// 1. Resolve the client-created tunnel (in its cluster), or create our own.
	resolve := func() (*tunnels.Tunnel, error) {
		if external {
			return SDKResolveTunnel(ctx, tunnelName, clusterID)
		}
		return SDKCreateTunnel(ctx, tunnelName)
	}
	sdkTunnel, err := resolve()
	if err != nil {
		return DevTunnelConnection{}, fmt.Errorf("devtunnel setup %q: %w", tunnelName, err)
	}

	info := &DevTunnelInfo{
		TunnelID:   sdkTunnel.TunnelID,
		ClusterID:  sdkTunnel.ClusterID,
		TunnelName: tunnelName,
		AuthToken:  authToken,
		External:   external,
	}

	if _, err := GlobalDevTunnelManager.Register(info); err != nil {
		log.Printf("devtunnel setup: warning — failed to register %q in manager: %v", tunnelName, err)
	}

	// 2c. Register any extra ports (e.g. log stream).
	for _, p := range portsToOpen {
		if p > 0 {
			if err := SDKAddPort(ctx, tunnelName, p); err != nil {
				return DevTunnelConnection{}, fmt.Errorf("devtunnel setup: add extra ports %d to %q: %w", p, tunnelName, err)
			}
			info.Ports = append(info.Ports, p)
		}
	}

	// 3. Obtain host token and start the relay.
	// Ports are already registered via SDK above — the CLI doesn't need -p flags
	// (which would require manage scope the host token doesn't have).
	// The relay forwards SDK-registered ports automatically.
	hostToken, err := SDKGetHostToken(ctx, tunnelName)
	if err != nil {
		return DevTunnelConnection{}, fmt.Errorf("devtunnel setup: get host token for %q: %w", tunnelName, err)
	}

	// Host by cluster-qualified id so the CLI targets the tunnel's cluster directly; a bare id
	// makes it search the default cluster and fail with "Login required" for a tunnel created
	// elsewhere (e.g. by the client).
	cmdID, connectionURL, err := CLIHostTunnel(info.QualifiedID(), hostToken)
	if err != nil {
		return DevTunnelConnection{}, fmt.Errorf("devtunnel setup: start host for %q: %w", tunnelName, err)
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
		log.Printf("devtunnel setup: warning — could not obtain connect token for %q: %v", tunnelName, tokenErr)
	} else {
		conn.Token = connectToken
	}

	log.Printf("devtunnel setup: tunnel %q ready (id=%s, url=%s)", tunnelName, sdkTunnel.TunnelID, connectionURL)
	return conn, nil
}

// DevTunnelForward registers a new port on an existing tunnel. The running host picks it up
// dynamically (the client refreshes ports on connect), so no rehost is needed. Idempotent.
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

	if err := GlobalDevTunnelManager.AddPort(tunnelName, port); err != nil {
		return fmt.Errorf("devtunnel forward: record port: %w", err)
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
func DevTunnelConnect(tunnelID string, accessToken string) (string, map[int]int, error) {
	cmdID, portMap, err := CLIConnectTunnel(tunnelID, accessToken)
	if err != nil {
		return "", nil, fmt.Errorf("devtunnel connect %q: %w", tunnelID, err)
	}
	log.Printf("devtunnel connect: connected to tunnel %q (cmd=%s, ports=%v)", tunnelID, cmdID, portMap)
	return cmdID, portMap, nil
}
