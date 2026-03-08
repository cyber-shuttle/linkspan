package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fatedier/frp/client"
	v1 "github.com/fatedier/frp/pkg/config/v1"
)

// FRPTunnelProvider implements TunnelProvider using an external frp-server.
type FRPTunnelProvider struct {
	mu          sync.Mutex
	tunnels     map[string]*frpTunnelState
	connections map[string]*frpConnectionState
}

type frpTunnelState struct {
	TunnelID     string
	HostToken    string
	ConnectToken string
	ServerURL    string // e.g. "frp.example.com:7000"
	APIBaseURL   string // e.g. "http://frp.example.com:7500"
	APIKey       string // API key for frp-server
	Ports        []int
	HostService  *client.Service
	HostCancel   context.CancelFunc
}

type frpConnectionState struct {
	TunnelID string
	Service  *client.Service
	Cancel   context.CancelFunc
	PortMap  map[int]int
}

// NewFRPTunnelProvider creates a new FRP-based tunnel provider.
func NewFRPTunnelProvider() *FRPTunnelProvider {
	return &FRPTunnelProvider{
		tunnels:     make(map[string]*frpTunnelState),
		connections: make(map[string]*frpConnectionState),
	}
}

func (f *FRPTunnelProvider) Create(ctx context.Context, opts CreateOpts) (*TunnelResult, error) {
	if opts.ServerURL == "" {
		return nil, fmt.Errorf("frp: server_url is required")
	}
	if opts.AuthToken == "" {
		return nil, fmt.Errorf("frp: auth_token (API key) is required")
	}

	apiBase := deriveAPIBase(opts.ServerURL)

	// 1. Create tunnel on frp-server
	createResp, err := frpAPICall[map[string]string](ctx, apiBase, "POST", "/api/v1/tunnels",
		map[string]string{"expiration": opts.Expiration}, opts.AuthToken)
	if err != nil {
		return nil, fmt.Errorf("frp: create tunnel: %w", err)
	}

	tunnelID := (*createResp)["tunnelId"]
	hostToken := (*createResp)["hostToken"]
	connectToken := (*createResp)["connectToken"]

	// 2. Register ports
	for _, port := range opts.Ports {
		if port > 0 {
			_, err := frpAPICall[any](ctx, apiBase, "POST",
				fmt.Sprintf("/api/v1/tunnels/%s/ports", tunnelID),
				map[string]any{"port": port, "protocol": "tcp"}, opts.AuthToken)
			if err != nil {
				return nil, fmt.Errorf("frp: add port %d: %w", port, err)
			}
		}
	}

	// 3. Start FRP client (host mode) — register proxies for each port
	hostService, hostCancel, err := f.startHostClient(opts.ServerURL, hostToken, tunnelID, opts.Ports)
	if err != nil {
		return nil, fmt.Errorf("frp: start host client: %w", err)
	}

	state := &frpTunnelState{
		TunnelID:     tunnelID,
		HostToken:    hostToken,
		ConnectToken: connectToken,
		ServerURL:    opts.ServerURL,
		APIBaseURL:   apiBase,
		APIKey:       opts.AuthToken,
		Ports:        opts.Ports,
		HostService:  hostService,
		HostCancel:   hostCancel,
	}

	f.mu.Lock()
	f.tunnels[tunnelID] = state
	f.mu.Unlock()

	return &TunnelResult{
		TunnelID:      tunnelID,
		ConnectToken:  connectToken,
		ConnectionURL: fmt.Sprintf("frp://%s/%s", opts.ServerURL, tunnelID),
		Ports:         opts.Ports,
	}, nil
}

func (f *FRPTunnelProvider) AddPort(ctx context.Context, tunnelID string, port int) error {
	f.mu.Lock()
	state, ok := f.tunnels[tunnelID]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("frp: tunnel %s not found", tunnelID)
	}

	_, err := frpAPICall[any](ctx, state.APIBaseURL, "POST",
		fmt.Sprintf("/api/v1/tunnels/%s/ports", tunnelID),
		map[string]any{"port": port, "protocol": "tcp"}, state.APIKey)
	if err != nil {
		return fmt.Errorf("frp: add port %d: %w", port, err)
	}

	// Restart host client with updated ports
	f.mu.Lock()
	state.Ports = append(state.Ports, port)
	if state.HostCancel != nil {
		state.HostCancel()
	}
	f.mu.Unlock()

	hostService, hostCancel, err := f.startHostClient(state.ServerURL, state.HostToken, tunnelID, state.Ports)
	if err != nil {
		return fmt.Errorf("frp: restart host client: %w", err)
	}

	f.mu.Lock()
	state.HostService = hostService
	state.HostCancel = hostCancel
	f.mu.Unlock()

	return nil
}

