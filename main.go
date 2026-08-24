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

func main() {
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
		os.Exit(0)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverPort := *serverPortFlag
	if serverPort < 0 || serverPort > 65535 {
		log.Fatalf("--port must be between 0 and 65535, got %d", serverPort)
	}
	addr := fmt.Sprintf("0.0.0.0:%d", serverPort)
	srv := &http.Server{Handler: httpapi.Mux()} // Addr is unused: we hand Serve our own listener

	// Bind before the tunnel starts, so the port is open when the relay connects.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}
	log.Printf("listening on %s", listener.Addr())

	if *socketPath != "" {
		if err := httpapi.ListenUnix(srv, *socketPath); err != nil {
			log.Fatalf("failed to listen on unix socket %s: %v", *socketPath, err)
		}
		log.Printf("also listening on unix socket %s", *socketPath)
	}

	if *workflowFile != "" {
		wf, err := workflow.LoadFile(*workflowFile)
		if err != nil {
			log.Fatalf("workflow: %v", err)
		}
		go func() {
			if err := workflow.Run(ctx, wf); err != nil {
				log.Fatalf("workflow: %v", err)
			}
		}()
	}

	if *tunnelEnable {
		if *tunnelID == "" || *tunnelCluster == "" || *tunnelHostToken == "" {
			log.Fatalf("devtunnel: --tunnel-enable needs --tunnel-id, --tunnel-cluster and --tunnel-host-token")
		}
		go func() {
			if err := tunnel.Host(ctx, *tunnelID, *tunnelCluster, *tunnelHostToken); err != nil {
				log.Fatalf("devtunnel: %v", err)
			}
		}()
	}

	serverErr := make(chan error, 1)
	go func() { serverErr <- srv.Serve(listener) }()

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
		if closeErr := srv.Close(); closeErr != nil {
			log.Printf("server force-close error: %v", closeErr)
		}
	}
	pm.Global.KillAll()
	sshd.StopAll()
	log.Println("Server gracefully stopped.")
}
