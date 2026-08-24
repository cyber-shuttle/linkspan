package checkpoint

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

/*
GPU checkpointing has a narrow supported contract, and the checks in this file
enforce it rather than discovering it mid-dump:

  - a single Linux process
  - a single NVIDIA GPU
  - the same GPU model class on restore, with at least as much memory
  - the same driver and CUDA runtime generation

Multi-GPU, NCCL, MPI, distributed PyTorch, and heterogeneous migration are
explicitly out of scope and are refused with a clear error, not attempted.
*/

// minimumCudaCheckpointDriver is the oldest NVIDIA driver whose cuda-checkpoint
// utility works with CRIU's CUDA plugin.
const minimumCudaCheckpointDriver = 570

// cudaPluginFileName is the plugin CRIU loads to drive cuda-checkpoint.
const cudaPluginFileName = "cuda_plugin.so"

// defaultCriuPluginDirs are searched in order when no plugin directory is
// configured. CRIU's own default is only /usr/lib/criu, so a plugin installed
// under /usr/local is invisible to it without --libdir.
var defaultCriuPluginDirs = []string{"/usr/local/lib/criu", "/usr/lib/criu", "/usr/lib64/criu"}

// nvidiaDeviceRE matches the per-GPU character devices a CUDA process opens.
// /dev/nvidiactl and /dev/nvidia-uvm are control nodes shared by every GPU, so
// only the numbered ones identify which devices are actually in use.
var nvidiaDeviceRE = regexp.MustCompile(`^/dev/nvidia(\d+)$`)

// GPUConfig is the CRIU CUDA plugin configuration. It is first-class because
// neither piece is reliably discoverable: the plugin is often installed outside
// CRIU's default --libdir, and cuda-checkpoint is often not on PATH.
type GPUConfig struct {
	CudaCheckpointPath string // NVIDIA's cuda-checkpoint binary; looked up on PATH when empty
	CriuPluginDir      string // directory holding cuda_plugin.so; probed when empty
}

// GPUDetails is the GPU provenance recorded in a manifest and re-checked on
// restore. It is what makes "same GPU model class, enough memory" enforceable.
type GPUDetails struct {
	Count                 int    `json:"count"`
	Name                  string `json:"name"`
	MemoryTotalMiB        int    `json:"memory_total_mib"`
	DriverVersion         string `json:"driver_version"`
	CudaCheckpointVersion string `json:"cuda_checkpoint_version,omitempty"`
	CriuPluginDir         string `json:"criu_plugin_dir,omitempty"`
}

type gpuDevice struct {
	Name           string
	MemoryTotalMiB int
	DriverVersion  string
}

// nvidiaSmi resolves nvidia-smi, which is the first thing a GPU preflight needs.
func nvidiaSmi() (string, error) {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return "", fmt.Errorf("nvidia-smi was not found on PATH: %w", err)
	}
	return path, nil
}

// queryGPUs asks nvidia-smi for the fields the contract is written in.
func queryGPUs(ctx context.Context) ([]gpuDevice, error) {
	path, err := nvidiaSmi()
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, path,
		"--query-gpu=name,memory.total,driver_version", "--format=csv,noheader,nounits")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var devices []gpuDevice
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		memory, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
		devices = append(devices, gpuDevice{
			Name:           strings.TrimSpace(parts[0]),
			MemoryTotalMiB: memory,
			DriverVersion:  strings.TrimSpace(parts[2]),
		})
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("nvidia-smi reported no GPUs")
	}
	return devices, nil
}

