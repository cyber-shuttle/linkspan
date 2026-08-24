// Package metrics reports the running job's live resource use: cgroup-v2 memory
// and cpu, plus per-GPU nvidia-smi. Every source degrades on its own, so a
// missing cgroup file or a CPU node with no driver omits a field rather than
// failing the read.
package metrics

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

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

func Read() Snapshot {
	m := Snapshot{GPUs: readGPUMetrics()}
	if cg, err := jobCgroupDir(); err == nil {
		m.MemBytes = readCgroup(cg+"/memory.current", parseInt64)
		m.CPUUsageUsec = readCgroup(cg+"/cpu.stat", parseCPUUsageUsec)
	}
	return m
}

func readCgroup[T any](path string, parse func(string) (T, error)) *T {
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

// nil when nvidia-smi is absent or errors: a CPU node has no driver.
func readGPUMetrics() []GPU {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,utilization.gpu,memory.used,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	return parseGPUMetrics(string(out))
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
