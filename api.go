package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/cyber-shuttle/linkspan/internal/utils"
	"github.com/cyber-shuttle/linkspan/subsystems/checkpoint"
	"github.com/cyber-shuttle/linkspan/subsystems/fork"
	"github.com/cyber-shuttle/linkspan/subsystems/jupyter"
	"github.com/cyber-shuttle/linkspan/subsystems/tunnel"
	"github.com/cyber-shuttle/linkspan/subsystems/vscode"
	"github.com/gorilla/mux"
)

// In-memory metadata store (key → arbitrary JSON value).
var (
	metadataStore = make(map[string]json.RawMessage)
	metadataMu    sync.RWMutex
)

// RegisterRoutes sets up all API routes on the given router.
func RegisterRoutes(api *mux.Router) {
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

	// Checkpoint / restore
	// NOTE: {id}/restore is registered before {id} for the same reason as
	// /tunnels/connect above.
	api.HandleFunc("/checkpoints", checkpoint.CreateCheckpointHandler).Methods("POST")
	api.HandleFunc("/checkpoints", checkpoint.ListCheckpointsHandler).Methods("GET")
	api.HandleFunc("/checkpoints/{id}/restore", checkpoint.RestoreCheckpointHandler).Methods("POST")
	api.HandleFunc("/checkpoints/{id}", checkpoint.GetCheckpointHandler).Methods("GET")

	// Fork process management
	api.HandleFunc("/fork/run", fork.RunForkProcessHandler).Methods("POST")
	api.HandleFunc("/fork/processes", fork.ListForkProcessesHandler).Methods("GET")
	api.HandleFunc("/fork/processes/{id}", fork.DeleteForkProcessHandler).Methods("DELETE")

	// Health and workflow status
	api.HandleFunc("/health", handleHealth).Methods("GET")

	// Live resource metrics for the running job (cgroup mem/cpu + nvidia-smi).
	api.HandleFunc("/metrics", metricsHandler).Methods("GET")

	// Metadata store — in-memory key-value for shared state
	api.HandleFunc("/metadata", handleMetadata).Methods("GET")
	api.HandleFunc("/metadata/{key:.+}", handleMetadataKey).Methods("GET", "PUT", "DELETE")
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok"}`)
}

func handleMetadata(w http.ResponseWriter, r *http.Request) {
	metadataMu.RLock()
	defer metadataMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metadataStore)
}

func handleMetadataKey(w http.ResponseWriter, r *http.Request) {
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
