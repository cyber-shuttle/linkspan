package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/logstream"
	pm "github.com/cyber-shuttle/linkspan/internal/process"
	"github.com/cyber-shuttle/linkspan/internal/workflow"
	"github.com/cyber-shuttle/linkspan/subsystems/jupyter"
	"github.com/cyber-shuttle/linkspan/subsystems/mount"
	"github.com/cyber-shuttle/linkspan/subsystems/tunnel"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs"
	"github.com/cyber-shuttle/linkspan/subsystems/vscode"
	"github.com/cyber-shuttle/linkspan/utils"
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

// In-memory metadata store (key → arbitrary JSON value).
var (
	metadataStore = make(map[string]json.RawMessage)
	metadataMu    sync.RWMutex
)

func main() {
	// Handle version flag early, before other initialization
	versionFlag := flag.Bool("version", false, "print version information and exit")
	verboseVersionFlag := flag.Bool("verbose-version", false, "print verbose version information and exit")

	// parse CLI flags
	tunnelAPI := flag.String("tunnel-api", "devtunnels", "tunnel API provider name (e.g. devtunnels)")
	tunnelEnable := flag.Bool("tunnel-enable", false, "enable tunnel startup")
	tunnelAuthToken := flag.String("tunnel-auth-token", "", "Microsoft Entra ID bearer token for the Dev Tunnels service")
	tunnelRetries := flag.Int("tunnel-retries", 3, "number of retries for tunnel startup")
	tunnelRetryDelay := flag.Duration("tunnel-retry-delay", 2*time.Second, "delay between tunnel startup retries")
	tunnelAttemptTimeout := flag.Duration("tunnel-attempt-timeout", 10*time.Second, "timeout per tunnel setup attempt")
	serverPortFlag := flag.Int("port", 8080, "port for the HTTP server to listen on")
	serverHostFlag := flag.String("host", "0.0.0.0", "host/IP for the HTTP server to bind to")
	workflowFile := flag.String("workflow", "", "path to workflow YAML file")
	vfsMode := flag.String("vfs-mode", "", "VFS mode: 'sync' or 'mount' (also reads CS_VFS_MODE env)")
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
	vfsModeName := *vfsMode
	if vfsModeName == "" {
		vfsModeName = os.Getenv("CS_VFS_MODE")
	}

	if sessionID != "" && vfsModeName != "" {
		dc, err := vfs.NewDataCache(sessionID)
		if err != nil {
			log.Fatalf("failed to initialize VFS data cache: %v", err)
		}

		vfsSyncProvider = vfs.NewSyncProvider(dc)
		vfsMountProvider = vfs.NewMountProvider(dc)

		switch vfsModeName {
		case "sync":
			if err := vfsSyncProvider.Start(); err != nil {
				log.Fatalf("failed to start VFS sync provider: %v", err)
			}
			log.Printf("[vfs] Sync provider started for session %s", sessionID)
		case "mount":
			if err := vfsMountProvider.Start(); err != nil {
				log.Fatalf("failed to start VFS mount provider: %v", err)
			}
			log.Printf("[vfs] Mount provider started for session %s", sessionID)
		default:
			log.Fatalf("unknown VFS mode: %s (expected 'sync' or 'mount')", vfsModeName)
		}
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

	// Jupyter kernel management
	api.HandleFunc("/jupyter/kernels", jupyter.ListKernels).Methods("GET")
	api.HandleFunc("/jupyter/kernels", jupyter.ProvisionKernel).Methods("POST")
	api.HandleFunc("/jupyter/kernels/{id}", jupyter.DeleteKernel).Methods("DELETE")
	api.HandleFunc("/jupyter/kernels/{id}/connection", jupyter.GetKernelConnectionInfo).Methods("GET")
	api.HandleFunc("/jupyter/kernels/{id}/status", jupyter.GetKernelStatus).Methods("GET")
	api.HandleFunc("/jupyter/kernels/shutdown", jupyter.ShutdownKernel).Methods("POST")

	// VS Code remote session management
	api.HandleFunc("/vscode/sessions", vscode.ListVSCodeSessions).Methods("GET")
	api.HandleFunc("/vscode/sessions", vscode.CreateVSCodeSession).Methods("POST")
	api.HandleFunc("/vscode/sessions/{id}", vscode.DeleteVSCodeSession).Methods("DELETE")
	api.HandleFunc("/vscode/sessions/{id}/status", vscode.GetVSCodeSessionStatus).Methods("GET")

	// Tunnel management
	api.HandleFunc("/tunnels/devtunnels", tunnel.ListDevTunnels).Methods("GET")
	api.HandleFunc("/tunnels/devtunnels", tunnel.CreateDevTunnel).Methods("POST")
	api.HandleFunc("/tunnels/devtunnels/{id}", tunnel.DeleteDevTunnel).Methods("DELETE")

	api.HandleFunc("/tunnels/frp", tunnel.ListFRPTunnels).Methods("GET")
	api.HandleFunc("/tunnels/frp", tunnel.CreateFRPTunnelProxy).Methods("POST")
	api.HandleFunc("/tunnels/frp/{id}", tunnel.DeleteFRPTunnel).Methods("DELETE")

	// Provider-agnostic tunnel endpoints
	// NOTE: /tunnels/connect must be registered before /tunnels/{id} so that
	// gorilla/mux does not match "connect" as a tunnel ID.
	api.HandleFunc("/tunnels/connect", tunnel.ConnectTunnel).Methods("POST")
	api.HandleFunc("/tunnels/connect/{id}", tunnel.DisconnectTunnel).Methods("DELETE")
	api.HandleFunc("/tunnels", tunnel.ListTunnels).Methods("GET")
	api.HandleFunc("/tunnels", tunnel.CreateTunnel).Methods("POST")
	api.HandleFunc("/tunnels/{id}/ports", tunnel.AddTunnelPort).Methods("POST")
	api.HandleFunc("/tunnels/{id}", tunnel.DeleteTunnel).Methods("DELETE")

	// Health and workflow status
	api.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok"}`)
	}).Methods("GET")

	// Workflow status — set up after engine creation below.
	var workflowEngine *workflow.Engine
	api.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if workflowEngine == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"state":"idle","currentStep":0,"totalSteps":0,"outputs":{}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(workflowEngine.Status())
	}).Methods("GET")

	// Metadata store — in-memory key-value for shared state
	api.HandleFunc("/metadata", func(w http.ResponseWriter, r *http.Request) {
		metadataMu.RLock()
		defer metadataMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(metadataStore)
	}).Methods("GET")

	api.HandleFunc("/metadata/{key:.+}", func(w http.ResponseWriter, r *http.Request) {
		key := mux.Vars(r)["key"]
		switch r.Method {
		case "GET":
			metadataMu.RLock()
			val, ok := metadataStore[key]
			metadataMu.RUnlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(val)
		case "PUT":
			r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			metadataMu.Lock()
			metadataStore[key] = json.RawMessage(body)
			metadataMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "DELETE":
			metadataMu.Lock()
			delete(metadataStore, key)
			metadataMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}).Methods("GET", "PUT", "DELETE")

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

	// Start SSH daemon on a random port
	sshPort, err := utils.GetAvailablePort()
	if err != nil {
		log.Fatalf("failed to get available port for SSH: %v", err)
	}
	sshAddr := fmt.Sprintf("0.0.0.0:%d", sshPort)
	sshSessionID := fmt.Sprintf("main-%d", sshPort)
	vscode.StartSSHServerForVSCodeConnection(sshSessionID, sshAddr)
	log.Printf("SSH server listening on %s", sshAddr)

	// Start log stream TCP listener so clients can connect for real-time logs.
	logListener, err := logBroadcaster.ListenAndServe("0.0.0.0:0")
	if err != nil {
		log.Fatalf("failed to start log stream listener: %v", err)
	}
	logPort := logListener.Addr().(*net.TCPAddr).Port
	log.Printf("log stream listening on 0.0.0.0:%d", logPort)

	// Run workflow if specified. Use "-" to read from stdin.
	if *workflowFile != "" {
		var wf *workflow.WorkflowConfig
		var err error
		if *workflowFile == "-" {
			wf, err = workflow.LoadReader(os.Stdin)
		} else {
			wf, err = workflow.LoadFile(*workflowFile)
		}
		if err != nil {
			log.Fatalf("workflow: %v", err)
		}
		workflowEngine = workflow.NewEngine(workflow.DefaultRegistry(), map[string]any{
			"Timestamp":        time.Now().Unix(),
			"ServerPort":       serverPort,
			"SshPort":          sshPort,
			"LogPort":          logPort,
			"ServerHost":       serverHost,
			"TunnelAuthToken":  *tunnelAuthToken,
			"LocalTunnelID":    os.Getenv("CS_LOCAL_TUNNEL_ID"),
			"LocalTunnelToken": os.Getenv("CS_LOCAL_TUNNEL_TOKEN"),
			"LocalSshPort":     os.Getenv("CS_LOCAL_SSH_PORT"),
			"LocalWorkspace":   os.Getenv("CS_LOCAL_WORKSPACE"),
			"SessionID":        os.Getenv("CS_SESSION_ID"),
		})
		go func() {
			if err := workflowEngine.Run(ctx, wf); err != nil {
				log.Fatalf("workflow: %v", err)
			}
		}()
	}

	// Start tunnel helper after the listener is bound so the port is open
	// when the tunnel attempts to connect or forward traffic.
	devtunnelAuthTokenForCleanup = *tunnelAuthToken

	if apiTunnelType == "devtunnels" && *tunnelEnable {
		authToken := *tunnelAuthToken
		if authToken == "" {
			log.Println("devtunnel: warning — --tunnel-auth-token not provided; tunnel startup will fail")
		}
		go func() {
			tunnelName := fmt.Sprintf("linkspan-tunnel-%d", time.Now().UnixNano())

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
				log.Printf("devtunnel: attempt %d/%d to create tunnel %s", attempt, *tunnelRetries, tunnelName)

				ch := make(chan error, 1)
				go func() {
					conn, err := tunnel.DevTunnelCreate(tunnelName, "1d", authToken, serverPort, sshPort)
					if err != nil {
						ch <- err
						return
					}

					log.Printf("Connect to agent using the URL: %s", conn.ConnectionURL)
					log.Printf("DevTunnel ID: %s", conn.DevTunnelInfo.TunnelID)
					log.Printf("DevTunnel Token: %s", conn.Token)
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

			log.Printf("devtunnel: failed to create tunnel %s after %d attempts", tunnelName, *tunnelRetries)
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
