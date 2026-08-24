package checkpoint

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/controller"
	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

// fixedDeadline is a real DeadlineProvider backed by a known time — Slurm
// cannot be asked for a deadline 300ms from now.
func fixedDeadline(at time.Time) DeadlineProvider {
	return DeadlineFunc(func(context.Context) (time.Time, error) { return at, nil })
}

func failingDeadline(msg string) DeadlineProvider {
	return DeadlineFunc(func(context.Context) (time.Time, error) {
		return time.Time{}, errNoDeadline{msg}
	})
}

type errNoDeadline struct{ msg string }

func (e errNoDeadline) Error() string { return e.msg }

// drainShutdownChannel clears any shutdown left by an earlier test, so a
// later assertion reads this test's own trigger.
func drainShutdownChannel(t *testing.T) {
	t.Helper()
	for {
		select {
		case <-controller.ExternalShutdownChannel:
		default:
			return
		}
	}
}

func awaitShutdown(t *testing.T, within time.Duration) string {
	t.Helper()
	select {
	case reason := <-controller.ExternalShutdownChannel:
		return reason
	case <-time.After(within):
		t.Fatalf("expected a shutdown to be triggered within %s", within)
		return ""
	}
}

// waitFor polls until cond holds, so a test never races the guard's goroutine.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

func TestParseSignal(t *testing.T) {
	for name, want := range map[string]syscall.Signal{
		"SIGUSR1": syscall.SIGUSR1,
		"usr1":    syscall.SIGUSR1,
		"SIGUSR2": syscall.SIGUSR2,
		"sigterm": syscall.SIGTERM,
		" SIGINT": syscall.SIGINT,
		"HUP":     syscall.SIGHUP,
	} {
		got, err := ParseSignal(name)
		if err != nil {
			t.Fatalf("ParseSignal(%q) failed: %v", name, err)
		}
		if got != want {
			t.Fatalf("ParseSignal(%q) = %v, want %v", name, got, want)
		}
	}

	if _, err := ParseSignal("SIGBANANA"); err == nil {
		t.Fatalf("expected an unsupported signal name to be rejected")
	}
}

// The arithmetic the whole feature rests on.
func TestCheckpointAtSubtractsTheMargin(t *testing.T) {
	end := time.Date(2026, 8, 24, 18, 0, 0, 0, time.Local)
	g := NewWalltimeGuard(nil, TargetFromProcessID("p-1"), CreateOptions{WorkloadID: "wl"},
		fixedDeadline(end), WalltimeOptions{Margin: 5 * time.Minute})

	at, err := g.checkpointAt(context.Background())
	if err != nil {
		t.Fatalf("checkpointAt failed: %v", err)
	}
	if want := end.Add(-5 * time.Minute); !at.Equal(want) {
		t.Fatalf("expected checkpoint at %s, got %s", want, at)
	}
}

func TestCheckpointAtReportsAMissingDeadline(t *testing.T) {
	g := NewWalltimeGuard(nil, TargetFromProcessID("p-1"), CreateOptions{WorkloadID: "wl"},
		nil, WalltimeOptions{Margin: time.Minute})
	if _, err := g.checkpointAt(context.Background()); err == nil {
		t.Fatalf("expected an error with no deadline provider")
	}
}

func TestWalltimeGuardRejectsBadConfiguration(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")
	processID := startTestProcess(t, "sleep", "30")
	t.Cleanup(func() { _ = pm.GlobalProcessManager.Kill(processID) })

	deadline := fixedDeadline(time.Now().Add(time.Hour))

	noMargin := NewWalltimeGuard(svc, TargetFromProcessID(processID), CreateOptions{WorkloadID: "wl"}, deadline, WalltimeOptions{})
	if err := noMargin.Start(context.Background()); err == nil {
		t.Fatalf("expected a zero margin to be rejected")
	}

	pidTarget := NewWalltimeGuard(svc, TargetFromPID(1), CreateOptions{WorkloadID: "wl"}, deadline, WalltimeOptions{Margin: time.Minute})
	if err := pidTarget.Start(context.Background()); err == nil {
		t.Fatalf("expected a pid target to be rejected: the guard must race the application's completion")
	}

	unconfigured := NewWalltimeGuard(&CheckpointService{criu: &criuCheckpointer{}, workloads: map[string]*workloadEntry{}},
		TargetFromProcessID(processID), CreateOptions{WorkloadID: "wl"}, deadline, WalltimeOptions{Margin: time.Minute})
	if err := unconfigured.Start(context.Background()); err == nil {
		t.Fatalf("expected an unconfigured service to be rejected")
	}
}

