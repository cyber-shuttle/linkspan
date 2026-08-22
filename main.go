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
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
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
	tunnelID := flag.String("tunnel-id", "", "id of the client-created dev tunnel to host; the client owns its lifecycle")
	tunnelCluster := flag.String("tunnel-cluster", "", "cluster id of --tunnel-id, needed to resolve it")
	tunnelHostToken := flag.String("tunnel-host-token", "", "host-scoped access token for --tunnel-id; the client owns the tunnel and its ports, so no Entra bearer is needed")
	tunnelRetries := flag.Int("tunnel-retries", 3, "number of retries for tunnel startup")
	tunnelRetryDelay := flag.Duration("tunnel-retry-delay", 2*time.Second, "delay between tunnel startup retries")
	tunnelAttemptTimeout := flag.Duration("tunnel-attempt-timeout", 10*time.Second, "timeout per tunnel setup attempt")
	serverPortFlag := flag.Int("port", 8080, "port for the HTTP server to listen on")
	serverHostFlag := flag.String("host", "0.0.0.0", "host/IP for the HTTP server to bind to")
	socketPath := flag.String("socket", "", "also listen on this unix socket path (in-cluster access via `srun --jobid`)")
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
	api.HandleFunc("/tunnels/devtunnels/forward", tunnel.ForwardDevTunnelPort).Methods("POST")
	api.HandleFunc("/tunnels/devtunnels/auth-token", tunnel.RefreshDevTunnelAuthToken).Methods("POST")
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

	// Live resource metrics for the running job (cgroup mem/cpu + nvidia-smi).
	api.HandleFunc("/metrics", metricsHandler).Methods("GET")

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

	if *socketPath != "" {
		if _, err := listenUnix(srv, *socketPath); err != nil {
			log.Fatalf("failed to listen on unix socket %s: %v", *socketPath, err)
		}
		log.Printf("also listening on unix socket %s", *socketPath)
	}

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
			"Timestamp":  time.Now().Unix(),
			"ServerPort": serverPort,
			"ServerHost": serverHost,
		})
		go func() {
			if err := workflowEngine.Run(ctx, wf); err != nil {
				log.Fatalf("workflow: %v", err)
			}
		}()
	}

	// Start tunnel helper after the listener is bound so the port is open
	// when the tunnel attempts to connect or forward traffic.
	if apiTunnelType == "devtunnels" && *tunnelEnable {
		if *tunnelID == "" || *tunnelCluster == "" || *tunnelHostToken == "" {
			log.Fatalf("devtunnel: --tunnel-enable needs --tunnel-id, --tunnel-cluster and --tunnel-host-token")
		}
		go func() {
			// cleanupAttempt kills any host CLI process and removes the tunnel
			// from the manager so a timed-out or failed attempt doesn't leak.
			cleanupAttempt := func() {
				info, err := tunnel.GlobalDevTunnelManager.Find(*tunnelID)
				if err != nil {
					return // not registered yet, nothing to clean up
				}
				if info.HostCmdID != "" {
					_ = pm.GlobalProcessManager.Kill(info.HostCmdID)
				}
				tunnel.GlobalDevTunnelManager.Remove(*tunnelID)
			}

			for attempt := 1; attempt <= *tunnelRetries; attempt++ {
				log.Printf("devtunnel: attempt %d/%d to host tunnel %s", attempt, *tunnelRetries, *tunnelID)

				ch := make(chan error, 1)
				go func() {
					conn, err := tunnel.DevTunnelHost(*tunnelID, *tunnelCluster, *tunnelHostToken)
					if err != nil {
						log.Printf("devtunnel bring-up error: %v", err)
						ch <- err
						return
					}

					log.Printf("Connect to agent using the URL: %s", conn.ConnectionURL)
					ch <- nil
				}()

				attemptCtx, cancel := context.WithTimeout(ctx, *tunnelAttemptTimeout)
				select {
				case err := <-ch:
					cancel()
					if err == nil {
						log.Printf("devtunnel: successfully hosting %s", *tunnelID)
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

			log.Fatalf("devtunnel: failed to host tunnel %s after %d attempts", *tunnelID, *tunnelRetries)
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

func cleanupResources() {
	log.Println("Cleaning up resources before shutdown...")
	mount.CleanupAll()
	pm.GlobalProcessManager.KillAll()
	tunnel.GlobalDevTunnelManager.CleanAll()
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

// --- Live job metrics (GET /api/v1/metrics) -------------------------------------------------------------------------

type gpuMetric struct {
	Index       int `json:"index"`
	UtilPct     int `json:"utilPct"`
	MemUsedMiB  int `json:"memUsedMiB"`
	MemTotalMiB int `json:"memTotalMiB"`
}

// liveMetrics mirrors the csbridge MetricSample live fields. Each source is read independently and omitted (omitempty)
// when unavailable, so the client sees an absent field (undefined) rather than a misleading zero.
type liveMetrics struct {
	MemBytes     *int64      `json:"memBytes,omitempty"`
	CPUUsageUsec *int64      `json:"cpuUsageUsec,omitempty"`
	GPUs         []gpuMetric `json:"gpus,omitempty"`
}

// metricsHandler reports the running job's live resource use: cgroup-v2 memory + cpu and per-GPU nvidia-smi. Every
// source degrades independently — a missing cgroup file or absent nvidia-smi (CPU node) just omits that field; it
// never fails the request. Replaces the srun-driven bash probe csbridge used to run over SSH.
func metricsHandler(w http.ResponseWriter, r *http.Request) {
	m := liveMetrics{GPUs: readGPUMetrics()}
	if cg, err := jobCgroupDir(); err == nil {
		if b, err := readInt64File(filepath.Join(cg, "memory.current")); err == nil {
			m.MemBytes = &b
		}
		if u, err := readCPUUsageUsec(filepath.Join(cg, "cpu.stat")); err == nil {
			m.CPUUsageUsec = &u
		}
	}
	utils.RespondJSON(w, http.StatusOK, m)
}

// jobCgroupDir resolves this process's cgroup-v2 directory stripped to the job level (dropping any /step_* leaf), so
// memory/cpu reflect the whole allocation rather than one step. Mirrors `sed 's|^0::||; s|/step_.*||' /proc/self/cgroup`.
func jobCgroupDir() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	return "/sys/fs/cgroup" + jobCgroupSuffix(string(b)), nil
}

// jobCgroupSuffix is the pure path-munging half of jobCgroupDir (unit-tested).
func jobCgroupSuffix(procCgroup string) string {
	lines := strings.Split(strings.TrimSpace(procCgroup), "\n")
	p := strings.TrimPrefix(strings.TrimSpace(lines[len(lines)-1]), "0::")
	if i := strings.Index(p, "/step_"); i >= 0 {
		p = p[:i]
	}
	return p
}

func readInt64File(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

func readCPUUsageUsec(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parseCPUUsageUsec(string(b))
}

// parseCPUUsageUsec pulls the cumulative `usage_usec` line out of a cgroup cpu.stat body.
func parseCPUUsageUsec(cpuStat string) (int64, error) {
	for ln := range strings.SplitSeq(cpuStat, "\n") {
		if v, ok := strings.CutPrefix(ln, "usage_usec "); ok {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, fmt.Errorf("usage_usec not found in cpu.stat")
}

// readGPUMetrics returns per-GPU stats via nvidia-smi, or nil if it's absent or errors (CPU nodes have no driver).
func readGPUMetrics() []gpuMetric {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,utilization.gpu,memory.used,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	return parseGPUMetrics(string(out))
}

// parseGPUMetrics parses `index, util, memUsed, memTotal` CSV rows; malformed rows are skipped.
func parseGPUMetrics(out string) []gpuMetric {
	var gpus []gpuMetric
	for ln := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Split(ln, ",")
		if len(f) != 4 {
			continue
		}
		var n [4]int
		ok := true
		for i := range f {
			v, err := strconv.Atoi(strings.TrimSpace(f[i]))
			if err != nil {
				ok = false
				break
			}
			n[i] = v
		}
		if ok {
			gpus = append(gpus, gpuMetric{Index: n[0], UtilPct: n[1], MemUsedMiB: n[2], MemTotalMiB: n[3]})
		}
	}
	return gpus
}
