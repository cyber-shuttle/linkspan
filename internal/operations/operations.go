package operations

import (
	"context"
	"flag"
	"fmt"
	"github.com/cyber-shuttle/linkspan/internal/config"
	pm "github.com/cyber-shuttle/linkspan/internal/process"
	"github.com/cyber-shuttle/linkspan/subsystems/fork"
	"github.com/cyber-shuttle/linkspan/subsystems/mount"
	"github.com/cyber-shuttle/linkspan/subsystems/tunnel"
	"github.com/cyber-shuttle/linkspan/subsystems/vscode"
	"log"
	"os"
	"runtime"
	"time"
)

func StartAPIDevTunnel(tunnelToken string, tunnelID string,
	tunnelRetries int, tunnelRetryDelay time.Duration,
	tunnelAttemptTimeout time.Duration,
	tunnelCluster string, serverPort int,
	ctx context.Context) error {
	if tunnelToken == "" {
		log.Fatalf("devtunnel: warning — --tunnel-auth-token not provided; tunnel startup will fail")
	}
	go func() {
		// Host a client-created tunnel when an id is supplied; otherwise create our own.
		if tunnelID == "" {
			tunnelID = fmt.Sprintf("linkspan-tunnel-%d", time.Now().UnixNano())
		}

		// cleanupAttempt kills any host CLI process and removes the tunnel
		// from the manager so a timed-out or failed attempt doesn't leak.
		cleanupAttempt := func() {
			info, err := tunnel.GlobalDevTunnelManager.Find(tunnelID)
			if err != nil {
				return // not registered yet, nothing to clean up
			}
			if info.HostCmdID != "" {
				_ = pm.GlobalProcessManager.Kill(info.HostCmdID)
			}
			tunnel.GlobalDevTunnelManager.Remove(tunnelID)
		}

		for attempt := 1; attempt <= tunnelRetries; attempt++ {
			log.Printf("devtunnel: attempt %d/%d to bring up tunnel %s", attempt, tunnelRetries, tunnelID)

			ch := make(chan error, 1)
			go func() {
				conn, err := tunnel.DevTunnelSetup(tunnelID, "1d", tunnelToken, tunnelID != "", tunnelCluster, serverPort)
				if err != nil {
					log.Printf("devtunnel bring-up error: %v", err)
					ch <- err
					return
				}

				log.Printf("Connect to agent using the URL: %s", conn.ConnectionURL)
				log.Printf("DevTunnel ID: %s", conn.DevTunnelInfo.TunnelID)
				log.Printf("DevTunnel Token: %s", conn.Token)
				log.Printf("DevTunnel forwarded ports: %v", conn.DevTunnelInfo.Ports)
				log.Printf("Devtunnel cluster id: %s", conn.DevTunnelInfo.ClusterID)
				ch <- nil
			}()

			attemptCtx, cancel := context.WithTimeout(ctx, tunnelAttemptTimeout)
			select {
			case err := <-ch:
				cancel()
				if err == nil {
					log.Printf("devtunnel: successfully created %s", tunnelID)
					return
				}
				log.Printf("devtunnel: attempt %d failed: %v", attempt, err)
				cleanupAttempt()
			case <-attemptCtx.Done():
				log.Printf("devtunnel: attempt %d timed out after %s", attempt, tunnelAttemptTimeout.String())
				cancel()
				cleanupAttempt()
			}

			if attempt < tunnelRetries {
				time.Sleep(tunnelRetryDelay)
			}
		}

		log.Fatalf("devtunnel: failed to create tunnel %s after %d attempts", tunnelID, tunnelRetries)
	}()
	return nil
}

func ProcessCommandArguments(c config.LinkspanConfig) error {
	versionFlag := flag.Bool("version", false, "print version information and exit")
	verboseVersionFlag := flag.Bool("verbose-version", false, "print verbose version information and exit")

	tunnelAPI := flag.String("tunnel-api", "devtunnels", "tunnel API provider name (e.g. devtunnels)")
	tunnelEnable := flag.Bool("tunnel-enable", false, "enable tunnel startup")
	tunnelID := flag.String("tunnel-id", "", "host this client-created dev tunnel id instead of creating one; the client owns its lifecycle")
	tunnelCluster := flag.String("tunnel-cluster", "", "cluster id of the client-created tunnel (required with --tunnel-id to resolve it)")
	tunnelAuthToken := flag.String("tunnel-auth-token", "", "Microsoft Entra ID bearer token for the Dev Tunnels service")
	tunnelRetries := flag.Int("tunnel-retries", 3, "number of retries for tunnel startup")
	tunnelRetryDelay := flag.Duration("tunnel-retry-delay", 2*time.Second, "delay between tunnel startup retries")
	tunnelAttemptTimeout := flag.Duration("tunnel-attempt-timeout", 10*time.Second, "timeout per tunnel setup attempt")
	serverPortFlag := flag.Int("port", 8080, "port for the HTTP server to listen on")
	serverHostFlag := flag.String("host", "0.0.0.0", "host/IP for the HTTP server to bind to")
	flag.Parse()

	c.TunnelApi = *tunnelAPI
	c.EnableAPITunnelAtStartup = *tunnelEnable
	c.TunnelId = *tunnelID
	c.TunnelCluster = *tunnelCluster
	c.TunnelAuthToken = *tunnelAuthToken
	c.TunnelRetries = *tunnelRetries
	c.TunnelRetryDelay = *tunnelRetryDelay
	c.TunnelAttemptTimeout = *tunnelAttemptTimeout
	c.ServerPort = *serverPortFlag
	c.ServerHost = *serverHostFlag

	if *versionFlag {
		fmt.Printf("%s\n", c.Version)
		os.Exit(0)
	}

	if *verboseVersionFlag {
		fmt.Printf("%s\n", c.Version)
		fmt.Printf("  commit:    %s\n", c.Commit)
		fmt.Printf("  built:     %s\n", c.Date)
		fmt.Printf("  built by:  %s\n", c.BuiltBy)
		fmt.Printf("  go:        %s\n", runtime.Version())
		fmt.Printf("  platform:  %s/%s\n", runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}
	return nil
}

func CleanupResources(c config.LinkspanConfig) {
	log.Println("Cleaning up resources before shutdown...")
	mount.CleanupAll()
	fork.GlobalForkProcessManager.KillAllForkProcesses()
	pm.GlobalProcessManager.KillAll()
	tunnel.GlobalDevTunnelManager.CleanAll(c.TunnelAuthToken)
	tunnel.DeleteAllFRPTunnels()
	vscode.StopAllSSHServers()
	log.Println("Resource cleanup completed.")
}
