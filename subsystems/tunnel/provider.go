package tunnel

import "context"

// TunnelProvider abstracts tunnel lifecycle operations so multiple backends
// (Dev Tunnels, FRP) can be used interchangeably.
type TunnelProvider interface {
	// Create creates a tunnel and returns its metadata.
	Create(ctx context.Context, opts CreateOpts) (*TunnelResult, error)
	// AddPort registers an additional port for forwarding.
	AddPort(ctx context.Context, tunnelID string, port int) error
	// Connect connects to a remote tunnel and returns local port mappings.
	Connect(ctx context.Context, tunnelID string, token string) (*ConnectResult, error)
	// Disconnect tears down a connection established by Connect.
	Disconnect(ctx context.Context, connectionID string) error
	// Delete tears down a tunnel and all its resources.
	Delete(ctx context.Context, tunnelID string) error
	// List returns all active tunnels for this provider.
	List(ctx context.Context) ([]TunnelInfo, error)
}

// CreateOpts configures tunnel creation.
type CreateOpts struct {
	Name       string
	AuthToken  string // API key (FRP) or AAD token (devtunnel)
	Ports      []int  // ports to register immediately
	Expiration string // e.g. "1d"
	ServerURL  string // FRP server URL (ignored by devtunnel)
}

// TunnelResult is returned after successful tunnel creation.
type TunnelResult struct {
	TunnelID      string
	ConnectToken  string
	ConnectionURL string
	Ports         []int
}

// ConnectResult holds the outcome of connecting to a remote tunnel.
type ConnectResult struct {
	ConnectionID string      // opaque ID for later Disconnect
	PortMap      map[int]int // remotePort -> localPort
}

// TunnelInfo describes an active tunnel.
type TunnelInfo struct {
	TunnelID string `json:"tunnelId"`
	Provider string `json:"provider"` // "devtunnel" or "frp"
	Ports    []int  `json:"ports"`
}
