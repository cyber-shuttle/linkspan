package tunnel

import (
	"context"
	"fmt"
	"log"
)

// DevTunnelCreate creates a tunnel with the given ports via the SDK and registers
// it with GlobalDevTunnelManager.  tunnelName is used as the tunnel ID (the API
// does not support custom display names).  authToken is a Microsoft Entra ID
// (Azure AD) bearer token used to authenticate against the tunnel service.
func DevTunnelCreate(tunnelName string, expiration string, ports []int, authToken string) (DevTunnelInfo, error) {
	if err := InitSDK(authToken); err != nil {
		return DevTunnelInfo{}, fmt.Errorf("devtunnel create: init SDK: %w", err)
	}

	ctx := context.Background()
	sdkTunnel, err := SDKCreateTunnel(ctx, tunnelName, ports)
	if err != nil {
		return DevTunnelInfo{}, fmt.Errorf("devtunnel create %q: %w", tunnelName, err)
	}

	info := &DevTunnelInfo{
		TunnelID:   sdkTunnel.TunnelID,
		TunnelName: tunnelName,
		Ports:      ports,
	}

	if _, err := GlobalDevTunnelManager.Register(info); err != nil {
		log.Printf("devtunnel create: warning — failed to register %q in manager: %v", tunnelName, err)
	}

	log.Printf("devtunnel create: tunnel %q ready (id=%s)", tunnelName, sdkTunnel.TunnelID)
	return *info, nil
}

// DevTunnelHost starts hosting the tunnel identified by tunnelName.  It obtains a
// host-scoped access token from the SDK and passes it to the devtunnel CLI binary
// (auto-downloaded if absent) via --access-token so no interactive CLI login is
// required.  A connect-scoped token is also fetched and included in the returned
// DevTunnelConnection.
func DevTunnelHost(tunnelName string, authToken string) (string, DevTunnelConnection, error) {
	if err := InitSDK(authToken); err != nil {
		return "", DevTunnelConnection{}, fmt.Errorf("devtunnel host: init SDK: %w", err)
	}

	devTunInfo, err := GlobalDevTunnelManager.Find(tunnelName)
	if err != nil {
		return "", DevTunnelConnection{}, fmt.Errorf("devtunnel host: tunnel %q not registered: %w", tunnelName, err)
	}

	ctx := context.Background()

	// Obtain a host token — this is what the devtunnel CLI needs to act as relay host.
	hostToken, err := SDKGetHostToken(ctx, tunnelName)
	if err != nil {
		return "", DevTunnelConnection{}, fmt.Errorf("devtunnel host: get host token for %q: %w", tunnelName, err)
	}

	// The CLI `host` command takes the tunnel ID.
	cmdID, connectionURL, err := CLIHostTunnel(devTunInfo.TunnelID, hostToken)
	if err != nil {
		return "", DevTunnelConnection{}, fmt.Errorf("devtunnel host: start CLI for %q: %w", tunnelName, err)
	}

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