func (f *FRPTunnelProvider) Connect(ctx context.Context, tunnelID string, token string) (*ConnectResult, error) {
	// Determine server URL from stored tunnels
	var serverURL string
	f.mu.Lock()
	if state, ok := f.tunnels[tunnelID]; ok {
		serverURL = state.ServerURL
	}
	f.mu.Unlock()

	if serverURL == "" {
		return nil, fmt.Errorf("frp: cannot determine server URL for tunnel %s (tunnel not found locally)", tunnelID)
	}

	// Get port list from server
	apiBase := deriveAPIBase(serverURL)
	tunnelResp, err := frpAPICall[map[string]any](ctx, apiBase, "GET",
		fmt.Sprintf("/api/v1/tunnels/%s", tunnelID), nil, token)
	if err != nil {
		return nil, fmt.Errorf("frp: get tunnel info: %w", err)
	}

	portsRaw, _ := (*tunnelResp)["ports"].([]any)
	var remotePorts []int
	for _, p := range portsRaw {
		if pm, ok := p.(map[string]any); ok {
			if port, ok := pm["port"].(float64); ok {
				remotePorts = append(remotePorts, int(port))
			}
		}
	}

	// Allocate local ports for each remote port
	portMap := make(map[int]int)
	for _, remotePort := range remotePorts {
		localPort, err := getAvailablePort()
		if err != nil {
			return nil, fmt.Errorf("frp: get available port: %w", err)
		}
		portMap[remotePort] = localPort
	}

	// Start FRP visitors with XTCP-first, TCP fallback
	connService, connCancel, err := f.startVisitorClient(serverURL, token, tunnelID, portMap)
	if err != nil {
		return nil, fmt.Errorf("frp: start visitor client: %w", err)
	}

	connID := fmt.Sprintf("frp-conn-%d", time.Now().UnixNano())

	f.mu.Lock()
	f.connections[connID] = &frpConnectionState{
		TunnelID: tunnelID,
		Service:  connService,
		Cancel:   connCancel,
		PortMap:  portMap,
	}
	f.mu.Unlock()

	return &ConnectResult{
		ConnectionID: connID,
		PortMap:      portMap,
	}, nil
}

func (f *FRPTunnelProvider) Disconnect(_ context.Context, connectionID string) error {
	f.mu.Lock()
	conn, ok := f.connections[connectionID]
	if ok {
		delete(f.connections, connectionID)
	}
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("frp: connection %s not found", connectionID)
	}
	if conn.Cancel != nil {
		conn.Cancel()
	}
	return nil
}

func (f *FRPTunnelProvider) Delete(ctx context.Context, tunnelID string) error {
	f.mu.Lock()
	state, ok := f.tunnels[tunnelID]
	if ok {
		delete(f.tunnels, tunnelID)
	}
	f.mu.Unlock()

	if ok {
		if state.HostCancel != nil {
			state.HostCancel()
		}
		// Delete on server (best-effort)
		_, _ = frpAPICall[any](ctx, state.APIBaseURL, "DELETE",
			fmt.Sprintf("/api/v1/tunnels/%s", tunnelID), nil, state.APIKey)
	}
	return nil
}

func (f *FRPTunnelProvider) List(_ context.Context) ([]TunnelInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []TunnelInfo
	for _, t := range f.tunnels {
		out = append(out, TunnelInfo{
			TunnelID: t.TunnelID,
			Provider: "frp",
			Ports:    t.Ports,
		})
	}
	return out, nil
}

// --- FRP Client Helpers ---

