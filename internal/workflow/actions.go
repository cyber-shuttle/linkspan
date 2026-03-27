package workflow

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"

	"github.com/cyber-shuttle/linkspan/subsystems/mount"
	"github.com/cyber-shuttle/linkspan/subsystems/tunnel"
)

// registerBuiltinActions populates a Registry with all built-in action wrappers.
func registerBuiltinActions(r *Registry) {
	r.Register("tunnel.devtunnel_create", actionDevTunnelCreate)
	r.Register("tunnel.devtunnel_forward", actionDevTunnelForward)
	r.Register("tunnel.devtunnel_delete", actionDevTunnelDelete)
	r.Register("tunnel.devtunnel_connect", actionDevTunnelConnect)
	r.Register("tunnel.frp_proxy_create", actionFrpProxyCreate)
	r.Register("shell.exec", actionShellExec)
	r.Register("mount.setup_overlay", actionSetupOverlay)

	// Provider-agnostic tunnel actions
	r.Register("tunnel.create", actionTunnelCreate)
	r.Register("tunnel.add_port", actionTunnelAddPort)
	r.Register("tunnel.connect", actionTunnelConnect)
	r.Register("tunnel.disconnect", actionTunnelDisconnect)
	r.Register("tunnel.delete", actionTunnelDelete)
}

// --- tunnel.devtunnel_create ---
// Creates a tunnel, hosts the relay, and forwards the server port so the client
// can communicate with linkspan immediately.  Additional ports are added later
// via tunnel.devtunnel_forward.

func actionDevTunnelCreate(params map[string]any) (*ActionResult, error) {
	tunnelName := stringParam(params, "tunnel_name")
	expiration := stringParam(params, "expiration")
	if expiration == "" {
		expiration = "1d"
	}
	authToken := stringParam(params, "auth_token")
	if authToken == "" {
		return nil, fmt.Errorf("tunnel.devtunnel_create: auth_token is required")
	}

	var ports []int
	if openPorts, ok := params["open_ports"].([]any); ok {
		for _, p := range openPorts {
			ports = append(ports, toInt(p))
		}
	}

	conn, err := tunnel.DevTunnelCreate(tunnelName, expiration, authToken, ports...)
	if err != nil {
		return nil, err
	}

	result := ActionResult{
		"tunnel_id":      conn.DevTunnelInfo.QualifiedID(),
		"tunnel_name":    conn.DevTunnelInfo.TunnelName,
		"connection_url": conn.ConnectionURL,
		"token":          conn.Token,
	}
	return &result, nil
}

// --- tunnel.devtunnel_forward ---

func actionDevTunnelForward(params map[string]any) (*ActionResult, error) {
	tunnelName := stringParam(params, "tunnel_name")
	if tunnelName == "" {
		return nil, fmt.Errorf("tunnel.devtunnel_forward: tunnel_name is required")
	}
	authToken := stringParam(params, "auth_token")
	if authToken == "" {
		return nil, fmt.Errorf("tunnel.devtunnel_forward: auth_token is required")
	}
	port := toInt(params["port"])
	if port == 0 {
		return nil, fmt.Errorf("tunnel.devtunnel_forward: port is required")
	}

	if err := tunnel.DevTunnelForward(tunnelName, port, authToken); err != nil {
		return nil, err
	}

	return &ActionResult{"port": port}, nil
}

// --- tunnel.devtunnel_delete ---

func actionDevTunnelDelete(params map[string]any) (*ActionResult, error) {
	tunnelName := stringParam(params, "tunnel_name")
	authToken := stringParam(params, "auth_token")
	if authToken == "" {
		return nil, fmt.Errorf("tunnel.devtunnel_delete: auth_token is required")
	}

	if err := tunnel.DevTunnelDelete(tunnelName, authToken); err != nil {
		return nil, err
	}
	return &ActionResult{}, nil
}

// --- tunnel.devtunnel_connect ---

