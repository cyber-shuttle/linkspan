package tunnel

import (
	"context"
	"fmt"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

// DevTunnelProvider implements TunnelProvider using the Microsoft Dev Tunnels
// SDK and CLI binary.
type DevTunnelProvider struct{}

func (d *DevTunnelProvider) Create(ctx context.Context, opts CreateOpts) (*TunnelResult, error) {
	if opts.AuthToken == "" {
		return nil, fmt.Errorf("devtunnel: auth_token is required")
	}

	// Extract individual port roles from the Ports slice.
	var serverPort, sshPort int
	var extraPorts []int
	for i, p := range opts.Ports {
		switch i {
		case 0:
			serverPort = p
		case 1:
			sshPort = p
		default:
			extraPorts = append(extraPorts, p)
		}
	}

	conn, err := DevTunnelCreate(opts.Name, opts.Expiration, opts.AuthToken, serverPort, sshPort, extraPorts...)
	if err != nil {
		return nil, err
	}

	return &TunnelResult{
		TunnelID:      conn.DevTunnelInfo.QualifiedID(),
		ConnectToken:  conn.Token,
		ConnectionURL: conn.ConnectionURL,
		Ports:         conn.DevTunnelInfo.Ports,
	}, nil
}

func (d *DevTunnelProvider) AddPort(ctx context.Context, tunnelID string, port int) error {
	info, err := GlobalDevTunnelManager.Find(tunnelID)
	if err != nil {
		// tunnelID might be a qualified ID -- try using it directly as the tunnel name
		return DevTunnelForward(tunnelID, port, "")
	}
	return DevTunnelForward(info.TunnelName, port, info.AuthToken)
}

func (d *DevTunnelProvider) Connect(ctx context.Context, tunnelID string, token string) (*ConnectResult, error) {
	cmdID, portMap, err := DevTunnelConnect(tunnelID, token)
	if err != nil {
		return nil, err
	}
	return &ConnectResult{
		ConnectionID: cmdID,
		PortMap:      portMap,
	}, nil
}

func (d *DevTunnelProvider) Disconnect(ctx context.Context, connectionID string) error {
	return pm.GlobalProcessManager.Kill(connectionID)
}

func (d *DevTunnelProvider) Delete(ctx context.Context, tunnelID string) error {
	info, err := GlobalDevTunnelManager.Find(tunnelID)
	if err != nil {
		// Try using tunnelID as the name directly
		return DevTunnelDelete(tunnelID, "")
	}

	// Kill the host CLI process
	if info.HostCmdID != "" {
		_ = pm.GlobalProcessManager.Kill(info.HostCmdID)
	}

	if err := DevTunnelDelete(info.TunnelName, info.AuthToken); err != nil {
		return err
	}
	GlobalDevTunnelManager.Remove(info.TunnelName)
	return nil
}

func (d *DevTunnelProvider) List(ctx context.Context) ([]TunnelInfo, error) {
	all, err := GlobalDevTunnelManager.GetAll()
	if err != nil {
		return nil, err
	}
	var out []TunnelInfo
	for _, t := range all {
		out = append(out, TunnelInfo{
			TunnelID: t.QualifiedID(),
			Provider: "devtunnel",
			Ports:    t.Ports,
		})
	}
	return out, nil
}