/*
Requirement 5: an application that finishes before the deadline must not be
checkpointed. No CRIU needed — the point is that nothing is attempted.
*/
func TestWalltimeGuardSkipsCheckpointWhenApplicationFinishesFirst(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	processID := startTestProcess(t, "sleep", "0.2")
	g := NewWalltimeGuard(svc, TargetFromProcessID(processID), CreateOptions{WorkloadID: "wl-finished"},
		fixedDeadline(time.Now().Add(time.Hour)), WalltimeOptions{Margin: time.Minute})

	if err := g.Start(ctx); err != nil {
		t.Fatalf("failed to start the guard: %v", err)
	}

	waitFor(t, 5*time.Second, "the workload to be marked completed", func() bool {
		return svc.WorkloadState("wl-finished") == WorkloadCompleted
	})

	if g.CheckpointID() != "" {
		t.Fatalf("a completed application must not be checkpointed, got %s", g.CheckpointID())
	}
	manifests, err := svc.ListCheckpoints()
	if err != nil {
		t.Fatalf("failed to list checkpoints: %v", err)
	}
	if len(manifests) != 0 {
		t.Fatalf("expected no checkpoints to have been written, got %d", len(manifests))
	}
}

// A completed workload is terminal: a late trigger must be refused rather than
// dumping a process that is already gone.
func TestCompletedWorkloadIsNotCheckpointable(t *testing.T) {
	svc := newTestService(t, "/nonexistent/criu", "")
	svc.MarkWorkloadCompleted("wl-done")

	if got := svc.WorkloadState("wl-done"); got != WorkloadCompleted {
		t.Fatalf("expected state completed, got %q", got)
	}
	_, err := svc.CreateCheckpoint(context.Background(), TargetFromPID(os.Getpid()), CreateOptions{WorkloadID: "wl-done"})
	if err == nil {
		t.Fatalf("expected a completed workload to refuse a checkpoint")
	}
}

// SIGTERM must always end in shutdown, even when the checkpoint cannot be
// written — swallowing it would hang the allocation until SIGKILL.
func TestLastChanceSignalShutsDownEvenWhenCheckpointFails(t *testing.T) {
	drainShutdownChannel(t)

	// Configured enough to start, but the dump itself will fail.
	svc := newTestService(t, "/nonexistent/criu", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	processID := startTestProcess(t, "sleep", "30")
	t.Cleanup(func() { _ = pm.GlobalProcessManager.Kill(processID) })

	g := NewWalltimeGuard(svc, TargetFromProcessID(processID), CreateOptions{WorkloadID: "wl-sigterm"},
		failingDeadline("no scheduler here"), WalltimeOptions{Margin: time.Minute})
	if err := g.Start(ctx); err != nil {
		t.Fatalf("failed to start the guard: %v", err)
	}

	// Only signal once the handler is installed, or SIGTERM would kill the
	// test binary via its default disposition.
	<-g.Watching()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("failed to send SIGTERM: %v", err)
	}

	reason := awaitShutdown(t, 5*time.Second)
	if reason == "" {
		t.Fatalf("expected a shutdown reason naming the signal")
	}
	if g.CheckpointID() != "" {
		t.Fatalf("no checkpoint should have been written with a broken CRIU path")
	}
}

