package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rewriteManifest edits a real checkpoint's manifest in place, so a test can
// break exactly one property of an otherwise genuine checkpoint.
func rewriteManifest(t *testing.T, checkpointDir string, mutate func(*Manifest)) {
	t.Helper()
	m, err := ReadManifest(checkpointDir)
	if err != nil {
		t.Fatalf("failed to read manifest: %v", err)
	}
	mutate(m)
	if err := writeManifest(checkpointDir, m); err != nil {
		t.Fatalf("failed to rewrite manifest: %v", err)
	}
}

func hasError(v *RestoreValidation, want string) bool {
	return strings.Contains(strings.Join(v.Errors, "\n"), want)
}

func TestValidateRestoreAcceptsFreshCheckpoint(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-valid")

	v, err := svc.ValidateRestore(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ValidateRestore failed: %v", err)
	}
	if !v.OK() {
		t.Fatalf("expected a checkpoint just taken on this host to validate, got %v", v.Errors)
	}
}

func TestValidateRestoreRejectsIncompleteCheckpoint(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-incomplete")
	dir := checkpointDirPath(svc.criu.CheckpointRoot, "wl-incomplete", checkpointID)

	if err := os.Remove(filepath.Join(dir, completeFileName)); err != nil {
		t.Fatalf("failed to remove completion marker: %v", err)
	}

	v, err := svc.ValidateRestore(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ValidateRestore failed: %v", err)
	}
	if !hasError(v, "missing its COMPLETE marker") {
		t.Fatalf("expected a partial checkpoint to be rejected, got %v", v.Errors)
	}
}

func TestValidateRestoreRejectsUnreadableImages(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-images")
	dir := checkpointDirPath(svc.criu.CheckpointRoot, "wl-images", checkpointID)

	if err := os.RemoveAll(imagesDirPath(dir)); err != nil {
		t.Fatalf("failed to remove images dir: %v", err)
	}

	v, err := svc.ValidateRestore(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ValidateRestore failed: %v", err)
	}
	if !hasError(v, "not accessible") {
		t.Fatalf("expected unreachable checkpoint storage to be rejected, got %v", v.Errors)
	}
}

func TestValidateRestoreRejectsForeignArchitecture(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-arch")
	dir := checkpointDirPath(svc.criu.CheckpointRoot, "wl-arch", checkpointID)

	rewriteManifest(t, dir, func(m *Manifest) { m.Arch = "someotherarch" })

	v, err := svc.ValidateRestore(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ValidateRestore failed: %v", err)
	}
	if !hasError(v, "not portable across architectures") {
		t.Fatalf("expected an architecture mismatch to be rejected, got %v", v.Errors)
	}
}

func TestValidateRestoreRejectsMissingExecutable(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-exe")
	dir := checkpointDirPath(svc.criu.CheckpointRoot, "wl-exe", checkpointID)

	gone := filepath.Join(t.TempDir(), "uninstalled-app")
	rewriteManifest(t, dir, func(m *Manifest) { m.Executable = gone })

	v, err := svc.ValidateRestore(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ValidateRestore failed: %v", err)
	}
	if !hasError(v, "is not present on this host") {
		t.Fatalf("expected a missing executable to be rejected, got %v", v.Errors)
	}
}

func TestValidateRestoreRejectsForeignUID(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, which may restore a process owned by any user")
	}

	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-uid")
	dir := checkpointDirPath(svc.criu.CheckpointRoot, "wl-uid", checkpointID)

	rewriteManifest(t, dir, func(m *Manifest) { m.UID = os.Getuid() + 1 })

	v, err := svc.ValidateRestore(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ValidateRestore failed: %v", err)
	}
	if !hasError(v, "restore as the original user") {
		t.Fatalf("expected a uid mismatch to be rejected, got %v", v.Errors)
	}
}

// A file that has since disappeared is a warning, not a blocker: CRIU
// restores its contents from the images.
func TestValidateRestoreWarnsOnMissingOpenFile(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	checkpointID := checkpointRealProcess(t, svc, "wl-openfile")
	dir := checkpointDirPath(svc.criu.CheckpointRoot, "wl-openfile", checkpointID)

	scratch := filepath.Join(t.TempDir(), "scratch.tmp")
	rewriteManifest(t, dir, func(m *Manifest) { m.OpenFiles = append(m.OpenFiles, scratch) })

	v, err := svc.ValidateRestore(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ValidateRestore failed: %v", err)
	}
	if !v.OK() {
		t.Fatalf("expected a missing open file not to block the restore, got %v", v.Errors)
	}
	if !strings.Contains(strings.Join(v.Warnings, "\n"), scratch) {
		t.Fatalf("expected a warning naming %s, got %v", scratch, v.Warnings)
	}
}