func actionDevTunnelConnect(params map[string]any) (*ActionResult, error) {
	tunnelID := stringParam(params, "tunnel_id")
	if tunnelID == "" {
		return nil, fmt.Errorf("tunnel.devtunnel_connect: tunnel_id is required")
	}
	accessToken := stringParam(params, "access_token")
	if accessToken == "" {
		return nil, fmt.Errorf("tunnel.devtunnel_connect: access_token is required")
	}

	cmdID, portMap, err := tunnel.DevTunnelConnect(tunnelID, accessToken)
	if err != nil {
		return nil, err
	}

	// Convert port map to string-keyed map for template access
	portMapStr := make(map[string]any)
	for remote, local := range portMap {
		portMapStr[strconv.Itoa(remote)] = local
	}

	result := ActionResult{
		"command_id": cmdID,
		"port_map":   portMapStr,
	}

	// If ssh_port was provided, resolve the mapped local port for the overlay
	if sshPort := toInt(params["ssh_port"]); sshPort != 0 {
		if mapped, ok := portMap[sshPort]; ok {
			result["mapped_ssh_port"] = mapped
			log.Printf("[tunnel.devtunnel_connect] mapped SSH port %d -> %d", sshPort, mapped)
		} else {
			log.Printf("[tunnel.devtunnel_connect] warning: SSH port %d not found in port map", sshPort)
			result["mapped_ssh_port"] = sshPort // fallback to original
		}
	}

	return &result, nil
}

// --- tunnel.frp_proxy_create ---

func actionFrpProxyCreate(params map[string]any) (*ActionResult, error) {
	tunnelName := stringParam(params, "tunnel_name")
	port := toInt(params["port"])
	tunnelType := stringParam(params, "tunnel_type")
	tunnelSecret := stringParam(params, "tunnel_secret")
	discoveryHost := stringParam(params, "discovery_host")
	discoveryPort := toInt(params["discovery_port"])
	discoveryToken := stringParam(params, "discovery_token")

	info, err := tunnel.FRPTunnelProxyCreate(
		tunnelName, port, tunnelType, tunnelSecret,
		discoveryHost, discoveryPort, discoveryToken,
	)
	if err != nil {
		return nil, err
	}

	result := ActionResult{
		"tunnel_name": info.TunnelName,
		"tunnel_type": info.TunnelType,
	}
	return &result, nil
}

// --- shell.exec ---

func actionShellExec(params map[string]any) (*ActionResult, error) {
	command := stringParam(params, "command")
	if command == "" {
		return nil, fmt.Errorf("shell.exec: command is required")
	}

	parts := strings.Fields(command)
	cmd := exec.Command(parts[0], parts[1:]...)

	output, err := cmd.CombinedOutput()
	log.Printf("shell.exec: %s\n%s", command, string(output))
	if err != nil {
		return nil, fmt.Errorf("shell.exec %q: %w\n%s", command, err, string(output))
	}

	result := ActionResult{
		"output": strings.TrimSpace(string(output)),
	}
	return &result, nil
}

// --- mount.setup_overlay ---

func actionSetupOverlay(params map[string]any) (*ActionResult, error) {
	sessionID := stringParam(params, "session_id")
	if sessionID == "" {
		return nil, fmt.Errorf("mount.setup_overlay: session_id is required")
	}
	localWorkspace := stringParam(params, "local_workspace")
	if localWorkspace == "" {
		return nil, fmt.Errorf("mount.setup_overlay: local_workspace is required")
	}
	localSshPort := toInt(params["local_ssh_port"])
	if localSshPort == 0 {
		return nil, fmt.Errorf("mount.setup_overlay: local_ssh_port is required")
	}

	overlay, err := mount.SetupOverlay(sessionID, localSshPort, localWorkspace)
	if err != nil {
		return nil, fmt.Errorf("mount.setup_overlay: %w", err)
	}

	result := ActionResult{
		"merged_path": overlay.MergedDir,
		"cache_path":  overlay.CacheDir,
		"source_path": overlay.SourceDir,
	}
	return &result, nil
}

// --- tunnel.create (provider-agnostic) ---

func actionTunnelCreate(params map[string]any) (*ActionResult, error) {
	providerName := stringParam(params, "provider")
	if providerName == "" {
		providerName = "devtunnel"
	}
	p, err := tunnel.GetProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("tunnel.create: %w", err)
	}

	var ports []int
	if openPorts, ok := params["open_ports"].([]any); ok {
		for _, p := range openPorts {
			ports = append(ports, toInt(p))
		}
	}

	opts := tunnel.CreateOpts{
		Name:       stringParam(params, "tunnel_name"),
		AuthToken:  stringParam(params, "auth_token"),
		Ports:      ports,
		Expiration: stringParam(params, "expiration"),
		ServerURL:  stringParam(params, "server_url"),
	}
	if opts.Expiration == "" {
		opts.Expiration = "1d"
	}

	result, err := p.Create(context.Background(), opts)
	if err != nil {
		return nil, fmt.Errorf("tunnel.create: %w", err)
	}

	return &ActionResult{
		"tunnel_id":      result.TunnelID,
		"connection_url": result.ConnectionURL,
		"token":          result.ConnectToken,
		"ssh_port":       toInt(params["ssh_port"]),
		"log_port":       toInt(params["log_port"]),
	}, nil
}

