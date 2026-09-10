// Package metrics reports the job's resource use: memory and CPU from its cgroup v2 hierarchy, and GPU utilisation
// and memory from nvidia-smi. A source that cannot be read leaves its field unset. A task samples the whole set,
// so a request answers from the last sample without waiting on anything.
//
//	GPU, Snapshot
//	interval, procCgroup, cgroupRoot  Test seams.
//	last           The last sample; nil before the first.
//	parseGPUs      Skips rows that do not scan, because a failed nvidia-smi prints its complaint to stdout.
//	readCgroupInt  An empty key takes the first line that parses.
//	sample         One whole set: the nvidia-smi probe through tasks.Exec, waited for however long it takes, then the
//	               cgroup files. The job's cgroup is the 0:: entry of /proc/self/cgroup without its step suffix.
//	Latest         The last sample, or an empty one before the first.
//	Poll           The task main starts: a sample, then interval, until cancelled. Cancellation kills a slow
//	               probe; one that ignores SIGKILL holds the task, and StopAll with it.
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

	"github.com/cyber-shuttle/linkspan/internal/tasks"
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

var (
	interval   = 5 * time.Second
	procCgroup = "/proc/self/cgroup"
	cgroupRoot = "/sys/fs/cgroup"
)

var last atomic.Pointer[Snapshot]

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

func sample(ctx context.Context) {
	var snap Snapshot
	var out bytes.Buffer
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=index,utilization.gpu,memory.used,memory.total",
		"--format=csv,noheader,nounits")
	cmd.Stdout = &out
	if err := tasks.Exec(ctx, cmd); err == nil {
		snap.GPUs = parseGPUs(out.String())
	}
	if b, err := os.ReadFile(procCgroup); err == nil {
		_, path, _ := strings.Cut(string(b), "0::")
		job, _, _ := strings.Cut(strings.TrimSpace(path), "/step_")
		snap.MemBytes = readCgroupInt(cgroupRoot+job+"/memory.current", "")
		snap.CPUUsageUsec = readCgroupInt(cgroupRoot+job+"/cpu.stat", "usage_usec ")
	}
	last.Store(&snap)
}

func Latest() Snapshot {
	if snap := last.Load(); snap != nil {
		return *snap
	}
	return Snapshot{}
}

func Poll(ctx context.Context) error {
	for {
		sample(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
