package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
	"github.com/cyber-shuttle/linkspan/internal/workflow"
	"github.com/cyber-shuttle/linkspan/subsystems/tunnel"
	"github.com/cyber-shuttle/linkspan/subsystems/vscode"
	"github.com/cyber-shuttle/linkspan/utils"
)

// version is set via ldflags at build time. Both consumers parse `--version`
// output as a bare X.Y.Z[.commit] on stdout, so it must stay the only line.
var version = "dev"

// The client creates the tunnel and owns its lifetime; linkspan only hosts it.
const (
	tunnelRetries        = 3
	tunnelRetryDelay     = 2 * time.Second
	tunnelAttemptTimeout = 10 * time.Second
)

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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		utils.RespondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/metrics", metricsHandler)
	mux.HandleFunc("GET /api/v1/vscode/sessions", vscode.ListVSCodeSessions)
	mux.HandleFunc("POST /api/v1/vscode/sessions", vscode.CreateVSCodeSession)

	serverPort := *serverPortFlag
	if serverPort < 0 || serverPort > 65535 {
		log.Fatalf("invalid server port: %d", serverPort)
	}
	addr := fmt.Sprintf("0.0.0.0:%d", serverPort)
	srv := &http.Server{Addr: addr, Handler: mux}

	// Bind before starting the tunnel so the port is open when the relay connects.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}
	if serverPort == 0 {
		serverPort = listener.Addr().(*net.TCPAddr).Port
	}
	log.Printf("listening on %s", listener.Addr())

	if *socketPath != "" {
		if err := listenUnix(srv, *socketPath); err != nil {
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
		go hostTunnel(ctx, *tunnelID, *tunnelCluster, *tunnelHostToken)
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
	pm.GlobalProcessManager.KillAll()
	vscode.StopAllSSHServers()
	log.Println("Server gracefully stopped.")
}

// hostTunnel retries the relay bring-up, killing the host process left behind by
// a failed or timed-out attempt so it cannot leak.
func hostTunnel(ctx context.Context, tunnelID, clusterID, hostToken string) {
	for attempt := 1; attempt <= tunnelRetries; attempt++ {
		log.Printf("devtunnel: attempt %d/%d to host tunnel %s", attempt, tunnelRetries, tunnelID)
		type hosted struct {
			cmdID, url string
			err        error
		}
		ch := make(chan hosted, 1)
		go func() {
			cmdID, url, err := tunnel.DevTunnelHost(tunnelID, clusterID, hostToken)
			ch <- hosted{cmdID, url, err}
		}()

		attemptCtx, cancel := context.WithTimeout(ctx, tunnelAttemptTimeout)
		select {
		case h := <-ch:
			cancel()
			if h.err == nil {
				log.Printf("devtunnel: successfully hosting %s at %s", tunnelID, h.url)
				return
			}
			log.Printf("devtunnel: attempt %d failed: %v", attempt, h.err)
			if h.cmdID != "" {
				_ = pm.GlobalProcessManager.Kill(h.cmdID)
			}
		case <-attemptCtx.Done():
			cancel()
			log.Printf("devtunnel: attempt %d timed out after %s", attempt, tunnelAttemptTimeout)
		}
		if attempt < tunnelRetries {
			time.Sleep(tunnelRetryDelay)
		}
	}
	log.Fatalf("devtunnel: failed to host tunnel %s after %d attempts", tunnelID, tunnelRetries)
}

// listenUnix serves srv on a unix socket in a background goroutine.
func listenUnix(srv *http.Server, path string) error {
	os.Remove(path) // clear a stale socket; bind fails if the path exists
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("unix socket server error: %v", err)
		}
	}()
	return nil
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
