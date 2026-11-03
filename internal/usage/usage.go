// Package usage reports what the job uses. What the job is gets read once at start; a task then samples every
// interval, and a request answers from the last sample. An unreadable value is left out.
// Library calls cannot be cancelled, so a sample runs apart and a stop does not wait for it.
//
//	Sample, job
//	interval, procCgroup, cgroupRoot, procRoot, sysClass  Test seams.
//	readInt, scale
//	detect  The job holds the node when its cgroup has every CPU of Slurm's parent cgroup.
//	sample, Latest, Poll
package usage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Sample struct {
	SampledAt     *time.Time       `json:"sampledAt,omitempty"`
	MemBytes      *int64           `json:"memBytes,omitempty"`
	MemAnonBytes  *int64           `json:"memAnonBytes,omitempty"`
	MemFileBytes  *int64           `json:"memFileBytes,omitempty"`
	OOMKills      *int64           `json:"oomKills,omitempty"`
	CPUUsageUsec  *int64           `json:"cpuUsageUsec,omitempty"`
	CPUSystemUsec *int64           `json:"cpuSystemUsec,omitempty"`
	Processes     []Process        `json:"processes,omitempty"`
	GPUSource     *string          `json:"gpuSource,omitempty"`
	GPUs          []map[string]any `json:"gpus,omitempty"`
	Network       map[string]Link  `json:"network,omitempty"`
}

type job struct {
	cgroup    string
	wholeNode bool
	gpus      func() (*string, []map[string]any)
	gpuMem    func() map[int]int64
}

var (
	interval   = 5 * time.Second
	procCgroup = "/proc/self/cgroup"
	cgroupRoot = "/sys/fs/cgroup"
	procRoot   = "/proc"
	sysClass   = "/sys/class"
)

var (
	last      atomic.Pointer[Sample]
	jobCgroup = regexp.MustCompile(`(?m)^0::(.*?)/step_`)
)

func readInt(path, key string) *int64 {
	b, _ := os.ReadFile(path)
	for line := range strings.Lines(string(b)) {
		if s, ok := strings.CutPrefix(line, key); ok {
			if f := strings.Fields(s); len(f) > 0 {
				if v, err := strconv.ParseInt(f[0], 10, 64); err == nil {
					return &v
				}
			}
		}
	}
	return nil
}

func scale(v *int64, by int64) *int64 {
	if v != nil {
		*v *= by
	}
	return v
}

func detect() job {
	var j job
	if b, err := os.ReadFile(procCgroup); err == nil {
		if m := jobCgroup.FindSubmatch(b); m != nil {
			j.cgroup = cgroupRoot + string(m[1])
			mine, _ := os.ReadFile(j.cgroup + "/cpuset.cpus.effective")
			all, _ := os.ReadFile(filepath.Dir(j.cgroup) + "/cpuset.cpus.effective")
			j.wholeNode = len(mine) > 1 && bytes.Equal(mine, all)
		}
	}
	j.gpus, j.gpuMem = openGPUs()
	return j
}

func sample(j job) Sample {
	now := time.Now().UTC()
	s := Sample{SampledAt: &now}
	s.GPUSource, s.GPUs = j.gpus()
	if c := j.cgroup; c != "" {
		s.MemBytes = readInt(c+"/memory.current", "")
		s.MemAnonBytes = readInt(c+"/memory.stat", "anon ")
		s.MemFileBytes = readInt(c+"/memory.stat", "file ")
		s.OOMKills = readInt(c+"/memory.events", "oom_kill ")
		s.CPUUsageUsec = readInt(c+"/cpu.stat", "usage_usec ")
		s.CPUSystemUsec = readInt(c+"/cpu.stat", "system_usec ")
		s.Processes = readProcesses(j)
	}
	if j.wholeNode {
		s.Network = readNetwork()
	}
	return s
}

func Latest() Sample {
	if s := last.Load(); s != nil {
		return *s
	}
	return Sample{}
}

func Poll(ctx context.Context) error {
	var j *job
	for {
		done := make(chan struct{})
		go func() {
			if j == nil {
				d := detect()
				j = &d
			}
			s := sample(*j)
			last.Store(&s)
			close(done)
		}()
		select {
		case <-ctx.Done():
			return nil
		case <-done:
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
