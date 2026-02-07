package tunnel

import (
	"context"
	"fmt"
	"sync"

	"github.com/fatedier/frp/client"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/config/v1/validation"
	"github.com/fatedier/frp/pkg/policy/security"
	"github.com/fatedier/frp/pkg/util/log"
)

type FrpTunnelInfo struct {
	TunnelName   string `json:"tunnelName"`
	TunnelType string `json:"tunnelType"`
}

// FRPTunnelClient wraps an FRP client service so it can be stopped.
type FRPTunnelClient struct {
	service *client.Service
	cancel  context.CancelFunc
	mu      sync.Mutex
}

// activeTunnels tracks running FRP tunnels by tunnel ID for later termination.
var (
	activeTunnels   = make(map[string]*FRPTunnelClient)
	activeTunnelsMu sync.Mutex
)

// Stop gracefully stops the FRP tunnel client.
func (c *FRPTunnelClient) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Cancel context first to signal the service to stop
	if c.cancel != nil {
		log.Infof("stopping FRP tunnel...")
		c.cancel()
		c.cancel = nil
	}
	
	// Set service to nil to prevent double-close
	c.service = nil
	
	return nil
}

// StopTunnelByID stops the FRP tunnel associated with the given tunnel ID.
func StopFrpTunnelByName(tunnelName string) error {
	activeTunnelsMu.Lock()
	tunnel, exists := activeTunnels[tunnelName]
	if exists {
		delete(activeTunnels, tunnelName)
	}
	activeTunnelsMu.Unlock()

	if !exists {
		return fmt.Errorf("no tunnel found for  %s", tunnelName)
	}

	return tunnel.Stop()
}

// StopAllTunnels stops all active FRP tunnels.
func StopFrpAllTunnels() {
	activeTunnelsMu.Lock()
	tunnels := make([]*FRPTunnelClient, 0, len(activeTunnels))
	for _, tunnel := range activeTunnels {
		tunnels = append(tunnels, tunnel)
	}
	activeTunnels = make(map[string]*FRPTunnelClient)
	activeTunnelsMu.Unlock()

	for _, tunnel := range tunnels {
		_ = tunnel.Stop()
	}
}

func FrpTunnelProxyCreate(tunnelName string, port int, tunnelType string,
	tunnelSecret string, discoveryHost string, discoveryPort int, discoveryToken string) (FrpTunnelInfo, error) {
	// Create XTCP proxy config with secret key
	proxyConfig := &v1.XTCPProxyConfig{}
	proxyConfig.Name = tunnelName
	proxyConfig.Type = tunnelType
	proxyConfig.LocalPort = port
	proxyConfig.Secretkey = tunnelSecret // Set the secret key here
	proxyConfig.LocalIP = "127.0.0.1"
	proxyConfig.Transport.BandwidthLimitMode = "client" // Set valid bandwidth limit mode
	proxyConfigs := []v1.ProxyConfigurer{proxyConfig}

	commonConfig := &v1.ClientCommonConfig{}
	commonConfig.Auth.Method = v1.AuthMethod("token")
	commonConfig.Auth.Token = discoveryToken
	commonConfig.ServerAddr = discoveryHost
	commonConfig.ServerPort = discoveryPort
	commonConfig.Transport.Protocol = "tcp" // Set valid transport protocol
	commonConfig.Log.Level = "info"
	commonConfig.Log.To = "console"

	visitorCfgs := []v1.VisitorConfigurer{}
	unsafeFeatures := security.NewUnsafeFeatures(nil)

	warning, err := validation.ValidateAllClientConfig(commonConfig, proxyConfigs, visitorCfgs, unsafeFeatures)
	if err != nil {
		fmt.Printf("validation error: %v\n", err)
		return FrpTunnelInfo{}, err
	}
	if warning != nil {
		fmt.Printf("validation warning: %s\n", warning.Error())
	}

	srv, ctx, cancel, err := startService(commonConfig, proxyConfigs, visitorCfgs)
	if err != nil {
		log.Errorf("Error starting service: %v", err)
		return FrpTunnelInfo{}, err
	}

	tunnelClient := &FRPTunnelClient{
		service: srv,
		cancel:  cancel,
	}

	// Register the tunnel for later termination
	activeTunnelsMu.Lock()
	activeTunnels[tunnelName] = tunnelClient
	activeTunnelsMu.Unlock()

	// Monitor for errors in background
	go func() {
		<-ctx.Done()
		activeTunnelsMu.Lock()
		delete(activeTunnels, tunnelName)
		activeTunnelsMu.Unlock()
	}()

	info := FrpTunnelInfo{
		TunnelName:   tunnelName,
		TunnelType: "xtcp",
	}

	return info, nil
}

func FrpTunnelVisitorCreate(tunnelName string, bindPort int, tunnelType string,
	tunnelSecret string, discoveryHost string, discoveryPort int, discoveryToken string) (FrpTunnelInfo, error) {
	// Placeholder for FRP tunnel creation logic
	return FrpTunnelInfo{}, nil
}

func FrpTunnelList() ([]FrpTunnelInfo, error) {
	activeTunnelsMu.Lock()
	defer activeTunnelsMu.Unlock()

	tunnelInfos := make([]FrpTunnelInfo, 0, len(activeTunnels))
	for tunnelName, _ := range activeTunnels {
		tunnelInfos = append(tunnelInfos, FrpTunnelInfo{
			TunnelName: tunnelName,
			TunnelType: "xtcp", // Assuming all are xtcp for this example
		})
	}

	return tunnelInfos, nil
}

func startService(
	cfg *v1.ClientCommonConfig,
	proxyCfgs []v1.ProxyConfigurer,
	visitorCfgs []v1.VisitorConfigurer,
) (*client.Service, context.Context, context.CancelFunc, error) {
	log.InitLogger(cfg.Log.To, cfg.Log.Level, int(cfg.Log.MaxDays), cfg.Log.DisablePrintColor)

	svr, err := client.NewService(client.ServiceOptions{
		Common:      cfg,
		ProxyCfgs:   proxyCfgs,
		VisitorCfgs: visitorCfgs,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		err := svr.Run(ctx)
		if err != nil {
			log.Errorf("Error running server: %v", err)
		}
	}()

	return svr, ctx, cancel, nil
}
