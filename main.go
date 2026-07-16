package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/logstream"
	pm "github.com/cyber-shuttle/linkspan/internal/process"
	"github.com/cyber-shuttle/linkspan/subsystems/mount"
	"github.com/cyber-shuttle/linkspan/subsystems/tunnel"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs"
	"github.com/cyber-shuttle/linkspan/subsystems/vscode"
	"github.com/gorilla/mux"
)

// Version information set via ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
	builtBy = "unknown"
)

// VFS providers initialized at startup, cleaned up on shutdown.
var (
	vfsSyncProvider  *vfs.SyncProvider
	vfsMountProvider *vfs.MountProvider
)

func main() {
	// Handle version flag early, before other initialization
	versionFlag := flag.Bool("version", false, "print version information and exit")
	verboseVersionFlag := flag.Bool("verbose-version", false, "print verbose version information and exit")

	// parse CLI flags
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
	vfsSessionID := flag.String("vfs-session-id", "", "session ID for VFS (also reads CS_SESSION_ID env)")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("%s\n", version)
		os.Exit(0)
	}

	if *verboseVersionFlag {
		fmt.Printf("%s\n", version)
		fmt.Printf("  commit:    %s\n", commit)
		fmt.Printf("  built:     %s\n", date)
		fmt.Printf("  built by:  %s\n", builtBy)
		fmt.Printf("  go:        %s\n", runtime.Version())
		fmt.Printf("  platform:  %s/%s\n", runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	// Install log broadcaster so connected clients receive log output in
	// real time.  Must happen before any log.* calls.
	logBroadcaster := logstream.New(os.Stderr)
	logBroadcaster.Install()

	// Initialize VFS if session ID is provided
	sessionID := *vfsSessionID
	if sessionID == "" {
		sessionID = os.Getenv("CS_SESSION_ID")
	}

	// Support users passing `--tunnel-api=devtunnels` by trimming leading '='
	apiTunnelType := strings.TrimLeft(*tunnelAPI, "=")

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt,    // Ctrl+C
		syscall.SIGTERM, // termination (reliable on Linux/macOS)
	)
	defer stop()

	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	RegisterRoutes(api)

	// Use the configured server host and port from CLI flags.
	// Port 0 means "let the OS pick a free port".
	serverPort := *serverPortFlag
	serverHost := *serverHostFlag
	if serverPort < 0 || serverPort > 65535 {
		log.Fatalf("invalid server port: %d", serverPort)
	}
	addr := fmt.Sprintf("%s:%d", serverHost, serverPort)

	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	// Create listener first so the port is bound before starting any
	// external tunnel process that expects the port to be open.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}

	// When port 0 was requested, update serverPort to the actual bound port.
	if serverPort == 0 {
		serverPort = listener.Addr().(*net.TCPAddr).Port
	}
	log.Printf("listening on %s:%d", serverHost, serverPort)

	// Start tunnel helper after the listener is bound so the port is open
	// when the tunnel attempts to connect or forward traffic.
	devtunnelAuthTokenForCleanup = *tunnelAuthToken

	if apiTunnelType == "devtunnels" && *tunnelEnable {
		authToken := *tunnelAuthToken
		if authToken == "" {
			log.Fatalf("devtunnel: warning — --tunnel-auth-token not provided; tunnel startup will fail")
		}
		go func() {
			// Host a client-created tunnel when an id is supplied; otherwise create our own.
			tunnelName := *tunnelID
			if tunnelName == "" {
				tunnelName = fmt.Sprintf("linkspan-tunnel-%d", time.Now().UnixNano())
			}

			// cleanupAttempt kills any host CLI process and removes the tunnel
			// from the manager so a timed-out or failed attempt doesn't leak.
			cleanupAttempt := func() {
				info, err := tunnel.GlobalDevTunnelManager.Find(tunnelName)
				if err != nil {
					return // not registered yet, nothing to clean up
				}
				if info.HostCmdID != "" {
					_ = pm.GlobalProcessManager.Kill(info.HostCmdID)
				}
				tunnel.GlobalDevTunnelManager.Remove(tunnelName)
			}

			for attempt := 1; attempt <= *tunnelRetries; attempt++ {
				log.Printf("devtunnel: attempt %d/%d to bring up tunnel %s", attempt, *tunnelRetries, tunnelName)

				ch := make(chan error, 1)
				go func() {
					conn, err := tunnel.DevTunnelSetup(tunnelName, "1d", authToken, *tunnelID != "", *tunnelCluster, serverPort)
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

				attemptCtx, cancel := context.WithTimeout(ctx, *tunnelAttemptTimeout)
				select {
				case err := <-ch:
					cancel()
					if err == nil {
						log.Printf("devtunnel: successfully created %s", tunnelName)
						return
					}
					log.Printf("devtunnel: attempt %d failed: %v", attempt, err)
					cleanupAttempt()
				case <-attemptCtx.Done():
					log.Printf("devtunnel: attempt %d timed out after %s", attempt, tunnelAttemptTimeout.String())
					cancel()
					cleanupAttempt()
				}

				if attempt < *tunnelRetries {
					time.Sleep(*tunnelRetryDelay)
				}
			}

			log.Fatalf("devtunnel: failed to create tunnel %s after %d attempts", tunnelName, *tunnelRetries)
		}()
	} else if apiTunnelType == "devtunnels" {
		log.Println("devtunnel startup skipped (disabled via flag)")
	}

	// Run server
	serverErr := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		serverErr <- err
	}()

	select {
	case <-ctx.Done():
		log.Println("Shutdown signal received...")
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("server error: %v", err)
		}
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown error: %v — forcing close", err)
		// Shutdown did not complete within the deadline; force-close all
		// remaining connections so the process does not hang indefinitely.
		if closeErr := srv.Close(); closeErr != nil {
			log.Printf("server force-close error: %v", closeErr)
		}
	}

	cleanupResources()

	log.Println("Server gracefully stopped.")
}

// devtunnelAuthTokenForCleanup holds the auth token supplied at startup so the
// shutdown path can call CleanAll without needing a separate flag reference.
var devtunnelAuthTokenForCleanup string

func cleanupResources() {
	log.Println("Cleaning up resources before shutdown...")
	mount.CleanupAll()
	pm.GlobalProcessManager.KillAll()
	tunnel.GlobalDevTunnelManager.CleanAll(devtunnelAuthTokenForCleanup)
	tunnel.DeleteAllFRPTunnels()
	vscode.StopAllSSHServers()

	// VFS cleanup
	if vfsSyncProvider != nil {
		vfsSyncProvider.Stop()
	}
	if vfsMountProvider != nil {
		vfsMountProvider.Stop()
	}

	log.Println("Resource cleanup completed.")
}
