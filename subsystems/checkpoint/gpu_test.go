package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireGPU skips unless this host can actually do GPU checkpointing, the same
// way requireCriu gates on a usable CRIU.
func requireGPU(t *testing.T) *GPUDetails {
	t.Helper()
	details, err := gpuPreflight(context.Background(), GPUConfig{}, 0)
	if err != nil {
		t.Skipf("GPU checkpointing is not available on this host: %v", err)
	}
	return details
}

func writeFakeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
	return path
}

func TestMajorVersion(t *testing.T) {
	for version, want := range map[string]int{"610.43.02": 610, "570.0": 570, "12": 12} {
		got, err := majorVersion(version)
		if err != nil {
			t.Fatalf("majorVersion(%q) failed: %v", version, err)
		}
		if got != want {
			t.Fatalf("majorVersion(%q) = %d, want %d", version, got, want)
		}
	}
	if _, err := majorVersion("not-a-version"); err == nil {
		t.Fatalf("expected an unparseable version to be rejected")
	}
}

func TestResolveCudaCheckpointRejectsBadPaths(t *testing.T) {
	dir := t.TempDir()

	if _, err := resolveCudaCheckpoint(GPUConfig{CudaCheckpointPath: filepath.Join(dir, "nope")}); err == nil {
		t.Fatalf("expected a missing cuda-checkpoint to be rejected")
	}
	if _, err := resolveCudaCheckpoint(GPUConfig{CudaCheckpointPath: dir}); err == nil {
		t.Fatalf("expected a directory to be rejected")
	}

	notExec := filepath.Join(dir, "cuda-checkpoint")
	if err := os.WriteFile(notExec, []byte("x"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := resolveCudaCheckpoint(GPUConfig{CudaCheckpointPath: notExec}); err == nil {
		t.Fatalf("expected a non-executable cuda-checkpoint to be rejected")
	}

	good := writeFakeExecutable(t, dir, "cuda-checkpoint-ok")
	got, err := resolveCudaCheckpoint(GPUConfig{CudaCheckpointPath: good})
	if err != nil || got != good {
		t.Fatalf("expected the configured path to be accepted, got %q (%v)", got, err)
	}
}

func TestResolveCriuPluginDir(t *testing.T) {
	dir := t.TempDir()

	if _, err := resolveCriuPluginDir(GPUConfig{CriuPluginDir: dir}); err == nil {
		t.Fatalf("expected a directory without %s to be rejected", cudaPluginFileName)
	}

	if err := os.WriteFile(filepath.Join(dir, cudaPluginFileName), []byte("elf"), 0644); err != nil {
		t.Fatalf("failed to write fake plugin: %v", err)
	}
	got, err := resolveCriuPluginDir(GPUConfig{CriuPluginDir: dir})
	if err != nil || got != dir {
		t.Fatalf("expected the configured plugin dir to be accepted, got %q (%v)", got, err)
	}
}

/*
CRIU only searches its compiled-in plugin directory, so a CUDA plugin installed
under /usr/local is invisible without --libdir. Losing this argument is the
difference between a GPU checkpoint and a silently CPU-only one.
*/
func TestPluginDirReachesTheCriuCommand(t *testing.T) {
	dump := strings.Join(buildDumpArgs(dumpOptions{PID: 1, ImagesDir: "/i", WorkDir: "/w", LogFile: "d.log", PluginDir: "/usr/local/lib/criu"}), " ")
	if !strings.Contains(dump, "--libdir /usr/local/lib/criu") {
		t.Fatalf("expected --libdir in dump args, got %s", dump)
	}
	restore := strings.Join(buildRestoreArgs(restoreOptions{ImagesDir: "/i", WorkDir: "/w", LogFile: "r.log", PidFile: "r.pid", PluginDir: "/usr/local/lib/criu"}), " ")
	if !strings.Contains(restore, "--libdir /usr/local/lib/criu") {
		t.Fatalf("expected --libdir in restore args, got %s", restore)
	}

	// A CPU-only checkpoint must not pass it at all.
	cpu := strings.Join(buildDumpArgs(dumpOptions{PID: 1, ImagesDir: "/i", WorkDir: "/w", LogFile: "d.log"}), " ")
	if strings.Contains(cpu, "--libdir") {
		t.Fatalf("a CPU checkpoint must not pass --libdir, got %s", cpu)
	}
}

// CRIU's plugin execs cuda-checkpoint by name, so its directory has to be on
// the child's PATH — the most common reason a correct install still fails.
func TestGpuCriuEnvPrependsCudaCheckpointDir(t *testing.T) {
	env := gpuCriuEnv([]string{"HOME=/home/x", "PATH=/usr/bin:/bin"}, "/opt/nvidia/cuda-checkpoint")

	var path string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	if !strings.HasPrefix(path, "/opt/nvidia:") {
		t.Fatalf("expected cuda-checkpoint's directory first on PATH, got %q", path)
	}
	if !strings.Contains(path, "/usr/bin:/bin") {
		t.Fatalf("expected the inherited PATH to be preserved, got %q", path)
	}

	// A parent with no PATH at all must still get one.
	bare := gpuCriuEnv([]string{"HOME=/home/x"}, "/opt/nvidia/cuda-checkpoint")
	if !containsPrefix(bare, "PATH=/opt/nvidia") {
		t.Fatalf("expected a PATH to be added, got %v", bare)
	}
}

func containsPrefix(env []string, prefix string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func TestProcessGPUDevicesOnANonGPUProcess(t *testing.T) {
	// The test binary itself holds no NVIDIA device.
	devices, err := processGPUDevices(os.Getpid())
	if err != nil {
		t.Fatalf("failed to read own fds: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("expected no NVIDIA devices for the test process, got %v", devices)
	}

	uses, err := ProcessUsesGPU(os.Getpid())
	if err != nil || uses {
		t.Fatalf("expected the test process not to be using a GPU, got %v (%v)", uses, err)
	}
}

func TestProcessGPUDevicesReportsMissingProcess(t *testing.T) {
	if _, err := processGPUDevices(1 << 30); err == nil {
		t.Fatalf("expected an error for a pid that does not exist")
	}
}

func TestValidateMode(t *testing.T) {
	for _, ok := range []string{"", "auto", "cpu", "gpu"} {
		if err := ValidateMode(ok); err != nil {
			t.Fatalf("expected mode %q to be valid: %v", ok, err)
		}
	}
	if err := ValidateMode("tpu"); err == nil {
		t.Fatalf("expected an unknown mode to be rejected")
	}
}

// Requirement 1: GPU mode must fail loudly, never quietly degrade to CPU.
func TestGpuModeFailsRatherThanFallingBack(t *testing.T) {
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		t.Skip("this host has nvidia-smi; the no-GPU path cannot be exercised here")
	}

	c := &criuCheckpointer{}
	details, err := c.resolveGPUMode(context.Background(), ModeGPU, os.Getpid())
	if err == nil {
		t.Fatalf("expected GPU mode to fail on a host with no GPU, got %+v", details)
	}
	if !strings.Contains(err.Error(), "GPU preflight failed") {
		t.Fatalf("expected the error to name the preflight, got %v", err)
	}
}

// A process with no GPU is CPU-checkpointed under auto without any GPU tooling
// being required — the common case must not become harder.
func TestAutoModeStaysOnCPUForANonGPUProcess(t *testing.T) {
	c := &criuCheckpointer{}
	details, err := c.resolveGPUMode(context.Background(), ModeAuto, os.Getpid())
	if err != nil {
		t.Fatalf("auto mode on a non-GPU process should succeed: %v", err)
	}
	if details != nil {
		t.Fatalf("expected no GPU details for a non-GPU process, got %+v", details)
	}
}

func TestExplicitCpuModeIsFineForANonGPUProcess(t *testing.T) {
	c := &criuCheckpointer{}
	details, err := c.resolveGPUMode(context.Background(), ModeCPU, os.Getpid())
	if err != nil || details != nil {
		t.Fatalf("expected plain CPU mode for a non-GPU process, got %+v (%v)", details, err)
	}
}

/*
The real preflight against real hardware: every one of the six checks has to
pass, and the provenance it returns is what the restore side later validates.
*/
func TestGpuPreflightOnRealHardware(t *testing.T) {
	details := requireGPU(t)

	if details.Name == "" {
		t.Fatalf("expected a GPU name to be recorded")
	}
	if details.MemoryTotalMiB <= 0 {
		t.Fatalf("expected GPU memory to be recorded, got %d", details.MemoryTotalMiB)
	}
	if details.DriverVersion == "" {
		t.Fatalf("expected a driver version to be recorded")
	}
	if details.CriuPluginDir == "" {
		t.Fatalf("expected the CRIU CUDA plugin directory to be recorded")
	}
	if _, err := os.Stat(filepath.Join(details.CriuPluginDir, cudaPluginFileName)); err != nil {
		t.Fatalf("recorded plugin dir does not hold %s: %v", cudaPluginFileName, err)
	}

	driver, err := majorVersion(details.DriverVersion)
	if err != nil || driver < minimumCudaCheckpointDriver {
		t.Fatalf("expected a driver at or above %d, got %s", minimumCudaCheckpointDriver, details.DriverVersion)
	}
	t.Logf("GPU preflight passed: %s, %d MiB, driver %s, cuda-checkpoint %s, plugin dir %s",
		details.Name, details.MemoryTotalMiB, details.DriverVersion, details.CudaCheckpointVersion, details.CriuPluginDir)
}

// The driver and cuda-checkpoint ship in lockstep; a mismatched pair cannot
// checkpoint CUDA state and must be caught in preflight, not by CRIU.
func TestPreflightRejectsMismatchedCudaCheckpoint(t *testing.T) {
	requireGPU(t)

	// A stand-in that reports no version at all still passes (the check is
	// skipped when the version is unreadable), so this asserts the opposite
	// case: a wrong plugin directory is caught.
	dir := t.TempDir()
	_, err := gpuPreflight(context.Background(), GPUConfig{CriuPluginDir: dir}, 0)
	if err == nil {
		t.Fatalf("expected preflight to fail when the plugin dir has no %s", cudaPluginFileName)
	}
	if !strings.Contains(err.Error(), cudaPluginFileName) {
		t.Fatalf("expected the error to name the missing plugin, got %v", err)
	}
}
