package checkpoint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestBuildDumpArgs(t *testing.T) {
	args := buildDumpArgs(dumpOptions{PID: 1234, ImagesDir: "/ckpt/c1/images", WorkDir: "/ckpt/c1", LogFile: "dump.log", Extra: []string{"--ext-mount-map", "auto"}})

	if args[0] != "dump" {
		t.Fatalf("expected first arg to be \"dump\", got %q", args[0])
	}
	if strings.Contains(strings.Join(args, " "), "--leave-running") {
		t.Fatalf("a stopping checkpoint must not pass --leave-running, got %v", args)
	}
	for _, forbidden := range []string{"sh", "-c"} {
		for _, a := range args {
			if a == forbidden {
				t.Fatalf("dump args should never contain a shell invocation, found %q in %v", forbidden, args)
			}
		}
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-t 1234", "--images-dir /ckpt/c1/images", "--work-dir /ckpt/c1", "--log-file dump.log", "--ext-mount-map auto"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected dump args to contain %q, got %v", want, args)
		}
	}
}

func TestBuildDumpArgsLeaveRunning(t *testing.T) {
	args := buildDumpArgs(dumpOptions{PID: 1234, ImagesDir: "/ckpt/c1/images", WorkDir: "/ckpt/c1", LogFile: "dump.log", LeaveRunning: true})
	if !strings.Contains(strings.Join(args, " "), "--leave-running") {
		t.Fatalf("expected --leave-running in dump args, got %v", args)
	}
}

func TestBuildRestoreArgs(t *testing.T) {
	args := buildRestoreArgs(restoreOptions{ImagesDir: "/ckpt/c1/images", WorkDir: "/ckpt/c1", LogFile: "restore.log", PidFile: "restore.pid", Extra: []string{"--ext-mount-map", "auto"}})

	if args[0] != "restore" {
		t.Fatalf("expected first arg to be \"restore\", got %q", args[0])
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--restore-detached", "--images-dir /ckpt/c1/images", "--work-dir /ckpt/c1", "--log-file restore.log", "--pidfile restore.pid", "--ext-mount-map auto"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected restore args to contain %q, got %v", want, args)
		}
	}
}

func TestReadPidFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/restore.pid"
	if err := os.WriteFile(path, []byte(strconv.Itoa(4242)+"\n"), 0644); err != nil {
		t.Fatalf("failed to write test pidfile: %v", err)
	}

	pid, err := readPidFile(path)
	if err != nil {
		t.Fatalf("readPidFile failed: %v", err)
	}
	if pid != 4242 {
		t.Fatalf("expected pid 4242, got %d", pid)
	}
}

func TestCheckBinary(t *testing.T) {
	if err := (&criuCheckpointer{}).checkBinary(); err == nil {
		t.Fatalf("expected an empty CriuPath to fail")
	}
	if err := (&criuCheckpointer{CriuPath: "/definitely/not/a/real/path/criu"}).checkBinary(); err == nil {
		t.Fatalf("expected a nonexistent CriuPath to fail")
	}

	dir := t.TempDir()
	if err := (&criuCheckpointer{CriuPath: dir}).checkBinary(); err == nil {
		t.Fatalf("expected a directory CriuPath to fail")
	}

	notExec := dir + "/criu"
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if err := (&criuCheckpointer{CriuPath: notExec}).checkBinary(); err == nil {
		t.Fatalf("expected a non-executable CriuPath to fail")
	}

	if err := os.Chmod(notExec, 0755); err != nil {
		t.Fatalf("failed to chmod test file executable: %v", err)
	}
	if err := (&criuCheckpointer{CriuPath: notExec}).checkBinary(); err != nil {
		t.Fatalf("expected an executable CriuPath to pass: %v", err)
	}
}

