// Package metrics reports the running job's live resource use: cgroup-v2 memory
// and cpu, plus per-GPU nvidia-smi. Every source degrades on its own, so a
// missing cgroup file or a CPU node with no driver omits a field rather than
// failing the read.
package metrics

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// nvidia-smi blocks indefinitely on an unhealthy GPU, which is exactly the state
// the metrics are wanted for. Killing it is not enough: a process wedged in an
// uninterruptible driver call ignores SIGKILL until the call returns, and Wait
// blocks on it regardless of the context. So the read gives up on the probe
// rather than the probe giving up, and only one may ever be outstanding --
// cs-bridge polls every 5s, which would otherwise pile up a stuck process per
// poll for as long as the GPU stays sick.
const gpuProbeTimeout = 3 * time.Second

var gpuProbing atomic.Bool

type GPU struct {
	Index       int `json:"index"`
	UtilPct     int `json:"utilPct"`
	MemUsedMiB  int `json:"memUsedMiB"`
	MemTotalMiB int `json:"memTotalMiB"`
}

// Snapshot mirrors the csbridge MetricSample live fields. Every source is read
// independently and omitted when unavailable, so the client sees undefined
// rather than a misleading zero.
type Snapshot struct {
	MemBytes     *int64 `json:"memBytes,omitempty"`
	CPUUsageUsec *int64 `json:"cpuUsageUsec,omitempty"`
	GPUs         []GPU  `json:"gpus,omitempty"`
}

func Read(ctx context.Context) Snapshot {
	m := Snapshot{GPUs: readGPUMetrics(ctx)}
	if cg, err := jobCgroupDir(); err == nil {
		m.MemBytes = readCgroup(cg+"/memory.current", parseInt64)
		m.CPUUsageUsec = readCgroup(cg+"/cpu.stat", parseCPUUsageUsec)
	}
	return m
}

func readCgroup(path string, parse func(string) (int64, error)) *int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := parse(string(b))
	if err != nil {
		return nil
	}
	return &v
}

// Strips the /step_* leaf so metrics cover the whole allocation, not one step:
// `sed 's|^0::||; s|/step_.*||' /proc/self/cgroup`.
func jobCgroupDir() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	return "/sys/fs/cgroup" + jobCgroupSuffix(string(b)), nil
}

func jobCgroupSuffix(procCgroup string) string {
	lines := strings.Split(strings.TrimSpace(procCgroup), "\n")
	p := strings.TrimPrefix(strings.TrimSpace(lines[len(lines)-1]), "0::")
	if i := strings.Index(p, "/step_"); i >= 0 {
		p = p[:i]
	}
	return p
}

func parseInt64(s string) (int64, error) { return strconv.ParseInt(strings.TrimSpace(s), 10, 64) }

func parseCPUUsageUsec(cpuStat string) (int64, error) {
	for ln := range strings.SplitSeq(cpuStat, "\n") {
		if v, ok := strings.CutPrefix(ln, "usage_usec "); ok {
			return parseInt64(v)
		}
	}
	return 0, fmt.Errorf("usage_usec not found in cpu.stat")
}

// nil when nvidia-smi is absent, errors, outruns gpuProbeTimeout, or when an
// earlier probe is still stuck: a CPU node has no driver, and a sick one must
// not hold the handler open or accumulate processes.
func readGPUMetrics(ctx context.Context) []GPU {
	if !gpuProbing.CompareAndSwap(false, true) {
		return nil
	}
	probed := make(chan []GPU, 1)
	go func() {
		defer gpuProbing.Store(false)
		out, err := exec.CommandContext(ctx, "nvidia-smi",
			"--query-gpu=index,utilization.gpu,memory.used,memory.total",
			"--format=csv,noheader,nounits").Output()
		if err != nil {
			probed <- nil
			return
		}
		probed <- parseGPUMetrics(string(out))
	}()

	select {
	case gpus := <-probed:
		return gpus
	case <-time.After(gpuProbeTimeout):
		return nil // still running; gpuProbing stays set until it finally exits
	}
}

func parseGPUMetrics(out string) []GPU {
	var gpus []GPU
	for ln := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var g GPU
		if _, err := fmt.Sscanf(ln, "%d, %d, %d, %d", &g.Index, &g.UtilPct, &g.MemUsedMiB, &g.MemTotalMiB); err == nil {
			gpus = append(gpus, g)
		}
	}
	return gpus
}
