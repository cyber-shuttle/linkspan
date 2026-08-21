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
	"github.com/cyber-shuttle/linkspan/internal/controller"
	"github.com/cyber-shuttle/linkspan/internal/logstream"
	ops "github.com/cyber-shuttle/linkspan/internal/operations"
	"github.com/cyber-shuttle/linkspan/subsystems/checkpoint"
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
	ops.ProcessCommandArguments(c)

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

	if c.SocketPath != "" {
		if _, err := listenUnix(srv, c.SocketPath); err != nil {
			log.Fatalf("failed to listen on unix socket %s: %v", c.SocketPath, err)
		}
		log.Printf("also listening on unix socket %s", c.SocketPath)
	}

	if c.RestorePath != "" && c.ForkCommand != "" {
		log.Fatalf("Can not perform restore and fork execution at same time")
	}

	if c.RestorePath != "" {
		log.Printf("Restoring from path %s", c.RestorePath)
		cp := &checkpoint.CriuCheckpointer{
			CriuPath:               c.CRIUPath,
			SupportGpuCheckpoint:   c.SupportGpuCheckpoint,
			AdditionalCriuOpts:     c.AdditionalCriuOpts,
			DumpDirRoot:            c.DumpDirRoot,
			AllowedCheckpointUsers: c.AllowedCheckpointUsers,
		}
		result, err := cp.Restore(ctx, c.RestorePath)
		if err != nil {
			log.Fatalf("Failed to restore process from path %s: %v", c.RestorePath, err)
		}
		log.Printf("Restore completed successfully (pid=%d)", result.Pid)

		if c.CheckpointForkAfterDelay > 0 {
			// The restored process is detached from CRIU and is not a child of
			// linkspan's ProcessManager, so it cannot be looked up by internal
			// process id for a follow-up checkpoint.
			log.Printf("warning: --checkpoint-fork-after-delay is not supported for a restored process; ignoring")
		}

		if c.ShutdownOnForkCompletion && result.Pid > 0 {
			go watchPidAndShutdown(result.Pid, fmt.Sprintf("restored process %d exited", result.Pid))
		}
	}

	// Start fork process if specified
	if c.ForkCommand != "" {
		internalProcessId, err := ops.StartForkProcess(*c)
		if err != nil {
			log.Fatalf("Failed to start fork process: %v", err)
		}

		if c.CheckpointForkAfterDelay > 0 && c.CRIUPath != "" {
			log.Printf("waiting %d seconds before checkpointing fork process %s", c.CheckpointForkAfterDelay, internalProcessId)
			cp := &checkpoint.CriuCheckpointer{
				CriuPath:               c.CRIUPath,
				SupportGpuCheckpoint:   c.SupportGpuCheckpoint,
				AdditionalCriuOpts:     c.AdditionalCriuOpts,
				DumpDirRoot:            c.DumpDirRoot,
				AllowedCheckpointUsers: c.AllowedCheckpointUsers,
			}
			delay := time.Duration(c.CheckpointForkAfterDelay) * time.Second
			if err := cp.CheckpointProcessAfterDelay(ctx, internalProcessId, delay); err != nil {
				log.Printf("failed to schedule checkpoint for process %s: %v", internalProcessId, err)
			}
		}
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
	case reason := <-controller.ExternalShutdownChannel:
		log.Printf("Shutdown triggered: %s", reason)
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

// listenUnix serves srv on a unix socket in a background goroutine.
func listenUnix(srv *http.Server, path string) (net.Listener, error) {
	os.Remove(path) // clear a stale socket; bind fails if the path exists
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("unix socket server error: %v", err)
		}
	}()
	return ln, nil
}

// Used for the restore path, where the restored process is detached from CRIU and therefore not a child linkspan can Wait() on.
func watchPidAndShutdown(pid int, reason string) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
			controller.TriggerShutdown(reason)
			return
		}
	}
}
