package checkpoint

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// checkGpuPrerequisites is a shallow sanity check for GPU checkpoint mode:
// it confirms an NVIDIA GPU and its tooling are visible. Deep, plugin-specific
// CRIU/CUDA validation has no existing precedent in this repo and is left as
// a future extension of this function.
func checkGpuPrerequisites(ctx context.Context) error {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return fmt.Errorf("GPU checkpoint requested but nvidia-smi was not found on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, "-L")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("GPU checkpoint requested but nvidia-smi failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// gpuInfo returns nvidia-smi's per-GPU listing for manifest provenance.
// Best-effort: any failure just yields no GPU metadata.
func gpuInfo(ctx context.Context) []string {
	path, err := exec.LookPath("nvidia-smi")
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
