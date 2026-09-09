// Package metrics reports the job's current use: memory and CPU from its
// cgroup v2 hierarchy, GPU utilisation from nvidia-smi. A source that cannot
// be read leaves its field unset.
//
//	GPU, Snapshot
//	parseGPUs      It skips rows that do not scan, because a failed nvidia-smi
//	               prints its complaint to stdout.
//	readCgroupInt  It reads the integer on the line starting with key; an empty
//	               key takes the first line that parses.
//	Collect        The nvidia-smi probe runs outside procmgr behind the probing
//	               flag and is killed at gpuProbeTimeout. A wedged nvidia-smi
//	               ignores SIGKILL, so the call gives up at the timeout, the flag
//	               stays held until Wait returns, and no second probe starts. The
//	               job's cgroup is the 0:: entry of /proc/self/cgroup without its
//	               step suffix. procCgroup and cgroupRoot are test seams.
package metrics

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/procmgr"
)

type GPU struct {
	Index       int `json:"index"`
	UtilPct     int `json:"utilPct"`
	MemUsedMiB  int `json:"memUsedMiB"`
	MemTotalMiB int `json:"memTotalMiB"`
}

type Snapshot struct {
	MemBytes     *int64 `json:"memBytes,omitempty"`
	CPUUsageUsec *int64 `json:"cpuUsageUsec,omitempty"`
	GPUs         []GPU  `json:"gpus,omitempty"`
}

var gpuProbeTimeout = 3 * time.Second

var probing atomic.Bool

var (
	procCgroup = "/proc/self/cgroup"
	cgroupRoot = "/sys/fs/cgroup"
)

func parseGPUs(out string) []GPU {
	var gpus []GPU
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var g GPU
		if _, err := fmt.Sscanf(line, "%d, %d, %d, %d", &g.Index, &g.UtilPct, &g.MemUsedMiB, &g.MemTotalMiB); err == nil {
			gpus = append(gpus, g)
		}
	}
	return gpus
}

func readCgroupInt(path, key string) *int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if s, ok := strings.CutPrefix(line, key); ok {
			if v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				return &v
			}
		}
	}
	return nil
}

func Collect() Snapshot {
	var snap Snapshot
	if probing.CompareAndSwap(false, true) {
		ctx, cancel := context.WithTimeout(context.Background(), gpuProbeTimeout)
		defer cancel()
		var out bytes.Buffer
		cmd := exec.Command("nvidia-smi",
			"--query-gpu=index,utilization.gpu,memory.used,memory.total",
			"--format=csv,noheader,nounits")
		cmd.Stdout = &out
		done := make(chan struct{})
		go func() {
			defer probing.Store(false)
			defer close(done)
			_ = procmgr.Exec(ctx, cmd)
		}()
		select {
		case <-done:
			snap.GPUs = parseGPUs(out.String())
		case <-ctx.Done():
		}
	}
	if b, err := os.ReadFile(procCgroup); err == nil {
		_, path, _ := strings.Cut(string(b), "0::")
		job, _, _ := strings.Cut(strings.TrimSpace(path), "/step_")
		snap.MemBytes = readCgroupInt(cgroupRoot+job+"/memory.current", "")
		snap.CPUUsageUsec = readCgroupInt(cgroupRoot+job+"/cpu.stat", "usage_usec ")
	}
	return snap
}