/*
Requirement 2 end to end: the computed watcher fires at deadline - margin and
checkpoints through CheckpointService, recorded with the walltime trigger.
*/
func TestWalltimeGuardCheckpointsBeforeTheDeadline(t *testing.T) {
	drainShutdownChannel(t)

	svc := newTestService(t, requireCriu(t), "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	processID := startTestProcess(t, "sleep", "600")
	t.Cleanup(func() { _ = pm.GlobalProcessManager.Kill(processID) })

	// Deadline is margin + 300ms away, so the guard fires almost immediately.
	margin := 2 * time.Second
	g := NewWalltimeGuard(svc, TargetFromProcessID(processID), CreateOptions{WorkloadID: "wl-walltime"},
		fixedDeadline(time.Now().Add(margin+300*time.Millisecond)),
		WalltimeOptions{Margin: margin, ShutdownAfterCheckpoint: true})

	if err := g.Start(ctx); err != nil {
		t.Fatalf("failed to start the guard: %v", err)
	}

	waitFor(t, 20*time.Second, "the walltime checkpoint to be written", func() bool {
		return g.CheckpointID() != ""
	})

	manifest, err := svc.GetCheckpoint(g.CheckpointID())
	if err != nil {
		t.Fatalf("failed to read the checkpoint the guard wrote: %v", err)
	}
	if manifest.Trigger != TriggerWalltime {
		t.Fatalf("expected trigger %q, got %q", TriggerWalltime, manifest.Trigger)
	}
	if manifest.State != StateComplete {
		t.Fatalf("expected a complete checkpoint, got %q", manifest.State)
	}
	// A walltime checkpoint stops the application: the allocation is ending.
	if manifest.LeaveRunning {
		t.Fatalf("a walltime checkpoint must not leave the process running")
	}

	// Milestone 6: once the checkpoint is durable, the allocation is released.
	reason := awaitShutdown(t, 5*time.Second)
	if reason == "" {
		t.Fatalf("expected shutdown to be triggered after a successful checkpoint")
	}
}

/*
Requirement 3: the scheduler's pre-walltime signal is an independent trigger,
and works on a job whose deadline could not be determined at all.
*/
func TestSchedulerSignalTriggersCheckpointWithoutADeadline(t *testing.T) {
	drainShutdownChannel(t)

	svc := newTestService(t, requireCriu(t), "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	processID := startTestProcess(t, "sleep", "600")
	t.Cleanup(func() { _ = pm.GlobalProcessManager.Kill(processID) })

	g := NewWalltimeGuard(svc, TargetFromProcessID(processID), CreateOptions{WorkloadID: "wl-signal"},
		failingDeadline("squeue unavailable"),
		WalltimeOptions{Margin: time.Minute, PreWalltimeSignals: []os.Signal{syscall.SIGUSR1}})

	if err := g.Start(ctx); err != nil {
		t.Fatalf("failed to start the guard: %v", err)
	}

	<-g.Watching()
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("failed to send SIGUSR1: %v", err)
	}

	waitFor(t, 20*time.Second, "the signal-triggered checkpoint", func() bool {
		return g.CheckpointID() != ""
	})

	manifest, err := svc.GetCheckpoint(g.CheckpointID())
	if err != nil {
		t.Fatalf("failed to read the checkpoint: %v", err)
	}
	if manifest.Trigger != TriggerSignal {
		t.Fatalf("expected trigger %q, got %q", TriggerSignal, manifest.Trigger)
	}
	if manifest.LeaveRunning {
		t.Fatalf("a signal checkpoint must not leave the process running")
	}
}

// Several triggers can fire in quick succession; exactly one checkpoint must
// come out of them.
func TestRepeatedTriggersProduceOneCheckpoint(t *testing.T) {
	drainShutdownChannel(t)

	svc := newTestService(t, requireCriu(t), "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	processID := startTestProcess(t, "sleep", "600")
	t.Cleanup(func() { _ = pm.GlobalProcessManager.Kill(processID) })

	g := NewWalltimeGuard(svc, TargetFromProcessID(processID), CreateOptions{WorkloadID: "wl-once"},
		failingDeadline("no deadline"),
		WalltimeOptions{Margin: time.Minute, PreWalltimeSignals: []os.Signal{syscall.SIGUSR1}})

	if err := g.Start(ctx); err != nil {
		t.Fatalf("failed to start the guard: %v", err)
	}

	<-g.Watching()
	for i := 0; i < 3; i++ {
		if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
			t.Fatalf("failed to send SIGUSR1: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	waitFor(t, 20*time.Second, "the first checkpoint", func() bool {
		return g.CheckpointID() != ""
	})
	time.Sleep(500 * time.Millisecond) // let any duplicate trigger land

	manifests, err := svc.ListCheckpoints()
	if err != nil {
		t.Fatalf("failed to list checkpoints: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected exactly one checkpoint from repeated triggers, got %d", len(manifests))
	}
}
