// Tests for the parsers and for the sampling task.
//
//	TestParseGPUs
//	TestPollSamplesAndStops  The task must store what nvidia-smi prints; with a slow probe in flight, Latest must
//	                         still answer at once with the last sample and Stop must kill the probe and return.
//	TestSampleCgroup  A sample must read the job path with its step suffix removed.
package metrics

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

func TestParseGPUs(t *testing.T) {
	got := parseGPUs("0, 35, 1024, 40960\nNVIDIA-SMI has failed\n1, 0, 0, 40960\n")
	want := []GPU{{Index: 0, UtilPct: 35, MemUsedMiB: 1024, MemTotalMiB: 40960}, {Index: 1, MemTotalMiB: 40960}}
	if !slices.Equal(got, want) {
		t.Fatalf("parsed %+v, want %+v", got, want)
	}
	if parseGPUs("") != nil {
		t.Fatal("empty output must parse to nil, so the field is omitted")
	}
}

func TestPollSamplesAndStops(t *testing.T) {
	dir := t.TempDir()
	fake := []byte("#!/bin/sh\nif [ -f " + dir + "/slow ]; then /bin/sleep 30; fi\necho '0, 35, 1024, 40960'\n")
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), fake, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	old := interval
	interval = 50 * time.Millisecond
	t.Cleanup(func() { interval = old })

	_, _ = (&tasks.Task{Kind: "metrics", Run: Poll}).Start()
	t.Cleanup(func() { tasks.Stop("metrics") })
	for deadline := time.Now().Add(5 * time.Second); len(Latest().GPUs) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the task never stored a sample")
		}
	}
	if got := Latest().GPUs; !slices.Equal(got, []GPU{{Index: 0, UtilPct: 35, MemUsedMiB: 1024, MemTotalMiB: 40960}}) {
		t.Fatalf("sample = %+v", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "slow"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(interval + 200*time.Millisecond)
	start := time.Now()
	got := Latest()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond || len(got.GPUs) != 1 {
		t.Fatalf("Latest took %s and answered %+v with a probe in flight; want the last sample at once", elapsed, got.GPUs)
	}
	stopStart := time.Now()
	tasks.Stop("metrics")
	if elapsed := time.Since(stopStart); elapsed > 2*time.Second {
		t.Fatalf("Stop took %s; cancellation must kill the probe in flight", elapsed)
	}
}

func TestSampleCgroup(t *testing.T) {
	root := t.TempDir()
	job := filepath.Join(root, "system.slice", "job_7")
	if err := os.MkdirAll(job, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"memory.current": "123456\n",
		"cpu.stat":       "usage_usec 295339339\nuser_usec 1\n",
	} {
		if err := os.WriteFile(filepath.Join(job, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	proc := filepath.Join(root, "cgroup")
	if err := os.WriteFile(proc, []byte("0::/system.slice/job_7/step_0/task_0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldProc, oldRoot := procCgroup, cgroupRoot
	procCgroup, cgroupRoot = proc, root
	t.Cleanup(func() { procCgroup, cgroupRoot = oldProc, oldRoot })

	sample(context.Background())
	got := Latest()
	if got.MemBytes == nil || *got.MemBytes != 123456 {
		t.Fatalf("MemBytes = %v, want 123456", got.MemBytes)
	}
	if got.CPUUsageUsec == nil || *got.CPUUsageUsec != 295339339 {
		t.Fatalf("CPUUsageUsec = %v, want 295339339", got.CPUUsageUsec)
	}
}
