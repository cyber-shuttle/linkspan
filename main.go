package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/config"
	"github.com/cyber-shuttle/linkspan/internal/logstream"
	ops "github.com/cyber-shuttle/linkspan/internal/operations"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs"
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

	c := config.NewDefaultLinkspanConfig()
	c.Commit = commit
	c.BuiltBy = builtBy
	c.Date = date
	c.Version = version
	ops.ProcessCommandArguments(*c)

	// Install log broadcaster so connected clients receive log output in
	// real time.  Must happen before any log.* calls.
	logBroadcaster := logstream.New(os.Stderr)
	logBroadcaster.Install()

	// Support users passing `--tunnel-api=devtunnels` by trimming leading '='
	apiTunnelType := strings.TrimLeft(c.TunnelApi, "=")

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
	if c.ServerPort < 0 || c.ServerPort > 65535 {
		log.Fatalf("invalid server port: %d", c.ServerPort)
	}
	addr := fmt.Sprintf("%s:%d", c.ServerHost, c.ServerPort)

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
	if c.ServerPort == 0 {
		c.ServerPort = listener.Addr().(*net.TCPAddr).Port
	}
	log.Printf("listening on %s:%d", c.ServerHost, c.ServerPort)

	if apiTunnelType == "devtunnels" && c.EnableAPITunnelAtStartup {
		ops.StartAPIDevTunnel(c.TunnelAuthToken, c.TunnelId, c.TunnelRetries,
			c.TunnelRetryDelay, c.TunnelAttemptTimeout, c.TunnelCluster,
			c.ServerPort, ctx)
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

	ops.CleanupResources(*c)

	log.Println("Server gracefully stopped.")
}
