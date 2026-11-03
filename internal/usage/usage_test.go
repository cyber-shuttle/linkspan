// Tests run the sampler against fixture cgroup, /proc and /sys trees, with fakes in place of NVML and DCGM, which
// only a GPU node has.
//
//	write, seam, has
//	TestDetect, TestABI, TestGPUs, TestSample, TestPollSamplesAndStops
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

func write(t *testing.T, files map[string]string) {
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func seam[T any](t *testing.T, v *T, to T) {
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

func has(t *testing.T, v any, wants ...string) {
	t.Helper()
	b, _ := json.Marshal(v)
	for _, want := range wants {
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s lacks %s", b, want)
		}
	}
}

func TestDetect(t *testing.T) {
	root := t.TempDir()
	seam(t, &cgroupRoot, root)
	seam(t, &procCgroup, filepath.Join(root, "self"))
	scope := root + "/system.slice/slurmstepd.scope"
	write(t, map[string]string{scope + "/cpuset.cpus.effective": "0-63\n", scope + "/job_7/cpuset.cpus.effective": "0-63\n",
		scope + "/job_8/cpuset.cpus.effective": "0-3\n"})
	check := func(cgroup, wantCgroup string, wantWhole bool) {
		write(t, map[string]string{procCgroup: cgroup})
		if j := detect(); j.cgroup != wantCgroup || j.wholeNode != wantWhole {
			t.Fatalf("detect = %+v, want cgroup %q and wholeNode %v", j, wantCgroup, wantWhole)
		}
	}

	check("0::/system.slice/slurmstepd.scope/job_7/step_batch/user/task_0\n", scope+"/job_7", true)
	check("0::/system.slice/slurmstepd.scope/job_8/step_0/user/task_0\n", scope+"/job_8", false)
	check("0::/user.slice/user-1.slice/session-3.scope\n", "", false)
}

func TestABI(t *testing.T) {
	for _, c := range []struct {
		name       string
		size, want uintptr
	}{
		{"dcgmValue", unsafe.Sizeof(dcgmValue{}), dcgmValueSize}, {"nvmlMemory", unsafe.Sizeof(nvmlMemory{}), 40},
		{"nvmlField", unsafe.Sizeof(nvmlField{}), 40}, {"nvmlProcess", unsafe.Sizeof(nvmlProcess{}), 24},
	} {
		if c.size != c.want {
			t.Fatalf("%s is %d bytes, want %d", c.name, c.size, c.want)
		}
	}
}

func TestGPUs(t *testing.T) {
	up, opens := false, 0
	seam(t, &openNVML, func() ([]gpu, func() map[string]map[string]any, func() map[int]int64) {
		return []gpu{{0, "GPU-a", "NVIDIA B200", 0}, {1, "GPU-b", "NVIDIA B200", 0}},
			func() map[string]map[string]any { return map[string]map[string]any{"GPU-a": {"utilPct": 1.0}} }, nil
	})
	seam(t, &openDCGM, func([]gpu) func() map[string]map[string]any {
		opens++
		live := up
		return func() map[string]map[string]any {
			if !live || !up {
				return nil
			}
			return map[string]map[string]any{"GPU-a": {"tensorActive": 0.58}}
		}
	})
	gpus, _ := openGPUs()
	for i, c := range []struct {
		up    bool
		want  string
		opens int
	}{{false, "nvml", 2}, {true, "nvml", 3}, {true, "dcgm", 3}, {false, "nvml", 4}, {true, "nvml", 5}, {true, "dcgm", 5}} {
		up = c.up
		source, entries := gpus()
		if *source != c.want || opens != c.opens {
			t.Fatalf("sample %d: source %s after %d opens, want %s after %d", i, *source, opens, c.want, c.opens)
		}
		has(t, entries, map[string]string{"dcgm": `"tensorActive":0.58,"throttleReasons":null`, "nvml": `"utilPct":1,"uuid":"GPU-a"`}[c.want],
			`"utilPct":null,"uuid":"GPU-b"`)
	}
}

func TestSample(t *testing.T) {
	root := t.TempDir()
	seam(t, &procRoot, filepath.Join(root, "proc"))
	seam(t, &sysClass, filepath.Join(root, "class"))
	cg := filepath.Join(root, "job_7")
	ib := filepath.Join(sysClass, "infiniband/mlx5_0/ports/1")
	write(t, map[string]string{
		cg + "/memory.current":                             "123456\n",
		cg + "/memory.stat":                                "anon_thp 0\nanon 100\nfile 20\n",
		cg + "/memory.events":                              "oom_kill 1\n",
		cg + "/cpu.stat":                                   "usage_usec 295339339\nsystem_usec 5\n",
		cg + "/step_0/user/task_0/cgroup.procs":            "81\n82\n83\n",
		procRoot + "/81/comm":                              "python\n",
		procRoot + "/81/status":                            "Name:\tpython\nVmRSS:\t   2048 kB\n",
		procRoot + "/83/comm":                              "bash\n",
		ib + "/link_layer":                                 "InfiniBand\n",
		ib + "/counters/port_xmit_data":                    "10\n",
		ib + "/counters/port_rcv_data":                     "20\n",
		sysClass + "/infiniband/mlx5_1/ports/1/link_layer": "Ethernet\n",
		sysClass + "/net/eno1/type":                        "1\n",
		sysClass + "/net/eno1/device/vendor":               "0x8086\n",
		sysClass + "/net/eno1/statistics/tx_bytes":         "30\n",
		sysClass + "/net/eno1/statistics/rx_bytes":         "40\n",
		sysClass + "/net/ib0/type":                         "32\n",
		sysClass + "/net/ib0/device/vendor":                "0x15b3\n",
	})
	source := "nvml"
	gpus := func() (*string, []map[string]any) { return &source, nil }
	seam(t, &openNVML, func() ([]gpu, func() map[string]map[string]any, func() map[int]int64) { return nil, nil, nil })
	none, noMem := openGPUs()

	has(t, sample(job{cgroup: cg, gpus: gpus, gpuMem: func() map[int]int64 { return map[int]int64{81: 41984} }, wholeNode: true}),
		`"memBytes":123456`, `"memAnonBytes":100`, `"memFileBytes":20`, `"oomKills":1`,
		`"cpuUsageUsec":295339339`, `"cpuSystemUsec":5`,
		`"processes":[{"pid":81,"name":"python","rssBytes":2097152,"gpuMemMiB":41984},{"pid":83,"name":"bash","rssBytes":null,"gpuMemMiB":0}]`,
		`"gpuSource":"nvml"`,
		`"network":{"eno1":{"type":"ethernet","xmitBytes":30,"rcvBytes":40},"mlx5_0/1":{"type":"infiniband","xmitBytes":40,"rcvBytes":80}}`)

	has(t, sample(job{cgroup: cg, gpus: gpus, gpuMem: func() map[int]int64 { return nil }}), `"rssBytes":null,"gpuMemMiB":null}`)
	has(t, sample(job{cgroup: cg, gpus: none, gpuMem: noMem}), `"gpuMemMiB":0}],"gpuSource":null,"gpus":[],"network":null}`)
	has(t, sample(job{gpus: none, gpuMem: noMem}), `Z","memBytes":null`, `"processes":null,"gpuSource":null,"gpus":[],"network":null}`)
}

func TestPollSamplesAndStops(t *testing.T) {
	slow := filepath.Join(t.TempDir(), "slow")
	release, released := make(chan struct{}), make(chan struct{})
	seam(t, &openNVML, func() ([]gpu, func() map[string]map[string]any, func() map[int]int64) {
		return []gpu{{0, "GPU-a", "NVIDIA B200", 0}}, func() map[string]map[string]any {
			if _, err := os.Stat(slow); err == nil {
				<-release
				defer close(released)
			}
			return map[string]map[string]any{"GPU-a": {"utilPct": 35.0}}
		}, nil
	})
	seam(t, &interval, 50*time.Millisecond)
	last.Store(nil)
	has(t, Latest(), `{"sampledAt":null,`)

	_, _ = (&tasks.Task{Kind: "usage", Run: Poll}).Start()
	t.Cleanup(func() { tasks.Stop("usage") })
	for deadline := time.Now().Add(5 * time.Second); Latest().SampledAt == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the task never stored a sample")
		}
	}
	has(t, Latest().GPUs, `"utilPct":35`)

	write(t, map[string]string{slow: ""})
	t.Cleanup(func() {
		close(release)
		<-released
	})
	time.Sleep(interval + 200*time.Millisecond)
	start := time.Now()
	got := Latest()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond || len(got.GPUs) != 1 {
		t.Fatalf("Latest took %s and answered %v with a probe in flight; want the last sample at once", elapsed, got.GPUs)
	}
	stopStart := time.Now()
	tasks.Stop("usage")
	if elapsed := time.Since(stopStart); elapsed > 2*time.Second {
		t.Fatalf("Stop took %s; it must not wait for a sample in flight", elapsed)
	}
}