func (f *FRPTunnelProvider) startHostClient(serverURL, authToken, tunnelID string, ports []int) (*client.Service, context.CancelFunc, error) {
	host, port := parseHostPort(serverURL, 7000)

	cfg := &v1.ClientCommonConfig{}
	cfg.ServerAddr = host
	cfg.ServerPort = port
	cfg.Auth.Method = v1.AuthMethodToken
	cfg.Auth.Token = authToken
	cfg.Transport.Protocol = "tcp"
	cfg.Log.To = "console"
	cfg.Log.Level = "info"

	// Create one XTCP proxy + one TCP proxy (fallback) per port
	var proxyCfgs []v1.ProxyConfigurer
	for _, p := range ports {
		if p <= 0 {
			continue
		}
		proxyName := fmt.Sprintf("%s-%d", tunnelID, p)

		// XTCP proxy (P2P preferred)
		xtcp := &v1.XTCPProxyConfig{}
		xtcp.Name = proxyName
		xtcp.Type = "xtcp"
		xtcp.LocalIP = "127.0.0.1"
		xtcp.LocalPort = p
		xtcp.Secretkey = authToken
		xtcp.Transport.BandwidthLimitMode = "client"
		proxyCfgs = append(proxyCfgs, xtcp)

		// TCP proxy as fallback
		tcp := &v1.TCPProxyConfig{}
		tcp.Name = proxyName + "-tcp"
		tcp.Type = "tcp"
		tcp.LocalIP = "127.0.0.1"
		tcp.LocalPort = p
		tcp.RemotePort = 0 // Let server assign
		tcp.Transport.BandwidthLimitMode = "client"
		proxyCfgs = append(proxyCfgs, tcp)
	}

	visitorCfgs := []v1.VisitorConfigurer{}

	svc, err := client.NewService(client.ServiceOptions{
		Common:      cfg,
		ProxyCfgs:   proxyCfgs,
		VisitorCfgs: visitorCfgs,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create host client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := svc.Run(ctx); err != nil {
			log.Printf("frp host client error: %v", err)
		}
	}()

	time.Sleep(2 * time.Second)

	return svc, cancel, nil
}

func (f *FRPTunnelProvider) startVisitorClient(serverURL, authToken, tunnelID string, portMap map[int]int) (*client.Service, context.CancelFunc, error) {
	host, port := parseHostPort(serverURL, 7000)

	cfg := &v1.ClientCommonConfig{}
	cfg.ServerAddr = host
	cfg.ServerPort = port
	cfg.Auth.Method = v1.AuthMethodToken
	cfg.Auth.Token = authToken
	cfg.Transport.Protocol = "tcp"
	cfg.Log.To = "console"
	cfg.Log.Level = "info"

	proxyCfgs := []v1.ProxyConfigurer{}

	// Create XTCP visitors with TCP fallback per port
	var visitorCfgs []v1.VisitorConfigurer
	for remotePort, localPort := range portMap {
		proxyName := fmt.Sprintf("%s-%d", tunnelID, remotePort)

		xtcpVisitor := &v1.XTCPVisitorConfig{}
		xtcpVisitor.Name = proxyName + "-visitor"
		xtcpVisitor.Type = "xtcp"
		xtcpVisitor.ServerName = proxyName
		xtcpVisitor.SecretKey = authToken
		xtcpVisitor.BindAddr = "127.0.0.1"
		xtcpVisitor.BindPort = localPort
		xtcpVisitor.FallbackTo = proxyName + "-tcp"
		xtcpVisitor.FallbackTimeoutMs = 5000

		visitorCfgs = append(visitorCfgs, xtcpVisitor)
	}

	svc, err := client.NewService(client.ServiceOptions{
		Common:      cfg,
		ProxyCfgs:   proxyCfgs,
		VisitorCfgs: visitorCfgs,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create visitor client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := svc.Run(ctx); err != nil {
			log.Printf("frp visitor client error: %v", err)
		}
	}()

	time.Sleep(2 * time.Second)

	return svc, cancel, nil
}

// --- Utility Helpers ---

func deriveAPIBase(serverURL string) string {
	host, _, _ := net.SplitHostPort(serverURL)
	if host == "" {
		host = serverURL
	}
	return fmt.Sprintf("http://%s:7500", host)
}

func parseHostPort(addr string, defaultPort int) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, defaultPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, defaultPort
	}
	return host, port
}

func getAvailablePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

func frpAPICall[T any](ctx context.Context, baseURL, method, path string, body any, authToken string) (*T, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}

	var result T
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil // Some endpoints return no body on success
	}
	return &result, nil
}
