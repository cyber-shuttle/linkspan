package main

import (
	"context"
	"fmt"
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
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/config"
	"github.com/cyber-shuttle/linkspan/internal/controller"
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
  
  /*
  if *socketPath != "" {
		if _, err := listenUnix(srv, *socketPath); err != nil {
			log.Fatalf("failed to listen on unix socket %s: %v", *socketPath, err)
		}
		log.Printf("also listening on unix socket %s", *socketPath)
	}
  */

	// Start fork process if specified
	if c.ForkCommand != "" {
		_, err := ops.StartForkProcess(*c)
		if err != nil {
			log.Fatalf("failed to start fork process: %v", err)
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
