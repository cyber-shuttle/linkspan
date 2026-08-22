package checkpoint

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

// captureLogs redirects the standard logger to a buffer for the duration of
// the test, so the CheckpointProcessAfterDelay goroutine's branch can be
// observed without a real CRIU binary.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

func startTestProcess(t *testing.T, args ...string) string {
	t.Helper()
	id, err := pm.GlobalProcessManager.Start(exec.Command(args[0], args[1:]...), false)
	if err != nil {
		t.Fatalf("failed to start test process: %v", err)
	}
	return id
}

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

func TestCheckpointProcessAfterDelay_TimerFires(t *testing.T) {
	buf := captureLogs(t)

	id := startTestProcess(t, "sleep", "5")
	defer pm.GlobalProcessManager.Kill(id)

	cp := &CriuCheckpointer{CriuPath: "/nonexistent/criu", CheckpointRoot: t.TempDir(), WorkloadID: "wl-test"}
	if err := cp.CheckpointProcessAfterDelay(context.Background(), id, 150*time.Millisecond, TriggerManual); err != nil {
		t.Fatalf("CheckpointProcessAfterDelay returned error: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	logs := buf.String()
	if !strings.Contains(logs, "delay elapsed, checkpointing process "+id) {
		t.Fatalf("expected timer-fired log line, got logs:\n%s", logs)
	}
	// CriuPath is bogus, so the checkpoint attempt itself must fail preflight
	// — but it must have been *attempted*.
	if !strings.Contains(logs, "delayed checkpoint of process "+id+" failed") {
		t.Fatalf("expected a failed checkpoint attempt to be logged, got logs:\n%s", logs)
	}
}

func TestCheckpointProcessAfterDelay_ProcessCompletesFirst(t *testing.T) {
	buf := captureLogs(t)

	id := startTestProcess(t, "sleep", "0.1")

	cp := &CriuCheckpointer{CriuPath: "/nonexistent/criu", CheckpointRoot: t.TempDir(), WorkloadID: "wl-test"}
	if err := cp.CheckpointProcessAfterDelay(context.Background(), id, 3*time.Second, TriggerManual); err != nil {
		t.Fatalf("CheckpointProcessAfterDelay returned error: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	logs := buf.String()
	if !strings.Contains(logs, "completed before the checkpoint delay elapsed; skipping checkpoint") {
		t.Fatalf("expected early-completion log line, got logs:\n%s", logs)
	}
	if strings.Contains(logs, "delay elapsed, checkpointing process") {
		t.Fatalf("checkpoint should not have been attempted once the process exited early, got logs:\n%s", logs)
	}
}

func TestCheckpointProcessAfterDelay_ContextCanceled(t *testing.T) {
	buf := captureLogs(t)

	id := startTestProcess(t, "sleep", "5")
	defer pm.GlobalProcessManager.Kill(id)

	ctx, cancel := context.WithCancel(context.Background())

	cp := &CriuCheckpointer{CriuPath: "/nonexistent/criu", CheckpointRoot: t.TempDir(), WorkloadID: "wl-test"}
	if err := cp.CheckpointProcessAfterDelay(ctx, id, 3*time.Second, TriggerManual); err != nil {
		t.Fatalf("CheckpointProcessAfterDelay returned error: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(300 * time.Millisecond)

	logs := buf.String()
	if !strings.Contains(logs, "linkspan is shutting down; canceling scheduled checkpoint") {
		t.Fatalf("expected shutdown-cancellation log line, got logs:\n%s", logs)
	}
	if strings.Contains(logs, "delay elapsed, checkpointing process") {
		t.Fatalf("checkpoint should not have been attempted after shutdown, got logs:\n%s", logs)
	}
}

func TestCheckpointProcessAfterDelay_InvalidDelay(t *testing.T) {
	cp := &CriuCheckpointer{CriuPath: "/nonexistent/criu", CheckpointRoot: t.TempDir()}
	if err := cp.CheckpointProcessAfterDelay(context.Background(), "whatever", 0, TriggerManual); err == nil {
		t.Fatalf("expected an error for a non-positive delay")
	}
}

func TestCheckpointProcessAfterDelay_UnknownProcess(t *testing.T) {
	cp := &CriuCheckpointer{CriuPath: "/nonexistent/criu", CheckpointRoot: t.TempDir()}
	if err := cp.CheckpointProcessAfterDelay(context.Background(), "not-a-real-id", time.Second, TriggerManual); err == nil {
		t.Fatalf("expected an error for an unknown process id")
	}
}

func TestCheckDirWritable(t *testing.T) {
	if err := checkDirWritable(t.TempDir()); err != nil {
		t.Fatalf("expected a fresh temp dir to be writable: %v", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits do not restrict writes")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatalf("failed to chmod test dir read-only: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0755) })

	if err := checkDirWritable(dir); err == nil {
		t.Fatalf("expected a read-only dir to fail the writable check")
	}
}

func TestCheckPidExists(t *testing.T) {
	if err := checkPidExists(os.Getpid()); err != nil {
		t.Fatalf("expected our own pid to exist: %v", err)
	}

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to run helper process: %v", err)
	}
	gonePid := cmd.Process.Pid

	if err := checkPidExists(gonePid); err == nil {
		t.Fatalf("expected an exited pid to be reported as not existing")
	}

	if err := checkPidExists(0); err == nil {
		t.Fatalf("expected pid 0 to be rejected as invalid")
	}
}

func TestCheckAllowedUser(t *testing.T) {
	self := os.Getpid()

	if err := checkAllowedUser(self, nil); err != nil {
		t.Fatalf("default (empty) allow list should permit checkpointing our own process: %v", err)
	}

	if err := checkAllowedUser(self, []string{"*"}); err != nil {
		t.Fatalf("wildcard allow list should permit any user: %v", err)
	}

	u, err := user.Current()
	if err != nil {
		t.Skipf("could not resolve current user: %v", err)
	}
	if err := checkAllowedUser(self, []string{u.Username}); err != nil {
		t.Fatalf("allow list containing our own username should permit our own process: %v", err)
	}
	if err := checkAllowedUser(self, []string{u.Uid}); err != nil {
		t.Fatalf("allow list containing our own uid should permit our own process: %v", err)
	}

	if err := checkAllowedUser(self, []string{"definitely-not-a-real-user", "999999"}); err == nil {
		t.Fatalf("allow list not containing our user should reject our own process")
	}
}

func TestProcessOwnerGID(t *testing.T) {
	if _, err := processOwnerGID(os.Getpid()); err != nil {
		t.Fatalf("expected to determine our own gid: %v", err)
	}
}

func TestCheckBinary(t *testing.T) {
	if err := (&CriuCheckpointer{}).checkBinary(); err == nil {
		t.Fatalf("expected an empty CriuPath to fail")
	}
	if err := (&CriuCheckpointer{CriuPath: "/definitely/not/a/real/path/criu"}).checkBinary(); err == nil {
		t.Fatalf("expected a nonexistent CriuPath to fail")
	}

	dir := t.TempDir()
	if err := (&CriuCheckpointer{CriuPath: dir}).checkBinary(); err == nil {
		t.Fatalf("expected a directory CriuPath to fail")
	}

	notExec := dir + "/criu"
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if err := (&CriuCheckpointer{CriuPath: notExec}).checkBinary(); err == nil {
		t.Fatalf("expected a non-executable CriuPath to fail")
	}

	if err := os.Chmod(notExec, 0755); err != nil {
		t.Fatalf("failed to chmod test file executable: %v", err)
	}
	if err := (&CriuCheckpointer{CriuPath: notExec}).checkBinary(); err != nil {
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
	cp := &CriuCheckpointer{CriuPath: writeStubCriu(t)}
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

	cp := &CriuCheckpointer{CriuPath: criuPath, CheckpointRoot: t.TempDir()}
	if err := cp.CRIUCheck(context.Background()); err != nil {
		t.Fatalf("CRIUCheck failed against a real criu binary: %v", err)
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

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		Schema:       ManifestSchema,
		CheckpointID: "ckpt-test",
		WorkloadID:   "wl-test",
		CreatedAt:    time.Now().UTC().Truncate(time.Second),
		Trigger:      TriggerManual,
		ProcessID:    "p-123",
		OriginalPID:  4242,
		Command:      "/bin/sleep",
		Args:         []string{"sleep", "100"},
		CRIUOptions:  []string{"dump", "-t", "4242"},
		State:        StateCreating,
	}

	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest failed: %v", err)
	}

	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest failed: %v", err)
	}
	if got.CheckpointID != m.CheckpointID || got.WorkloadID != m.WorkloadID || got.State != m.State {
		t.Fatalf("round-tripped manifest mismatch: got %+v, want %+v", got, m)
	}
	if !got.CreatedAt.Equal(m.CreatedAt) {
		t.Fatalf("CreatedAt mismatch: got %v, want %v", got.CreatedAt, m.CreatedAt)
	}
	if len(got.Args) != len(m.Args) || got.Args[0] != m.Args[0] {
		t.Fatalf("Args mismatch: got %v, want %v", got.Args, m.Args)
	}
}

func TestIsCheckpointComplete(t *testing.T) {
	dir := t.TempDir()

	if isCheckpointComplete(dir) {
		t.Fatalf("expected incomplete: no manifest, no marker")
	}

	m := &Manifest{State: StateCreating}
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest failed: %v", err)
	}
	if isCheckpointComplete(dir) {
		t.Fatalf("expected incomplete: manifest present with state=creating, no COMPLETE marker")
	}

	if err := os.WriteFile(filepath.Join(dir, completeFileName), []byte{}, 0644); err != nil {
		t.Fatalf("failed to write COMPLETE marker: %v", err)
	}
	if isCheckpointComplete(dir) {
		t.Fatalf("expected incomplete: COMPLETE marker present but manifest state is still creating")
	}

	m.State = StateComplete
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest failed: %v", err)
	}
	if !isCheckpointComplete(dir) {
		t.Fatalf("expected complete: COMPLETE marker present and manifest state is complete")
	}
}

func TestRestoreRefusesIncompleteCheckpoint(t *testing.T) {
	criuPath, err := exec.LookPath("criu")
	if err != nil {
		t.Skip("criu is not installed on this host; skipping end-to-end Restore() gating test")
	}

	root := t.TempDir()
	workloadID, checkpointID := "wl-test", "ckpt-test"
	checkpointDir := filepath.Join(root, workloadID, checkpointID)
	if err := os.MkdirAll(filepath.Join(checkpointDir, "images"), 0755); err != nil {
		t.Fatalf("failed to create checkpoint dir: %v", err)
	}
	// Deliberately no manifest.json / COMPLETE marker.

	cp := &CriuCheckpointer{CriuPath: criuPath, CheckpointRoot: root}
	if _, err := cp.Restore(context.Background(), workloadID, checkpointID); err == nil {
		t.Fatalf("expected Restore to refuse a checkpoint directory without a completion marker")
	}
}

func TestNewIDsAreUniqueAndPrefixed(t *testing.T) {
	w1, w2 := NewWorkloadID(), NewWorkloadID()
	if w1 == w2 {
		t.Fatalf("expected distinct workload ids, got %q twice", w1)
	}
	if !strings.HasPrefix(w1, "wl-") || !strings.HasPrefix(w2, "wl-") {
		t.Fatalf("expected workload ids to start with \"wl-\", got %q and %q", w1, w2)
	}

	c1, c2 := NewCheckpointID(), NewCheckpointID()
	if c1 == c2 {
		t.Fatalf("expected distinct checkpoint ids, got %q twice", c1)
	}
	if !strings.HasPrefix(c1, "ckpt-") || !strings.HasPrefix(c2, "ckpt-") {
		t.Fatalf("expected checkpoint ids to start with \"ckpt-\", got %q and %q", c1, c2)
	}
}
