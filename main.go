package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/httpapi"
	pm "github.com/cyber-shuttle/linkspan/internal/process"
	"github.com/cyber-shuttle/linkspan/internal/workflow"
	"github.com/cyber-shuttle/linkspan/subsystems/sshd"
	"github.com/cyber-shuttle/linkspan/subsystems/tunnel"
)

// Set via ldflags. Both consumers parse `--version` as a bare X.Y.Z[.commit],
// so it must stay the only line on stdout.
var version = "dev"

// main is a wrapper so every defer in run runs before the process exits.
// log.Fatalf would skip them and orphan the devtunnel relay.
func main() { os.Exit(run()) }

// run returns the process exit status.
func run() int {
	versionFlag := flag.Bool("version", false, "print version information and exit")
	tunnelEnable := flag.Bool("tunnel-enable", false, "enable tunnel startup")
	tunnelID := flag.String("tunnel-id", "", "id of the client-created dev tunnel to host; the client owns its lifecycle")
	tunnelCluster := flag.String("tunnel-cluster", "", "cluster id of --tunnel-id, needed to resolve it")
	tunnelHostToken := flag.String("tunnel-host-token", "", "host-scoped access token for --tunnel-id; the client owns the tunnel and its ports, so no Entra bearer is needed")
	serverPortFlag := flag.Int("port", 8080, "port for the HTTP server to listen on")
	socketPath := flag.String("socket", "", "also listen on this unix socket path (in-cluster access via `srun --jobid`)")
	workflowFile := flag.String("workflow", "", "path to workflow YAML file")
	flag.Parse()

	if *versionFlag {
		fmt.Println(version)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A fatal background failure unwinds through the same path as a signal, so
	// shutdown happens once and in one place.
	ctx, abort := context.WithCancelCause(ctx)
	defer abort(nil)

	serverPort := *serverPortFlag
	if serverPort < 0 || serverPort > 65535 {
		log.Printf("--port must be between 0 and 65535, got %d", serverPort)
		return 1
	}
	addr := fmt.Sprintf("0.0.0.0:%d", serverPort)
	// Addr is unused: we hand Serve our own listener. ReadHeaderTimeout bounds a
	// client that opens a connection and never finishes its headers.
	srv := &http.Server{Handler: httpapi.Mux(), ReadHeaderTimeout: 10 * time.Second}

	// Bind before the tunnel starts, so the port is open when the relay connects.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("failed to listen on %s: %v", addr, err)
		return 1
	}
	log.Printf("listening on %s", listener.Addr())

	if *socketPath != "" {
		if err := httpapi.ListenUnix(srv, *socketPath); err != nil {
			log.Printf("failed to listen on unix socket %s: %v", *socketPath, err)
			return 1
		}
		log.Printf("also listening on unix socket %s", *socketPath)
	}

	if *workflowFile != "" {
		wf, err := workflow.LoadFile(*workflowFile)
		if err != nil {
			log.Printf("workflow: %v", err)
			return 1
		}
		go func() {
			if err := workflow.Run(ctx, wf); err != nil {
				abort(fmt.Errorf("workflow: %w", err))
			}
		}()
	}

	if *tunnelEnable {
		if *tunnelID == "" || *tunnelCluster == "" || *tunnelHostToken == "" {
			log.Printf("devtunnel: --tunnel-enable needs --tunnel-id, --tunnel-cluster and --tunnel-host-token")
			return 1
		}
		go func() {
			if err := tunnel.Host(ctx, *tunnelID, *tunnelCluster, *tunnelHostToken); err != nil {
				abort(fmt.Errorf("devtunnel: %w", err))
			}
		}()
	}

	// Runs on every exit path below: a dead HTTP server must not leave the
	// devtunnel relay running or SSH sessions accepting.
	defer func() {
		abort(nil) // stop the tunnel retry loop before killing what it started
		pm.Global.KillAll()
		sshd.StopAll()
		log.Println("Server gracefully stopped.")
	}()

	serverErr := make(chan error, 1)
	go func() { serverErr <- srv.Serve(listener) }()

	status := 0
	select {
	case <-ctx.Done():
		if cause := context.Cause(ctx); errors.Is(cause, context.Canceled) {
			log.Println("Shutdown signal received...")
		} else {
			log.Printf("fatal: %v", cause)
			status = 1
		}
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("server error: %v", err)
			status = 1
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown error: %v — forcing close", err)
		if closeErr := srv.Close(); closeErr != nil {
			log.Printf("server force-close error: %v", closeErr)
		}
	}
	return status
}
