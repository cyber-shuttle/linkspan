package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"

	tunnels "github.com/microsoft/dev-tunnels/go/tunnels"
)

// debugTransport wraps http.RoundTripper to log request/response details for debugging.
type debugTransport struct {
	base http.RoundTripper
}

func (d *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Log request details
	log.Printf("devtunnel sdk HTTP: %s %s", req.Method, req.URL)
	log.Printf("devtunnel sdk HTTP: Authorization: %s...", truncate(req.Header.Get("Authorization"), 40))
	if req.Body != nil {
		bodyBytes, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		log.Printf("devtunnel sdk HTTP: request body: %s", truncate(string(bodyBytes), 500))
	}

	resp, err := d.base.RoundTrip(req)
	if err != nil {
		log.Printf("devtunnel sdk HTTP: transport error: %v", err)
		return resp, err
	}

	// Log response, but also preserve the body for the caller
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	log.Printf("devtunnel sdk HTTP: response %d: %s", resp.StatusCode, truncate(string(respBody), 500))

	return resp, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// generateID produces a tunnel-ID-safe string from a logical name.
// The Dev Tunnels API requires IDs matching [a-z0-9][a-z0-9-]{1,58}[a-z0-9].
func generateID(name string) string {
	// Use the name directly if it's already valid, otherwise hash it.
	// Our names are like "ls-0e8cb90f" which are already valid tunnel IDs.
	return name
}

const sdkAPIVersion = "2023-09-27-preview"

// SDKManager wraps the Dev Tunnels SDK Manager for tunnel lifecycle operations.
// All management operations (create, delete, token retrieval) go through this type;
// the relay hosting protocol still requires the devtunnel CLI binary.
type SDKManager struct {
	mu        sync.Mutex
	manager   *tunnels.Manager
	authToken string
	// sdkTunnels caches the full Tunnel objects returned by the API so that
	// operations that require ClusterID + TunnelID can resolve them by name.
	sdkTunnels map[string]*tunnels.Tunnel
}

// globalSDK is the package-level singleton, initialised by InitSDK.
var globalSDK *SDKManager

// InitSDK initialises the SDK Manager with a Microsoft Entra ID (Azure AD) bearer
// token.  The manager is stored in the package-level globalSDK variable.
// It is safe to call InitSDK multiple times; only the first call takes effect.
func InitSDK(authToken string) error {
	// Re-entrant guard: only initialise once per token value.
	// If the token changed the caller should create a new process.
	if globalSDK != nil {
		return nil
	}

	serviceURI := tunnels.ServiceProperties.ServiceURI
	parsedURL, err := url.Parse(serviceURI)
	if err != nil {
		return fmt.Errorf("devtunnel sdk: parse service URI %q: %w", serviceURI, err)
	}

	userAgents := []tunnels.UserAgent{
		{Name: "linkspan", Version: "0.1.0"},
	}

	// tokenProvider returns the bearer token for every outgoing SDK request.
	tokenProvider := func() string {
		return "Bearer " + authToken
	}

	debugClient := &http.Client{Transport: &debugTransport{base: http.DefaultTransport}}
	mgr, err := tunnels.NewManager(userAgents, tokenProvider, parsedURL, debugClient, sdkAPIVersion)
	if err != nil {
		return fmt.Errorf("devtunnel sdk: create manager: %w", err)
	}

	globalSDK = &SDKManager{
		manager:    mgr,
		authToken:  authToken,
		sdkTunnels: make(map[string]*tunnels.Tunnel),
	}
	return nil
}

// requireSDK returns globalSDK or an error if InitSDK has not been called.
func requireSDK() (*SDKManager, error) {
	if globalSDK == nil {
		return nil, fmt.Errorf("devtunnel sdk: not initialised — call InitSDK first")
	}
	return globalSDK, nil
}

// SDKCreateTunnel creates a tunnel via a direct HTTP PUT to the tunnel service,
// bypassing the SDK's CreateTunnel which has a broken retry loop (retries
// unconditionally, even on success, causing 409/400 cascading failures).
// tunnelName is used as the tunnel ID (custom display names are a premium feature).
// No ports are registered — use SDKAddPort to add forwarding later.
func SDKCreateTunnel(ctx context.Context, tunnelName string) (*tunnels.Tunnel, error) {
	sdk, err := requireSDK()
	if err != nil {
		return nil, err
	}

	sdk.mu.Lock()
	defer sdk.mu.Unlock()

	log.Printf("devtunnel sdk: creating tunnel %q", tunnelName)

	created, err := sdk.createTunnelHTTP(ctx, tunnelName)
	if err != nil {
		return nil, fmt.Errorf("devtunnel sdk: CreateTunnel %q: %w", tunnelName, err)
	}
	log.Printf("devtunnel sdk: tunnel created — id=%s cluster=%s", created.TunnelID, created.ClusterID)

	sdk.sdkTunnels[tunnelName] = created
	return created, nil
}

// SDKAddPort registers a port on an existing tunnel via the SDK.
func SDKAddPort(ctx context.Context, tunnelName string, port int) error {
	sdk, err := requireSDK()
	if err != nil {
		return err
	}

	sdk.mu.Lock()
	defer sdk.mu.Unlock()

	t, err := sdk.resolveTunnel(ctx, tunnelName)
	if err != nil {
		return fmt.Errorf("devtunnel sdk: resolve tunnel %q for port add: %w", tunnelName, err)
	}

	portReq := &tunnels.TunnelPort{
		PortNumber: uint16(port), //nolint:gosec // port numbers fit in uint16
		Protocol:   string(tunnels.TunnelProtocolAuto),
	}
	log.Printf("devtunnel sdk: adding port %d to tunnel %q", port, tunnelName)
	if _, err := sdk.manager.CreateTunnelPort(ctx, t, portReq, nil); err != nil {
		return fmt.Errorf("devtunnel sdk: CreateTunnelPort %d on %q: %w", port, tunnelName, err)
	}

	return nil
}

// createTunnelHTTP performs a single PUT /tunnels/{id} request to create a tunnel.
// Only tunnelId is set in the request body — custom display names are a premium
// feature.  The logical tunnelName is tracked locally in our sdkTunnels cache.
func (sdk *SDKManager) createTunnelHTTP(ctx context.Context, tunnelName string) (*tunnels.Tunnel, error) {
	serviceURI := tunnels.ServiceProperties.ServiceURI
	tunnelID := generateID(tunnelName)

	reqURL := fmt.Sprintf("%stunnels/%s?api-version=%s", serviceURI, url.PathEscape(tunnelID), sdkAPIVersion)

	body := map[string]any{
		"tunnelId": tunnelID,
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Authorization", "Bearer "+sdk.authToken)
	req.Header.Set("User-Agent", "linkspan/0.1.0")
	req.Header.Set("If-Not-Match", "*")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode > 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var created tunnels.Tunnel
	if err := json.Unmarshal(respBody, &created); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &created, nil
}

// SDKDeleteTunnel deletes a tunnel by its logical name (which maps to its tunnel ID).
// Uses a direct HTTP DELETE to avoid nil-pointer panics in the SDK's DeleteTunnel
// (the SDK expects internal state on Tunnel objects that our HTTP-created objects lack).
func SDKDeleteTunnel(ctx context.Context, tunnelName string) error {
	sdk, err := requireSDK()
	if err != nil {
		return err
	}

	sdk.mu.Lock()
	defer sdk.mu.Unlock()

	tunnelID := generateID(tunnelName)
	log.Printf("devtunnel sdk: deleting tunnel %q (id=%s)", tunnelName, tunnelID)

	if err := sdk.deleteTunnelHTTP(ctx, tunnelName); err != nil {
		return fmt.Errorf("devtunnel sdk: DeleteTunnel %q: %w", tunnelName, err)
	}

	delete(sdk.sdkTunnels, tunnelName)
	return nil
}

// deleteTunnelHTTP performs a DELETE /tunnels/{id} request, bypassing the SDK's
// DeleteTunnel which panics on tunnel objects created via createTunnelHTTP.
func (sdk *SDKManager) deleteTunnelHTTP(ctx context.Context, tunnelName string) error {
	tunnelID := generateID(tunnelName)

	// Try to get the cluster ID from the cached tunnel object so we can
	// hit the cluster-specific endpoint directly.
	clusterID := ""
	if t, ok := sdk.sdkTunnels[tunnelName]; ok && t.ClusterID != "" {
		clusterID = t.ClusterID
	}

	var reqURL string
	if clusterID != "" {
		reqURL = fmt.Sprintf("https://%s.rel.tunnels.api.visualstudio.com/tunnels/%s?api-version=%s",
			url.PathEscape(clusterID), url.PathEscape(tunnelID), sdkAPIVersion)
	} else {
		serviceURI := tunnels.ServiceProperties.ServiceURI
		reqURL = fmt.Sprintf("%stunnels/%s?api-version=%s", serviceURI, url.PathEscape(tunnelID), sdkAPIVersion)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+sdk.authToken)
	req.Header.Set("User-Agent", "linkspan/0.1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		log.Printf("devtunnel sdk: tunnel %q already deleted (404)", tunnelName)
		return nil
	}

	if resp.StatusCode > 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// SDKGetHostToken retrieves a host-scoped access token for the given tunnel.
// The token is passed to the devtunnel CLI via `host --access-token`.
func SDKGetHostToken(ctx context.Context, tunnelName string) (string, error) {
	return sdkGetToken(ctx, tunnelName, tunnels.TunnelAccessScopeHost)
}

// SDKGetConnectToken retrieves a connect-scoped access token for the given tunnel.
// This token can be handed to clients so they can connect without AAD credentials.
func SDKGetConnectToken(ctx context.Context, tunnelName string) (string, error) {
	return sdkGetToken(ctx, tunnelName, tunnels.TunnelAccessScopeConnect)
}

// sdkGetToken is the shared implementation for host/connect token retrieval.
func sdkGetToken(ctx context.Context, tunnelName string, scope tunnels.TunnelAccessScope) (string, error) {
	sdk, err := requireSDK()
	if err != nil {
		return "", err
	}

	sdk.mu.Lock()
	defer sdk.mu.Unlock()

	t, err := sdk.resolveTunnel(ctx, tunnelName)
	if err != nil {
		return "", err
	}

	// Request the tunnel again with the desired token scope so the response
	// includes the AccessTokens map populated with a token for that scope.
	opts := &tunnels.TunnelRequestOptions{
		TokenScopes: tunnels.TunnelAccessScopes{scope},
	}
	refreshed, err := sdk.manager.GetTunnel(ctx, t, opts)
	if err != nil {
		return "", fmt.Errorf("devtunnel sdk: GetTunnel for %q scope=%s: %w", tunnelName, scope, err)
	}

	token, ok := refreshed.AccessTokens[scope]
	if !ok || token == "" {
		return "", fmt.Errorf("devtunnel sdk: no %s token returned for tunnel %q", scope, tunnelName)
	}

	// Update cache with latest object.
	sdk.sdkTunnels[tunnelName] = refreshed
	return token, nil
}

// SDKListTunnels returns all tunnels visible to the authenticated user.
func SDKListTunnels(ctx context.Context) ([]*tunnels.Tunnel, error) {
	sdk, err := requireSDK()
	if err != nil {
		return nil, err
	}

	// clusterID="" and domain="" means "list all tunnels globally"
	ts, err := sdk.manager.ListTunnels(ctx, "", "", nil)
	if err != nil {
		return nil, fmt.Errorf("devtunnel sdk: ListTunnels: %w", err)
	}
	return ts, nil
}

// resolveTunnel looks up the Tunnel object by logical name (which equals the tunnel ID)
// from the local cache; if not found it falls back to a network request.
// Must be called with sdk.mu held.
func (sdk *SDKManager) resolveTunnel(ctx context.Context, tunnelName string) (*tunnels.Tunnel, error) {
	if t, ok := sdk.sdkTunnels[tunnelName]; ok {
		return t, nil
	}

	// Not in cache — query by tunnel ID (we derive the ID from the name).
	probe := &tunnels.Tunnel{TunnelID: generateID(tunnelName)}
	t, err := sdk.manager.GetTunnel(ctx, probe, nil)
	if err != nil {
		return nil, fmt.Errorf("devtunnel sdk: resolve tunnel %q: %w", tunnelName, err)
	}
	sdk.sdkTunnels[tunnelName] = t
	return t, nil
}
