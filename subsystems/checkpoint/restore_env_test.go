package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareRestoreEnvironmentCreatesWorkspace(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	scratch := filepath.Join(root, "scratch", "run-1")

	v := prepareRestoreEnvironment(context.Background(), &Manifest{WorkingDir: workDir}, RestoreOptions{
		EnsureDirs: []string{scratch},
	})
	if !v.OK() {
		t.Fatalf("expected environment preparation to succeed: %v", v.Errors)
	}

	for _, dir := range []string{workDir, scratch} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("expected %s to have been created: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("expected %s to be a directory", dir)
		}
	}
}

func TestPrepareRestoreEnvironmentRunsPreRestoreCommands(t *testing.T) {
	staged := filepath.Join(t.TempDir(), "credential")

	v := prepareRestoreEnvironment(context.Background(), nil, RestoreOptions{
		PreRestoreCommands: []string{"touch " + staged},
		RequireFiles:       []string{staged},
	})
	if !v.OK() {
		t.Fatalf("expected the pre-restore command to satisfy the required file: %v", v.Errors)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("expected the pre-restore command to have run: %v", err)
	}
}

func TestPrepareRestoreEnvironmentStopsAtFailingCommand(t *testing.T) {
	unreached := filepath.Join(t.TempDir(), "unreached")

	v := prepareRestoreEnvironment(context.Background(), nil, RestoreOptions{
		PreRestoreCommands: []string{"exit 3", "touch " + unreached},
	})
	if v.OK() {
		t.Fatalf("expected a failing pre-restore command to abort preparation")
	}
	if !strings.Contains(v.Errors[0], "pre-restore command") {
		t.Fatalf("expected the error to name the failing command, got %v", v.Errors)
	}
	if _, err := os.Stat(unreached); err == nil {
		t.Fatalf("commands after a failure must not run")
	}
}

func TestPrepareRestoreEnvironmentRequiresCredentialFile(t *testing.T) {
	v := prepareRestoreEnvironment(context.Background(), nil, RestoreOptions{
		RequireFiles: []string{filepath.Join(t.TempDir(), "token")},
	})
	if v.OK() || !strings.Contains(v.Errors[0], "required file") {
		t.Fatalf("expected a missing credential to block the restore, got %v", v.Errors)
	}
}

// Pre-restore commands reconstruct module and toolchain state, so they need
// the environment the process was checkpointed with, not just linkspan's.
func TestPreRestoreCommandsInheritCheckpointEnvironment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "modulepath")

	m := &Manifest{Environment: map[string]string{"MODULEPATH": "/opt/modulefiles"}}
	v := prepareRestoreEnvironment(context.Background(), m, RestoreOptions{
		PreRestoreCommands: []string{"printf %s \"$MODULEPATH\" > " + out},
	})
	if !v.OK() {
		t.Fatalf("expected environment preparation to succeed: %v", v.Errors)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("failed to read command output: %v", err)
	}
	if string(data) != "/opt/modulefiles" {
		t.Fatalf("expected the checkpointed MODULEPATH to reach the command, got %q", data)
	}
}

func TestVerifyMountsAcceptsMountsPresentOnThisHost(t *testing.T) {
	mounts := readMountInfo("self")
	if len(mounts) == 0 {
		t.Skip("no real mounts reported on this host")
	}

	v := &RestoreValidation{}
	verifyMounts(&Manifest{Mounts: mounts}, v)
	if !v.OK() {
		t.Fatalf("expected this host's own mounts to satisfy the check: %v", v.Errors)
	}
}

// The workspace mount is the usual reason a restore onto a new allocation
// faults, so a missing one has to fail before CRIU runs rather than after.
func TestVerifyMountsDetectsMissingSharedStorage(t *testing.T) {
	v := &RestoreValidation{}
	verifyMounts(&Manifest{Mounts: []MountPoint{
		{Source: "nfs-server:/projects", Target: "/not-mounted-here", FSType: "nfs"},
	}}, v)

	if v.OK() || !strings.Contains(v.Errors[0], "is not mounted on this host") {
		t.Fatalf("expected a missing mount to block the restore, got %v", v.Errors)
	}
}
