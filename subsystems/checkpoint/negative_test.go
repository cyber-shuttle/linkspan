package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/*
The negative half of the stage 10 validation matrix.

Most of these cases already have homes elsewhere (missing CRIU in criu_test.go,
duplicate requests in service_test.go, and so on). What lives here is the two
that did not: GPU incompatibility on restore, and an unwritable checkpoint root
that stays unwritable even for root — the usual read-only-directory trick is a
no-op when the tests run privileged, which is exactly how they run on a compute
node.
*/

// gpuManifest builds a GPU-mode manifest whose recorded hardware can be bent
// away from this host's, one dimension at a time.
func gpuManifest(mutate func(*GPUDetails)) *Manifest {
	gpu := &GPUDetails{
		Name:           "NVIDIA A100-SXM4-40GB",
		MemoryTotalMiB: 40960,
		DriverVersion:  "610.43.02",
		Count:          1,
	}
	mutate(gpu)
	return &Manifest{Schema: ManifestSchema, GPUMode: true, GPU: gpu, State: StateComplete}
}

func validateGPUOf(t *testing.T, m *Manifest) *RestoreValidation {
	t.Helper()
	v := &RestoreValidation{}
	(&criuCheckpointer{}).validateGPUDetails(context.Background(), m, v)
	return v
}

// A checkpoint's own hardware must match this host's, or CUDA state does not
// fail cleanly on restore — it corrupts.
func TestIncompatibleGPUIsRejected(t *testing.T) {
	here := requireGPU(t)

	// Control: the host's own hardware validates.
	same := gpuManifest(func(g *GPUDetails) {
		g.Name = here.Name
		g.MemoryTotalMiB = here.MemoryTotalMiB
		g.DriverVersion = here.DriverVersion
	})
	if v := validateGPUOf(t, same); !v.OK() {
		t.Fatalf("this host's own GPU must validate against itself, got %v", v.Errors)
	}

	// A different GPU model class.
	wrongModel := gpuManifest(func(g *GPUDetails) {
		g.Name = "NVIDIA GeForce RTX 4090"
		g.MemoryTotalMiB = here.MemoryTotalMiB
		g.DriverVersion = here.DriverVersion
	})
	v := validateGPUOf(t, wrongModel)
	if v.OK() || !strings.Contains(strings.Join(v.Errors, " "), "different GPU model") {
		t.Fatalf("expected a different GPU model to be rejected, got %v", v.Errors)
	}

	// More GPU memory than this host has.
	tooBig := gpuManifest(func(g *GPUDetails) {
		g.Name = here.Name
		g.MemoryTotalMiB = here.MemoryTotalMiB * 4
		g.DriverVersion = here.DriverVersion
	})
	v = validateGPUOf(t, tooBig)
	if v.OK() || !strings.Contains(strings.Join(v.Errors, " "), "MiB") {
		t.Fatalf("expected insufficient GPU memory to be rejected, got %v", v.Errors)
	}

	// A newer driver at checkpoint time than this host runs.
	newerDriver := gpuManifest(func(g *GPUDetails) {
		g.Name = here.Name
		g.MemoryTotalMiB = here.MemoryTotalMiB
		g.DriverVersion = "9999.0.0"
	})
	v = validateGPUOf(t, newerDriver)
	if v.OK() || !strings.Contains(strings.Join(v.Errors, " "), "older") {
		t.Fatalf("expected an older host driver to be rejected, got %v", v.Errors)
	}
}

// A GPU checkpoint with no recorded details cannot be compatibility-checked,
// which is a warning rather than a hard failure.
func TestGPUCheckpointWithoutDetailsWarns(t *testing.T) {
	v := &RestoreValidation{}
	(&criuCheckpointer{}).validateGPUDetails(context.Background(),
		&Manifest{Schema: ManifestSchema, GPUMode: true, State: StateComplete}, v)

	if !v.OK() {
		t.Fatalf("a detail-less GPU checkpoint should warn, not error: %v", v.Errors)
	}
	if len(v.Warnings) == 0 {
		t.Fatalf("expected a warning about missing GPU details")
	}
}

/*
An unwritable checkpoint root has to be caught in preflight.

The root is placed under a regular file, so the MkdirAll fails on ENOTDIR for
root and non-root alike — a chmod 0500 directory would simply be ignored when
these tests run privileged on a compute node.
*/
func TestUnwritableCheckpointRootIsRejected(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "regular-file")
	if err := os.WriteFile(notADir, []byte("x"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	if err := checkDirWritable(filepath.Join(notADir, "checkpoints")); err == nil {
		t.Fatalf("expected a checkpoint root under a regular file to be rejected")
	}

	// And the CRIU preflight must surface it rather than proceeding.
	cp := &criuCheckpointer{CriuPath: requireCriu(t), CheckpointRoot: filepath.Join(notADir, "checkpoints")}
	err := cp.CRIUCheck(context.Background())
	if err == nil {
		t.Fatalf("expected CRIUCheck to fail on an unwritable checkpoint root")
	}
	if !strings.Contains(err.Error(), "checkpoint root") {
		t.Fatalf("expected the error to name the checkpoint root, got %v", err)
	}
}