// resolveCudaCheckpoint finds NVIDIA's cuda-checkpoint utility. CRIU's plugin
// execs it by name from $PATH, so the resolved path is also what tells us which
// directory has to be on the CRIU child's PATH.
func resolveCudaCheckpoint(cfg GPUConfig) (string, error) {
	if cfg.CudaCheckpointPath != "" {
		info, err := os.Stat(cfg.CudaCheckpointPath)
		if err != nil {
			return "", fmt.Errorf("cuda-checkpoint not found at %s: %w", cfg.CudaCheckpointPath, err)
		}
		if info.IsDir() || info.Mode()&0111 == 0 {
			return "", fmt.Errorf("cuda-checkpoint at %s is not an executable file", cfg.CudaCheckpointPath)
		}
		return cfg.CudaCheckpointPath, nil
	}

	path, err := exec.LookPath("cuda-checkpoint")
	if err != nil {
		return "", fmt.Errorf("cuda-checkpoint was not found on PATH and --cuda-checkpoint-path is not set: %w", err)
	}
	return path, nil
}

// cudaCheckpointVersion reads the utility's own version, which NVIDIA keeps in
// lockstep with the driver. Best-effort: an unreadable version is not fatal on
// its own, the driver comparison below is what actually gates.
func cudaCheckpointVersion(ctx context.Context, path string) string {
	cmd := exec.CommandContext(ctx, path, "--help")
	out, _ := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		// The line reads "Version 610.43.02. Copyright (C) ...", so take only
		// the first field after the label, not the rest of the sentence.
		if _, after, found := strings.Cut(line, "Version "); found {
			fields := strings.Fields(after)
			if len(fields) == 0 {
				continue
			}
			return strings.TrimSuffix(fields[0], ".")
		}
	}
	return ""
}

// resolveCriuPluginDir locates the directory holding cuda_plugin.so, which has
// to be passed to CRIU as --libdir when it is not CRIU's compiled-in default.
func resolveCriuPluginDir(cfg GPUConfig) (string, error) {
	if cfg.CriuPluginDir != "" {
		plugin := filepath.Join(cfg.CriuPluginDir, cudaPluginFileName)
		if _, err := os.Stat(plugin); err != nil {
			return "", fmt.Errorf("CRIU CUDA plugin not found at %s: %w", plugin, err)
		}
		return cfg.CriuPluginDir, nil
	}

	for _, dir := range defaultCriuPluginDirs {
		if _, err := os.Stat(filepath.Join(dir, cudaPluginFileName)); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("CRIU CUDA plugin %s was not found in any of %s and --criu-plugin-dir is not set",
		cudaPluginFileName, strings.Join(defaultCriuPluginDirs, ", "))
}

// majorVersion pulls the leading integer out of a version like "610.43.02".
func majorVersion(version string) (int, error) {
	major, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, fmt.Errorf("unrecognised version %q", version)
	}
	return n, nil
}

/*
processGPUDevices lists the numbered NVIDIA devices a process has open.

This is what distinguishes a CUDA process from an ordinary one without asking
the caller to declare it, and it is how the single-GPU contract is enforced:
the count comes from the process itself, not from how many GPUs the node has.
*/
func processGPUDevices(pid int) ([]int, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect open files of pid %d: %w", pid, err)
	}

	seen := map[int]bool{}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil {
			continue // fd closed underneath us; not an error worth failing on
		}
		if match := nvidiaDeviceRE.FindStringSubmatch(target); match != nil {
			minor, err := strconv.Atoi(match[1])
			if err == nil {
				seen[minor] = true
			}
		}
	}

	devices := make([]int, 0, len(seen))
	for minor := range seen {
		devices = append(devices, minor)
	}
	sort.Ints(devices)
	return devices, nil
}

// ProcessUsesGPU reports whether a process has any NVIDIA device open. A
// failure to tell is reported as false with the error, never guessed at.
func ProcessUsesGPU(pid int) (bool, error) {
	devices, err := processGPUDevices(pid)
	if err != nil {
		return false, err
	}
	return len(devices) > 0, nil
}