// --- tunnel.add_port (provider-agnostic) ---

func actionTunnelAddPort(params map[string]any) (*ActionResult, error) {
	providerName := stringParam(params, "provider")
	if providerName == "" {
		providerName = "devtunnel"
	}
	p, err := tunnel.GetProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("tunnel.add_port: %w", err)
	}

	tunnelID := stringParam(params, "tunnel_id")
	port := toInt(params["port"])
	if port == 0 {
		return nil, fmt.Errorf("tunnel.add_port: port is required")
	}

	if err := p.AddPort(context.Background(), tunnelID, port); err != nil {
		return nil, fmt.Errorf("tunnel.add_port: %w", err)
	}
	return &ActionResult{"port": port}, nil
}

// --- tunnel.connect (provider-agnostic) ---

func actionTunnelConnect(params map[string]any) (*ActionResult, error) {
	providerName := stringParam(params, "provider")
	if providerName == "" {
		providerName = "devtunnel"
	}
	p, err := tunnel.GetProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("tunnel.connect: %w", err)
	}

	tunnelID := stringParam(params, "tunnel_id")
	if tunnelID == "" {
		return nil, fmt.Errorf("tunnel.connect: tunnel_id is required")
	}
	accessToken := stringParam(params, "access_token")
	if accessToken == "" {
		return nil, fmt.Errorf("tunnel.connect: access_token is required")
	}

	cr, err := p.Connect(context.Background(), tunnelID, accessToken)
	if err != nil {
		return nil, fmt.Errorf("tunnel.connect: %w", err)
	}

	tunnel.TrackConnection(cr.ConnectionID, providerName)

	portMapStr := make(map[string]any, len(cr.PortMap))
	for remote, local := range cr.PortMap {
		portMapStr[strconv.Itoa(remote)] = local
	}

	result := ActionResult{
		"connection_id": cr.ConnectionID,
		"port_map":      portMapStr,
	}

	// If ssh_port was provided, resolve the mapped local port for the overlay
	if sshPort := toInt(params["ssh_port"]); sshPort != 0 {
		if mapped, ok := cr.PortMap[sshPort]; ok {
			result["mapped_ssh_port"] = mapped
			log.Printf("[tunnel.connect] mapped SSH port %d -> %d", sshPort, mapped)
		} else {
			log.Printf("[tunnel.connect] warning: SSH port %d not found in port map", sshPort)
			result["mapped_ssh_port"] = sshPort
		}
	}

	return &result, nil
}

// --- tunnel.disconnect (provider-agnostic) ---

func actionTunnelDisconnect(params map[string]any) (*ActionResult, error) {
	connID := stringParam(params, "connection_id")
	if connID == "" {
		return nil, fmt.Errorf("tunnel.disconnect: connection_id is required")
	}

	providerName, ok := tunnel.ConnectionProvider(connID)
	if !ok {
		return nil, fmt.Errorf("tunnel.disconnect: unknown connection %s", connID)
	}
	p, err := tunnel.GetProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("tunnel.disconnect: %w", err)
	}

	if err := p.Disconnect(context.Background(), connID); err != nil {
		return nil, fmt.Errorf("tunnel.disconnect: %w", err)
	}
	tunnel.UntrackConnection(connID)
	return &ActionResult{}, nil
}

// --- tunnel.delete (provider-agnostic) ---

func actionTunnelDelete(params map[string]any) (*ActionResult, error) {
	providerName := stringParam(params, "provider")
	if providerName == "" {
		providerName = "devtunnel"
	}
	p, err := tunnel.GetProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("tunnel.delete: %w", err)
	}

	tunnelID := stringParam(params, "tunnel_id")
	if tunnelID == "" {
		return nil, fmt.Errorf("tunnel.delete: tunnel_id is required")
	}

	if err := p.Delete(context.Background(), tunnelID); err != nil {
		return nil, fmt.Errorf("tunnel.delete: %w", err)
	}
	return &ActionResult{}, nil
}

func stringParam(params map[string]any, key string) string {
	v, _ := params[key].(string)
	return v
}

// --- helpers ---

// toInt converts a param value to int, handling YAML's default float64/int types.
func toInt(v any) int {
	switch val := v.(type) {
	case int:
		return val
	case float64:
		return int(val)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}
