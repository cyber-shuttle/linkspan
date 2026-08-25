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
	"github.com/cyber-shuttle/linkspan/internal/workflow"
	"github.com/cyber-shuttle/linkspan/subsystems/sshd"
	"github.com/cyber-shuttle/linkspan/subsystems/tunnel"
)

// Set via ldflags. Both consumers parse `--version` as a bare X.Y.Z[.commit],
// so it must stay the only line on stdout.
var version = "dev"

// A wrapper so every defer in run executes: log.Fatalf would skip them and
// orphan the devtunnel relay.
func main() { os.Exit(run()) }

func run() int {
	versionFlag := flag.Bool("version", false, "print version information and exit")
	tunnelEnable := flag.Bool("tunnel-enable", false, "enable tunnel startup")
	tunnelID := flag.String("tunnel-id", "", "id of the client-created dev tunnel to host; the client owns its lifecycle")
	tunnelCluster := flag.String("tunnel-cluster", "", "cluster id of --tunnel-id, needed to resolve it")
	tunnelHostToken := flag.String("tunnel-host-token", "", "host-scoped access token for --tunnel-id; the client owns the tunnel and its ports, so no Entra bearer is needed")
	serverPort := flag.Int("port", 8080, "port for the HTTP server to listen on")
	socketPath := flag.String("socket", "", "also listen on this unix socket path, for in-cluster access via srun --jobid")
	workflowFile := flag.String("workflow", "", "path to workflow YAML file")
	flag.Parse()

	if *versionFlag {
		fmt.Println(version)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A fatal background failure unwinds the same way a signal does.
	ctx, abort := context.WithCancelCause(ctx)
	defer abort(nil)

	// Loopback only: every route is unauthenticated and POST /vscode/sessions
	// starts an sshd for a caller-supplied key, so the wildcard would offer a
	// shell as the job owner to anything that could route to the node. Consumers
	// arrive through the relay, which dials localhost, or through --socket.
	addr := fmt.Sprintf("127.0.0.1:%d", *serverPort)
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

	hosted := make(chan struct{})
	switch {
	case !*tunnelEnable:
		close(hosted)
	case *tunnelID == "" || *tunnelCluster == "" || *tunnelHostToken == "":
		log.Printf("devtunnel: --tunnel-enable needs --tunnel-id, --tunnel-cluster and --tunnel-host-token")
		return 1
	default:
		go func() {
			defer close(hosted)
			if err := tunnel.Host(ctx, *tunnelID, *tunnelCluster, *tunnelHostToken); err != nil {
				abort(fmt.Errorf("devtunnel: %w", err))
			}
		}()
	}

	// A dead HTTP server must not leave the relay running or sessions accepting.
	defer func() {
		abort(nil) // stop the tunnel retry loop before killing what it started
		// A relay that has not reported ready is known only to Host, which kills
		// it on its own failure path; exiting first orphans it. Bounded, so a
		// wedged bring-up delays shutdown rather than preventing it.
		select {
		case <-hosted:
		case <-time.After(5 * time.Second):
			log.Println("devtunnel: bring-up did not finish stopping in time")
		}
		tunnel.StopRelay()
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
