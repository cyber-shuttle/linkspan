package workflow

import (
	"fmt"
	"log"
	"os/exec"
	"strings"

	tunnel "github.com/cyber-shuttle/linkspan/subsystems/tunnel"
	vscode "github.com/cyber-shuttle/linkspan/subsystems/vscode"
	utils "github.com/cyber-shuttle/linkspan/utils"
)

// registerBuiltinActions populates a Registry with all built-in action wrappers.
func registerBuiltinActions(r *Registry) {
	r.Register("vscode.create_session", actionVSCodeCreateSession)
	r.Register("tunnel.devtunnel_create", actionDevTunnelCreate)
	r.Register("tunnel.devtunnel_host", actionDevTunnelHost)
	r.Register("tunnel.devtunnel_delete", actionDevTunnelDelete)
	r.Register("tunnel.frp_proxy_create", actionFrpProxyCreate)
	r.Register("shell.exec", actionShellExec)
}

// --- vscode.create_session ---

func actionVSCodeCreateSession(params map[string]any) (*ActionResult, error) {
	password, _ := params["password"].(string)

	port, err := utils.GetAvailablePort()
	if err != nil {
		return nil, fmt.Errorf("get available port: %w", err)
	}

	sessionID := fmt.Sprintf("wf-%d", port)
	addr := fmt.Sprintf(":%d", port)

	vscode.StartSSHServerForVSCodeConnection(sessionID, addr, password)

	result := ActionResult{
		"session_id": sessionID,
		"bind_port":  port,
	}
	return &result, nil
}

// --- tunnel.devtunnel_create ---

func actionDevTunnelCreate(params map[string]any) (*ActionResult, error) {
	tunnelName, _ := params["tunnel_name"].(string)
	expiration, _ := params["expiration"].(string)
	if expiration == "" {
		expiration = "1d"
	}

	ports := toIntSlice(params["ports"])

	info, err := tunnel.DevTunnelCreate(tunnelName, expiration, ports)
	if err != nil {
		return nil, err
	}

	result := ActionResult{
		"tunnel_id":   info.TunnelID,
		"tunnel_name": info.TunnelName,
	}
	return &result, nil
}

// --- tunnel.devtunnel_host ---

func actionDevTunnelHost(params map[string]any) (*ActionResult, error) {
	tunnelName, _ := params["tunnel_name"].(string)
	createToken, _ := params["create_token"].(bool)

	cmdID, conn, err := tunnel.DevTunnelHost(tunnelName, createToken)
	if err != nil {
		return nil, err
	}

	result := ActionResult{
		"command_id":     cmdID,
		"connection_url": conn.ConnectionURL,
		"token":          conn.Token,
	}
	return &result, nil
}

// --- tunnel.devtunnel_delete ---

func actionDevTunnelDelete(params map[string]any) (*ActionResult, error) {
	tunnelName, _ := params["tunnel_name"].(string)
	if err := tunnel.DevTunnelDelete(tunnelName); err != nil {
		return nil, err
	}
	return &ActionResult{}, nil
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

// --- helpers ---

// toIntSlice converts a param value to []int, handling YAML-decoded []any.
func toIntSlice(v any) []int {
	switch val := v.(type) {
	case []any:
		out := make([]int, 0, len(val))
		for _, elem := range val {
			out = append(out, toInt(elem))
		}
		return out
	case []int:
		return val
	default:
		return nil
	}
}

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