// requireCriu skips a test that needs a working CRIU on this host, the same
// way the venv tests skip when python3 is unavailable. It returns the path to
// the binary. Checkpoint/restore cannot be exercised meaningfully without it:
// CRIU is the thing under test.
func requireCriu(t *testing.T) string {
	t.Helper()
	criuPath, err := exec.LookPath("criu")
	if err != nil {
		t.Skip("criu is not installed on this host")
	}
	cp := &criuCheckpointer{CriuPath: criuPath, CheckpointRoot: t.TempDir()}
	if err := cp.CRIUCheck(context.Background()); err != nil {
		t.Skipf("criu is installed but not usable on this host: %v", err)
	}
	return criuPath
}

/*
startDetachedProcess starts a process that is neither a child of the test
binary nor connected to its stdio, and returns its pid. This is what CRIU can
actually dump: a process still wired to the test's pipes would leave CRIU
trying to dump file descriptors whose other end is not part of the dump.
*/
func startDetachedProcess(t *testing.T, seconds string) int {
	t.Helper()
	out, err := exec.Command("sh", "-c", "sleep "+seconds+" >/dev/null 2>&1 </dev/null & echo $!").Output()
	if err != nil {
		t.Fatalf("failed to start detached process: %v", err)
	}

	var pid int
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &pid); err != nil || pid <= 0 {
		t.Fatalf("failed to parse detached pid from %q: %v", out, err)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func TestCRIUCheckRequiresCheckpointRoot(t *testing.T) {
	cp := &criuCheckpointer{CriuPath: requireCriu(t)}
	err := cp.CRIUCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CheckpointRoot is not configured") {
		t.Fatalf("expected a CheckpointRoot-not-configured error, got %v", err)
	}
}

func TestRealCriuAvailability(t *testing.T) {
	cp := &criuCheckpointer{CriuPath: requireCriu(t), CheckpointRoot: t.TempDir()}
	if err := cp.CRIUCheck(context.Background()); err != nil {
		t.Fatalf("CRIUCheck failed against a real criu binary: %v", err)
	}
}

func TestRestoreRefusesIncompleteCheckpoint(t *testing.T) {
	criuPath := requireCriu(t)

	root := t.TempDir()
	workloadID, checkpointID := "wl-test", "ckpt-test"
	if err := os.MkdirAll(imagesDirPath(checkpointDirPath(root, workloadID, checkpointID)), 0755); err != nil {
		t.Fatalf("failed to create checkpoint dir: %v", err)
	}
	// Deliberately no manifest.json / COMPLETE marker.

	cp := &criuCheckpointer{CriuPath: criuPath, CheckpointRoot: root}
	if _, err := cp.restore(context.Background(), workloadID, checkpointID); err == nil {
		t.Fatalf("expected restore to refuse a checkpoint directory without a completion marker")
	}
}

// A real dump followed by a real restore, at the lowest level: the restored
// root task's pid must come back from CRIU's --pidfile and be alive.
func TestCheckpointAndRestoreCapturesRestoredPid(t *testing.T) {
	cp := &criuCheckpointer{CriuPath: requireCriu(t), CheckpointRoot: t.TempDir()}
	pid := startDetachedProcess(t, "600")

	opts := CreateOptions{WorkloadID: "wl-criu", Trigger: TriggerWalltime}
	if err := opts.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults failed: %v", err)
	}
	result, err := cp.checkpoint(context.Background(), "", pid, opts)
	if err != nil {
		t.Fatalf("checkpoint failed: %v", err)
	}
	if err := checkPidExists(pid); err == nil {
		t.Fatalf("expected CRIU to have stopped pid %d as part of the dump", pid)
	}

	restored, err := cp.restore(context.Background(), "wl-criu", result.CheckpointID)
	if err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	defer syscall.Kill(restored.Pid, syscall.SIGKILL)

	if restored.Pid <= 0 {
		t.Fatalf("expected a restored pid from --pidfile, got %d", restored.Pid)
	}
	if err := checkPidExists(restored.Pid); err != nil {
		t.Fatalf("restored pid %d is not running: %v", restored.Pid, err)
	}
}
