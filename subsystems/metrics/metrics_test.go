// Tests for the parsers and for the one behaviour that matters on a failing
// node: a wedged nvidia-smi must not wedge Collect.
//
//	TestMain                      Run as the fake nvidia-smi, the test binary
//	                              forks a holder of its stdout into a new
//	                              session and exits, which is what a wedged
//	                              probe looks like to Wait: the group kill
//	                              misses the holder.
//	TestParseGPUs                 A row that does not scan must be skipped, and
//	                              empty output must parse to nil.
//	TestCollectOutlastsHungProbe  A second call must skip the stuck probe, and
//	                              the flag must clear once the probe's Wait
//	                              returns, so no later test inherits it.
//	TestCollectCgroup             The job path must have its step suffix removed.
package metrics

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

const fakeProbe = "LINKSPAN_TEST_FAKE_PROBE"

func TestMain(m *testing.M) {
	switch os.Getenv(fakeProbe) {
	case "hold":
		time.Sleep(3 * time.Second)
	case "probe":
		holder := exec.Command(os.Args[0])
		holder.Env = append(os.Environ(), fakeProbe+"=hold")
		holder.Stdout = os.Stdout
		holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := holder.Start(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	default:
		os.Exit(m.Run())
	}
}

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

func TestCollectOutlastsHungProbe(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fake := []byte("#!/bin/sh\n" + fakeProbe + "=probe exec " + self + "\n")
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), fake, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	old := gpuProbeTimeout
	gpuProbeTimeout = time.Second
	t.Cleanup(func() { gpuProbeTimeout = old })
	done := make(chan struct{})
	start := time.Now()
	go func() { defer close(done); Collect() }()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > gpuProbeTimeout+2*time.Second {
			t.Fatalf("Collect took %s, want it bounded near %s", elapsed, gpuProbeTimeout)
		}
	case <-time.After(gpuProbeTimeout + 5*time.Second):
		t.Fatal("Collect never returned with nvidia-smi hung")
	}

	secondStart := time.Now()
	Collect()
	if elapsed := time.Since(secondStart); elapsed > time.Second {
		t.Fatalf("a second call took %s while a probe was stuck; it must skip", elapsed)
	}
	if !probing.Load() {
		t.Fatal("the stuck probe no longer holds the flag, so a second one could start")
	}
	for deadline := time.Now().Add(10 * time.Second); probing.Load(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the flag never cleared after the probe's Wait returned")
		}
	}
}

func TestCollectCgroup(t *testing.T) {
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

	got := Collect()
	if got.MemBytes == nil || *got.MemBytes != 123456 {
		t.Fatalf("MemBytes = %v, want 123456", got.MemBytes)
	}
	if got.CPUUsageUsec == nil || *got.CPUUsageUsec != 295339339 {
		t.Fatalf("CPUUsageUsec = %v, want 295339339", got.CPUUsageUsec)
	}
}