/*
gpuPreflight runs every check GPU checkpointing depends on, in the order a
failure is most cheaply explained, and returns the provenance to record.

pid > 0 additionally enforces the single-GPU contract against the process
itself; pass 0 to check only the host (the restore side).
*/
func gpuPreflight(ctx context.Context, cfg GPUConfig, pid int) (*GPUDetails, error) {
	// 1 + 2: an NVIDIA GPU is visible and nvidia-smi can talk to the driver.
	devices, err := queryGPUs(ctx)
	if err != nil {
		return nil, fmt.Errorf("no usable NVIDIA GPU: %w", err)
	}

	// 3: NVIDIA's cuda-checkpoint utility is installed and executable.
	cudaCheckpoint, err := resolveCudaCheckpoint(cfg)
	if err != nil {
		return nil, err
	}

	// 4: the driver is new enough, and its cuda-checkpoint matches it.
	driver := devices[0].DriverVersion
	driverMajor, err := majorVersion(driver)
	if err != nil {
		return nil, fmt.Errorf("could not read the NVIDIA driver version: %w", err)
	}
	if driverMajor < minimumCudaCheckpointDriver {
		return nil, fmt.Errorf("NVIDIA driver %s is too old for CUDA checkpointing; %d or newer is required",
			driver, minimumCudaCheckpointDriver)
	}
	utilVersion := cudaCheckpointVersion(ctx, cudaCheckpoint)
	if utilVersion != "" {
		utilMajor, err := majorVersion(utilVersion)
		if err == nil && utilMajor != driverMajor {
			return nil, fmt.Errorf("cuda-checkpoint %s does not match NVIDIA driver %s; NVIDIA ships them in lockstep and a mismatched pair cannot checkpoint CUDA state",
				utilVersion, driver)
		}
	}

	// 5: CRIU's CUDA plugin is present somewhere we can point CRIU at.
	pluginDir, err := resolveCriuPluginDir(cfg)
	if err != nil {
		return nil, err
	}

	// 6 (contract): exactly one GPU, and one process, is in scope.
	if pid > 0 {
		inUse, err := processGPUDevices(pid)
		if err != nil {
			return nil, fmt.Errorf("could not determine which GPUs process %d is using: %w", pid, err)
		}
		if len(inUse) > 1 {
			return nil, fmt.Errorf("process %d is using %d GPUs (devices %v); GPU checkpointing currently supports a single GPU per process",
				pid, len(inUse), inUse)
		}
	}

	return &GPUDetails{
		Count:                 1,
		Name:                  devices[0].Name,
		MemoryTotalMiB:        devices[0].MemoryTotalMiB,
		DriverVersion:         driver,
		CudaCheckpointVersion: utilVersion,
		CriuPluginDir:         pluginDir,
	}, nil
}

/*
gpuCriuEnv is the environment a CRIU child needs to drive the CUDA plugin.

CRIU's plugin execs cuda-checkpoint by name, so its directory has to be on the
child's PATH — this is the most common reason a correctly installed setup still
fails to checkpoint CUDA state.
*/
func gpuCriuEnv(parent []string, cudaCheckpointPath string) []string {
	dir := filepath.Dir(cudaCheckpointPath)
	env := make([]string, 0, len(parent)+1)
	replaced := false
	for _, kv := range parent {
		if strings.HasPrefix(kv, "PATH=") && !replaced {
			env = append(env, "PATH="+dir+string(os.PathListSeparator)+strings.TrimPrefix(kv, "PATH="))
			replaced = true
			continue
		}
		env = append(env, kv)
	}
	if !replaced {
		env = append(env, "PATH="+dir)
	}
	return env
}

// checkGpuPrerequisites is the shallow "is there a GPU here at all" probe used
// where a full preflight would be too strong, such as restore-side validation
// of a checkpoint that merely ran on a GPU host.
func checkGpuPrerequisites(ctx context.Context) error {
	if _, err := queryGPUs(ctx); err != nil {
		return err
	}
	return nil
}

// gpuInfo returns nvidia-smi's per-GPU listing for manifest provenance.
// Best-effort: any failure just yields no GPU metadata.
func gpuInfo(ctx context.Context) []string {
	path, err := nvidiaSmi()
	if err != nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, path, "-L").Output()
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
