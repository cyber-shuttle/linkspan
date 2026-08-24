package workflow

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
	"github.com/cyber-shuttle/linkspan/subsystems/checkpoint"
	"github.com/cyber-shuttle/linkspan/subsystems/fork"
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

	// Application lifecycle: start a long-running process, checkpoint it, and
	// restore it in a later allocation.
	r.Register("process.start", actionProcessStart)
	r.Register("checkpoint.create", actionCheckpointCreate)
	r.Register("checkpoint.restore", actionCheckpointRestore)

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

	conn, err := tunnel.DevTunnelSetup(tunnelName, expiration, authToken, false, "", ports...)
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

// --- process.start ---
// Launches a long-running application and returns immediately with its id, so
// a later step can checkpoint it. shell.exec cannot serve this: it blocks
// until the command exits, which for a training job is the whole allocation.

func actionProcessStart(params map[string]any) (*ActionResult, error) {
	command := stringParam(params, "command")
	if command == "" {
		return nil, fmt.Errorf("process.start: command is required")
	}

	fp, err := fork.GlobalForkProcessManager.RunForkProcess(command, boolParam(params, "shutdown_on_completion"))
	if err != nil {
		return nil, fmt.Errorf("process.start: %w", err)
	}

	// The pid is what a human reads in the logs; checkpoint.create should be
	// given the process id, which survives linkspan restarting underneath it.
	info, err := pm.GlobalProcessManager.GetInfo(fp.InternalProcessId)
	if err != nil {
		return nil, fmt.Errorf("process.start: started %s but could not read it back: %w", fp.InternalProcessId, err)
	}

	log.Printf("[process.start] %s -> process_id=%s pid=%d", command, fp.InternalProcessId, info.Cmd.Process.Pid)
	return &ActionResult{
		"process_id": fp.InternalProcessId,
		"pid":        info.Cmd.Process.Pid,
		"command":    command,
	}, nil
}

// --- checkpoint.create ---

func actionCheckpointCreate(params map[string]any) (*ActionResult, error) {
	svc, err := checkpoint.ActiveService()
	if err != nil {
		return nil, fmt.Errorf("checkpoint.create: %w", err)
	}

	processID := stringParam(params, "process_id")
	pid := toInt(params["pid"])
	if (processID == "") == (pid == 0) {
		return nil, fmt.Errorf("checkpoint.create: exactly one of process_id and pid is required")
	}
	target := checkpoint.TargetFromProcessID(processID)
	if processID == "" {
		target = checkpoint.TargetFromPID(pid)
	}

	workloadID := stringParam(params, "workload_id")
	if workloadID == "" {
		workloadID = svc.DefaultWorkloadID()
	}

	opts := checkpoint.CreateOptions{
		WorkloadID: workloadID,
		Trigger:    checkpoint.TriggerWorkflow,
		Mode:       checkpoint.CheckpointMode(stringParam(params, "mode")),
		Reason:     stringParam(params, "reason"),
	}
	// Absent from the YAML means "use the trigger's default", which is not the
	// same as false, so the key has to be looked up rather than read.
	if v, ok := params["leave_running"]; ok {
		leave := toBool(v)
		opts.LeaveRunning = &leave
	}

	result, err := svc.CreateCheckpoint(context.Background(), target, opts)
	if err != nil {
		return nil, fmt.Errorf("checkpoint.create: %w", err)
	}

	// Read the state back from the manifest so a workflow and a REST caller
	// report the same status for the same checkpoint.
	status := string(checkpoint.StateComplete)
	if m, err := svc.GetCheckpoint(result.CheckpointID); err == nil {
		status = string(m.State)
	}

	log.Printf("[checkpoint.create] workload=%s checkpoint=%s status=%s", result.WorkloadID, result.CheckpointID, status)
	return &ActionResult{
		"checkpoint_id":   result.CheckpointID,
		"checkpoint_path": result.CheckpointDir,
		"status":          status,
		"workload_id":     result.WorkloadID,
	}, nil
}

// --- checkpoint.restore ---

func actionCheckpointRestore(params map[string]any) (*ActionResult, error) {
	svc, err := checkpoint.ActiveService()
	if err != nil {
		return nil, fmt.Errorf("checkpoint.restore: %w", err)
	}

	checkpointID := stringParam(params, "checkpoint_id")
	if checkpointID == "" {
		return nil, fmt.Errorf("checkpoint.restore: checkpoint_id is required")
	}

	// Start from the flags this allocation was started with, so a workflow
	// only has to name the prerequisites it wants to add or override.
	opts := svc.RestoreDefaults()
	if v, ok := params["force"]; ok {
		opts.Force = toBool(v)
	}
	if v, ok := params["shutdown_on_completion"]; ok {
		opts.ShutdownOnCompletion = toBool(v)
	}
	if cmds := stringSliceParam(params, "pre_restore_commands"); cmds != nil {
		opts.PreRestoreCommands = cmds
	}
	if dirs := stringSliceParam(params, "ensure_dirs"); dirs != nil {
		opts.EnsureDirs = dirs
	}
	if files := stringSliceParam(params, "require_files"); files != nil {
		opts.RequireFiles = files
	}

	result, err := svc.RestoreCheckpoint(context.Background(), checkpointID, opts)
	if err != nil {
		return nil, fmt.Errorf("checkpoint.restore: %w", err)
	}

	// Later steps in this allocation checkpoint the restored workload, not the
	// allocation's own, so its identity becomes the default.
	svc.SetDefaultWorkloadID(result.WorkloadID)

	for _, w := range result.Warnings {
		log.Printf("[checkpoint.restore] warning: %s", w)
	}
	log.Printf("[checkpoint.restore] checkpoint=%s -> process_id=%s pid=%d", checkpointID, result.ProcessID, result.Pid)

	return &ActionResult{
		"process_id":    result.ProcessID,
		"pid":           result.Pid,
		"checkpoint_id": result.CheckpointID,
		"workload_id":   result.WorkloadID,
	}, nil
}

func stringParam(params map[string]any, key string) string {
	v, _ := params[key].(string)
	return v
}

// --- helpers ---

// toBool converts a param value to bool, accepting the strings YAML users
// reach for as readily as a real boolean.
func toBool(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(val))
		if err != nil {
			return false
		}
		return b
	default:
		return false
	}
}

// stringSliceParam reads a YAML list of strings, returning nil when the key is
// absent so a caller can tell "not specified" from "specified as empty".
func stringSliceParam(params map[string]any, key string) []string {
	raw, ok := params[key]
	if !ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// boolParam reads an optional boolean param, defaulting to false.
func boolParam(params map[string]any, key string) bool {
	v, ok := params[key]
	if !ok {
		return false
	}
	return toBool(v)
}

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
