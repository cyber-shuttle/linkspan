package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestBuildDumpArgs(t *testing.T) {
	args := buildDumpArgs(1234, "/ckpt/c1/images", "/ckpt/c1", "dump.log", []string{"--ext-mount-map", "auto"})

	if args[0] != "dump" {
		t.Fatalf("expected first arg to be \"dump\", got %q", args[0])
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

func TestBuildRestoreArgs(t *testing.T) {
	args := buildRestoreArgs("/ckpt/c1/images", "/ckpt/c1", "restore.log", "restore.pid", []string{"--ext-mount-map", "auto"})

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

func writeStubCriu(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "criu")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("failed to write stub criu binary: %v", err)
	}
	return stub
}

func TestCRIUCheckRequiresCheckpointRoot(t *testing.T) {
	cp := &criuCheckpointer{CriuPath: writeStubCriu(t)}
	err := cp.CRIUCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CheckpointRoot is not configured") {
		t.Fatalf("expected a CheckpointRoot-not-configured error, got %v", err)
	}
}

func TestRealCriuAvailability(t *testing.T) {
	criuPath, err := exec.LookPath("criu")
	if err != nil {
		t.Skip("criu is not installed on this host; skipping real CRIUCheck test")
	}

	cp := &criuCheckpointer{CriuPath: criuPath, CheckpointRoot: t.TempDir()}
	if err := cp.CRIUCheck(context.Background()); err != nil {
		t.Fatalf("CRIUCheck failed against a real criu binary: %v", err)
	}
}

func TestRestoreRefusesIncompleteCheckpoint(t *testing.T) {
	criuPath, err := exec.LookPath("criu")
	if err != nil {
		t.Skip("criu is not installed on this host; skipping end-to-end restore gating test")
	}

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
