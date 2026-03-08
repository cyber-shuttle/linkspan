package workflow

import (
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"

	"github.com/cyber-shuttle/linkspan/subsystems/mount"
	tunnel "github.com/cyber-shuttle/linkspan/subsystems/tunnel"
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
}

// --- tunnel.devtunnel_create ---
// Creates a tunnel, hosts the relay, and forwards the server port so the client
// can communicate with linkspan immediately.  Additional ports are added later
// via tunnel.devtunnel_forward.

func actionDevTunnelCreate(params map[string]any) (*ActionResult, error) {
	tunnelName, _ := params["tunnel_name"].(string)
	expiration, _ := params["expiration"].(string)
	if expiration == "" {
		expiration = "1d"
	}
	authToken, _ := params["auth_token"].(string)
	if authToken == "" {
		return nil, fmt.Errorf("tunnel.devtunnel_create: auth_token is required")
	}
	serverPort := toInt(params["server_port"])
	sshPort := toInt(params["ssh_port"])

	conn, err := tunnel.DevTunnelCreate(tunnelName, expiration, authToken, serverPort, sshPort)
	if err != nil {
		return nil, err
	}

	result := ActionResult{
		"tunnel_id":      conn.DevTunnelInfo.QualifiedID(),
		"tunnel_name":    conn.DevTunnelInfo.TunnelName,
		"connection_url": conn.ConnectionURL,
		"token":          conn.Token,
		"ssh_port":       sshPort,
	}
	return &result, nil
}

// --- tunnel.devtunnel_forward ---

func actionDevTunnelForward(params map[string]any) (*ActionResult, error) {
	tunnelName, _ := params["tunnel_name"].(string)
	if tunnelName == "" {
		return nil, fmt.Errorf("tunnel.devtunnel_forward: tunnel_name is required")
	}
	authToken, _ := params["auth_token"].(string)
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
	tunnelName, _ := params["tunnel_name"].(string)
	authToken, _ := params["auth_token"].(string)
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
	tunnelID, _ := params["tunnel_id"].(string)
	if tunnelID == "" {
		return nil, fmt.Errorf("tunnel.devtunnel_connect: tunnel_id is required")
	}
	accessToken, _ := params["access_token"].(string)
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
	return &result, nil
}

// --- tunnel.frp_proxy_create ---

func actionFrpProxyCreate(params map[string]any) (*ActionResult, error) {
	tunnelName, _ := params["tunnel_name"].(string)
	port := toInt(params["port"])
	tunnelType, _ := params["tunnel_type"].(string)
	tunnelSecret, _ := params["tunnel_secret"].(string)
	discoveryHost, _ := params["discovery_host"].(string)
	discoveryPort := toInt(params["discovery_port"])
	discoveryToken, _ := params["discovery_token"].(string)

	info, err := tunnel.FrpTunnelProxyCreate(
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
	command, _ := params["command"].(string)
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
	sessionID, _ := params["session_id"].(string)
	if sessionID == "" {
		return nil, fmt.Errorf("mount.setup_overlay: session_id is required")
	}
	localWorkspace, _ := params["local_workspace"].(string)
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

// --- helpers ---

// toInt converts a param value to int, handling YAML's default float64/int types.
func toInt(v any) int {
	switch val := v.(type) {
	case int:
		return val
	case float64:
		return int(val)
	case string:
		// Support template-resolved numeric strings.
		var n int
		fmt.Sscanf(val, "%d", &n)
		return n
	default:
		return 0
	}
}
