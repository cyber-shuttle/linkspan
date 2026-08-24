package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// startProbeProcess starts a real process with the given environment and
// waits for it to exec, so /proc/<pid> describes that program rather than the
// still-forked test binary.
func startProbeProcess(t *testing.T, env []string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start test process: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })

	time.Sleep(200 * time.Millisecond)
	return cmd
}

func TestOpenFilesReportsRegularFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dataset.h5")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	defer f.Close()

	found := false
	for _, open := range openFiles(os.Getpid()) {
		info, err := os.Stat(open)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("openFiles reported %q, which is not a regular file", open)
		}
		if open == path {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the open file %s to be recorded, got %v", path, openFiles(os.Getpid()))
	}
}

// The manifest goes to shared storage, so the environment capture must pick
// up toolchain state and leave credentials behind.
func TestCapturedEnvironmentExcludesSecrets(t *testing.T) {
	cmd := startProbeProcess(t, []string{
		"LD_LIBRARY_PATH=/opt/gcc/lib64",
		"LOADEDMODULES=gcc/12.2.0:openmpi/4.1.5",
		"CUDA_VISIBLE_DEVICES=0,1",
		"AWS_SECRET_ACCESS_KEY=super-secret",
		"GITHUB_TOKEN=ghp_secret",
	})

	env := capturedEnvironment(cmd.Process.Pid)
	if env["LD_LIBRARY_PATH"] != "/opt/gcc/lib64" {
		t.Fatalf("expected LD_LIBRARY_PATH to be captured, got %q", env["LD_LIBRARY_PATH"])
	}
	if env["CUDA_VISIBLE_DEVICES"] != "0,1" {
		t.Fatalf("expected CUDA_VISIBLE_DEVICES to be captured, got %q", env["CUDA_VISIBLE_DEVICES"])
	}
	for _, secret := range []string{"AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN"} {
		if _, leaked := env[secret]; leaked {
			t.Fatalf("%s must never be recorded in a manifest, got %v", secret, env)
		}
	}

	modules := loadedModules(env)
	if len(modules) != 2 || modules[0] != "gcc/12.2.0" || modules[1] != "openmpi/4.1.5" {
		t.Fatalf("expected the loaded modules to be parsed from LOADEDMODULES, got %v", modules)
	}
}

func TestReadMountInfoSkipsVirtualFilesystems(t *testing.T) {
	mounts := readMountInfo("self")
	if len(mounts) == 0 {
		t.Fatalf("expected at least the root filesystem to be reported")
	}

	rootFound := false
	for _, m := range mounts {
		if virtualFilesystems[m.FSType] {
			t.Fatalf("virtual filesystem %s at %s must not be a restore prerequisite", m.FSType, m.Target)
		}
		if m.Target == "" {
			t.Fatalf("parsed a mount with no target: %+v", m)
		}
		if m.Target == "/" {
			rootFound = true
		}
	}
	if !rootFound {
		t.Fatalf("expected / among the real mounts, got %v", mounts)
	}
}

// The manifest should record the storage actually backing the process, not
// every mount that happened to exist on the checkpointing node.
func TestDependencyMountsResolveToBackingFilesystem(t *testing.T) {
	cmd := startProbeProcess(t, os.Environ())

	dir := t.TempDir()
	mounts := dependencyMounts(cmd.Process.Pid, []string{dir})
	if len(mounts) != 1 {
		t.Fatalf("expected exactly the one filesystem backing %s, got %v", dir, mounts)
	}
	if !pathUnder(dir, mounts[0].Target) {
		t.Fatalf("%s is not under the reported mount %s", dir, mounts[0].Target)
	}

	all := readMountInfo("self")
	for _, m := range all {
		if pathUnder(dir, m.Target) && len(m.Target) > len(mounts[0].Target) {
			t.Fatalf("expected the most specific mount for %s, got %s but %s is closer", dir, mounts[0].Target, m.Target)
		}
	}
}

func TestGatherManifestRecordsRestorePrerequisites(t *testing.T) {
	cmd := startProbeProcess(t, os.Environ())

	m := gatherManifest(t.Context(), manifestParams{
		WorkloadID:   "wl-manifest",
		PID:          cmd.Process.Pid,
		CheckpointID: "ckpt-manifest",
		Trigger:      TriggerManual,
	})

	if m.Executable == "" {
		t.Fatalf("expected the executable path to be recorded")
	}
	if _, err := os.Stat(m.Executable); err != nil {
		t.Fatalf("recorded executable %s does not exist: %v", m.Executable, err)
	}
	if m.WorkingDir == "" {
		t.Fatalf("expected the working directory to be recorded")
	}
	if m.UID != os.Getuid() {
		t.Fatalf("expected uid %d, got %d", os.Getuid(), m.UID)
	}
	if len(m.Mounts) == 0 {
		t.Fatalf("expected the filesystem backing the executable to be recorded")
	}
}
