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

var version = "dev" // set via ldflags

// A wrapper so every defer in run executes; log.Fatalf would orphan the relay.
func main() { os.Exit(run()) }

type options struct {
	version         bool
	tunnelEnable    bool
	tunnelID        string
	tunnelCluster   string
	tunnelHostToken string
	serverPort      int
	socketPath      string
	workflowFile    string
}

func registerFlags(fs *flag.FlagSet) *options {
	var o options
	fs.BoolVar(&o.version, "version", false, "print version information and exit")
	fs.BoolVar(&o.tunnelEnable, "tunnel-enable", false, "enable tunnel startup")
	fs.StringVar(&o.tunnelID, "tunnel-id", "", "id of the client-created Dev Tunnel to host; the client owns its lifecycle")
	fs.StringVar(&o.tunnelCluster, "tunnel-cluster", "", "cluster id of --tunnel-id, needed to resolve it")
	fs.StringVar(&o.tunnelHostToken, "tunnel-host-token", "", "host-scoped access token for --tunnel-id; the client owns the tunnel and its ports, so no Entra bearer is needed")
	fs.IntVar(&o.serverPort, "port", 8080, "port for the HTTP server to listen on")
	fs.StringVar(&o.socketPath, "socket", "", "also listen on this unix socket path, for in-cluster access via srun --jobid")
	fs.StringVar(&o.workflowFile, "workflow", "", "path to workflow YAML file")
	return &o
}

func run() int {
	opts := registerFlags(flag.CommandLine)
	flag.Parse()

	if opts.version {
		fmt.Println(version)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, abort := context.WithCancelCause(ctx)
	defer abort(nil)

	// Loopback only: every route is unauthenticated and POST /vscode/sessions
	// starts an sshd for a caller-supplied key, so a wildcard would offer a shell
	// as the job owner to anything that could route to the node.
	addr := fmt.Sprintf("127.0.0.1:%d", opts.serverPort)
	srv := &http.Server{Handler: httpapi.Mux(), ReadHeaderTimeout: 10 * time.Second}

	// Bind before the tunnel starts, so the port is open when the relay connects.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("failed to listen on %s: %v", addr, err)
		return 1
	}
	log.Printf("listening on %s", listener.Addr())

	if opts.socketPath != "" {
		if err := httpapi.ListenUnix(srv, opts.socketPath); err != nil {
			log.Printf("failed to listen on unix socket %s: %v", opts.socketPath, err)
			return 1
		}
		log.Printf("also listening on unix socket %s", opts.socketPath)
	}

	if opts.workflowFile != "" {
		wf, err := workflow.LoadFile(opts.workflowFile)
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

	if opts.tunnelEnable {
		if opts.tunnelID == "" || opts.tunnelCluster == "" || opts.tunnelHostToken == "" {
			log.Printf("devtunnel: --tunnel-enable needs --tunnel-id, --tunnel-cluster and --tunnel-host-token")
			return 1
		}
		go func() {
			if err := tunnel.Host(ctx, opts.tunnelID, opts.tunnelCluster, opts.tunnelHostToken); err != nil {
				abort(fmt.Errorf("devtunnel: %w", err))
			}
		}()
	}

	// A dead HTTP server must not leave the relay running or sessions accepting.
	defer func() {
		abort(nil) // stop the tunnel retry loop before killing what it started
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

	// Trade-off: Close, not Shutdown. No consumer needs a response that straddles
	// exit; revisit if one ever polls across a job's final second.
	_ = srv.Close()
	return status
}
