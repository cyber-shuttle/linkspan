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
	"strings"
	"syscall"
	"time"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
	jupyter "github.com/cyber-shuttle/linkspan/subsystems/jupyter"
	tunnel "github.com/cyber-shuttle/linkspan/subsystems/tunnel"
	vfs "github.com/cyber-shuttle/linkspan/subsystems/vfs"
	vscode "github.com/cyber-shuttle/linkspan/subsystems/vscode"
	"github.com/gorilla/mux"
)

func main() {

	// parse CLI flags
	tunnelAPI := flag.String("tunnel-api", "devtunnels", "tunnel API provider name (e.g. devtunnels)")
	tunnelEnable := flag.Bool("tunnel-enable", true, "enable tunnel startup")
	tunnelRetries := flag.Int("tunnel-retries", 3, "number of retries for tunnel startup")
	tunnelRetryDelay := flag.Duration("tunnel-retry-delay", 2*time.Second, "delay between tunnel startup retries")
	tunnelAttemptTimeout := flag.Duration("tunnel-attempt-timeout", 10*time.Second, "timeout per tunnel setup attempt")
	flag.Parse()
	// Support users passing `--tunnel-api=devtunnels` by trimming leading '='
	apiTunnelType := strings.TrimLeft(*tunnelAPI, "=")

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt,    // Ctrl+C
		syscall.SIGTERM, // termination (reliable on Linux/macOS)
	)
	defer stop()

	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()

	// Jupyter kernel management
	api.HandleFunc("/jupyter/kernels", jupyter.ListKernels).Methods("GET")
	api.HandleFunc("/jupyter/kernels", jupyter.ProvisionKernel).Methods("POST")
	api.HandleFunc("/jupyter/kernels/{id}", jupyter.DeleteKernel).Methods("DELETE")
	api.HandleFunc("/jupyter/kernels/{id}/connection", jupyter.GetKernelConnectionInfo).Methods("GET")
	api.HandleFunc("/jupyter/kernels/{id}/status", jupyter.GetKernelStatus).Methods("GET")
	api.HandleFunc("/jupyter/kernels/shutdown", jupyter.ShutdownKernel).Methods("POST")

	// VS Code remote session management
	api.HandleFunc("/vscode/sessions", vscode.ListVSCodes).Methods("GET")
	api.HandleFunc("/vscode/sessions", vscode.CreateVSCodeSession).Methods("POST")
	api.HandleFunc("/vscode/sessions/{id}", vscode.TerminateVSCodeSession).Methods("DELETE")

	// Remote filesystem management
	api.HandleFunc("/fs/list", vfs.ListFiles).Methods("GET")
	api.HandleFunc("/fs/read", vfs.ReadFile).Methods("GET")
	api.HandleFunc("/fs/write", vfs.WriteFile).Methods("POST")
	api.HandleFunc("/fs/delete", vfs.DeleteFile).Methods("DELETE")

	// Tunnel management
	api.HandleFunc("/tunnels/devtunnels", tunnel.ListDevTunnels).Methods("GET")
	api.HandleFunc("/tunnels/devtunnels", tunnel.CreateDevTunnel).Methods("POST")
	api.HandleFunc("/tunnels/devtunnels/{id}", tunnel.CloseTunnel).Methods("DELETE")

	serverPort := 8080
	addr := fmt.Sprintf(":%d", serverPort)

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
	log.Printf("listening on %s", addr)

	// Start tunnel helper after the listener is bound so the port is open
	// when the tunnel attempts to connect or forward traffic. Make startup
	// conditional and add retries/timeouts to be more robust in unreliable
	// environments.
	if apiTunnelType == "devtunnels" && *tunnelEnable {
		go func() {
			tunnelName := fmt.Sprintf("aget-tunnel-%d", time.Now().UnixNano())

			for attempt := 1; attempt <= *tunnelRetries; attempt++ {
				log.Printf("devtunnel: attempt %d/%d to create tunnel %s", attempt, *tunnelRetries, tunnelName)

				// run the create + host sequence in a goroutine so we can
				// apply a per-attempt timeout.
				ch := make(chan error, 1)
				go func() {
					_, err := tunnel.DevTunnelCreate(tunnelName, "1d", []int{serverPort})
					if err != nil {
						ch <- err
						return
					}
					_, tunnelConnection, err := tunnel.DevTunnelHost(tunnelName, true)
					if err != nil {
						ch <- err
						return
					}

					log.Printf("Connect to agent using the URL: %s", tunnelConnection.ConnectionURL)
					log.Printf("DevTunnel ID: %s", tunnelConnection.DevTunnelInfo.TunnelID)
					log.Printf("DevTunnel Token: %s", tunnelConnection.Token)
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
				case <-attemptCtx.Done():
					log.Printf("devtunnel: attempt %d timed out after %s", attempt, tunnelAttemptTimeout.String())
				}
				cancel()

				if attempt < *tunnelRetries {
					time.Sleep(*tunnelRetryDelay)
				}
			}

			log.Printf("devtunnel: failed to create tunnel %s after %d attempts", tunnelName, *tunnelRetries)
		}()
	} else if apiTunnelType == "devtunnels" {
		log.Println("devtunnel startup skipped (disabled via flag)")
	}

	// Run server.Serve using the already-bound listener. Serve will return
	// when the server is shut down or on error.
	serverErr := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		serverErr <- err
	}()

	select {
	case <-ctx.Done():
		// Ctrl+C / SIGTERM received
		log.Println("Shutdown signal received...")
	case err := <-serverErr:
		// Server failed to start or died unexpectedly
		if err != nil && err != http.ErrServerClosed {
			log.Printf("server error: %v", err)
		}
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Stop accepting new connections + wait for in-flight requests
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown error: %v", err)
	}

	cleanupResources()

	log.Println("Server gracefully stopped.")
}

func cleanupResources() {
	log.Println("Cleaning up resources before shutdown...")
	// Add resource cleanup logic here, e.g., terminating kernels, closing tunnels, etc.
	pm.GlobalProcessManager.KillAll()
	tunnel.GlobalDevTunnelManager.CleanAll()
	log.Println("Resource cleanup completed.")
}
