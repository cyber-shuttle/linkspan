package metrics

import "testing"

func TestJobCgroupSuffix(t *testing.T) {
	cases := map[string]string{
		// live srun step → strip to the job level
		"0::/system.slice/slurmstepd.scope/job_20228429/step_1/user/task_0": "/system.slice/slurmstepd.scope/job_20228429",
		// no step segment → unchanged
		"0::/user.slice/user-1000.slice": "/user.slice/user-1000.slice",
		// multi-line (cgroup v1-style) → last line wins
		"12:cpu:/a\n0::/b/step_0/task": "/b",
	}
	for in, want := range cases {
		if got := jobCgroupSuffix(in); got != want {
			t.Errorf("jobCgroupSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseCPUUsageUsec(t *testing.T) {
	got, err := parseCPUUsageUsec("usage_usec 295339339\nuser_usec 100\nsystem_usec 50\n")
	if err != nil || got != 295339339 {
		t.Fatalf("got (%d, %v), want (295339339, nil)", got, err)
	}
	if _, err := parseCPUUsageUsec("user_usec 1\n"); err == nil {
		t.Error("expected an error when usage_usec is absent")
	}
}

func TestParseGPUMetrics(t *testing.T) {
	got := parseGPUMetrics("0, 15, 1024, 40960\n1, 0, 0, 40960\n")
	want := []gpuMetric{
		{Index: 0, UtilPct: 15, MemUsedMiB: 1024, MemTotalMiB: 40960},
		{Index: 1, UtilPct: 0, MemUsedMiB: 0, MemTotalMiB: 40960},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if parseGPUMetrics("") != nil {
		t.Error("empty nvidia-smi output should yield nil GPUs")
	}
}
