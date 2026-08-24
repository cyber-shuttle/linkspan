package checkpoint

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/controller"
	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

// DefaultPreWalltimeSignal is what Slurm's --signal=<sig>@<seconds> should be
// pointed at. SIGUSR1 is conventional and, unlike SIGTERM, unambiguous: it can
// only mean "the allocation is ending", never "an operator stopped us".
var DefaultPreWalltimeSignal = syscall.SIGUSR1

// WalltimeOptions configures a WalltimeGuard.
type WalltimeOptions struct {
	Margin                  time.Duration // checkpoint this long before the allocation ends
	PreWalltimeSignals      []os.Signal   // scheduler's early warning; defaults to SIGUSR1
	LastChanceSignals       []os.Signal   // final warning; defaults to SIGTERM when CheckpointOnSigterm
	CheckpointOnSigterm     bool          // treat SIGTERM as a last-chance trigger rather than a plain stop
	ShutdownAfterCheckpoint bool          // release the allocation once the checkpoint is durable
}

/*
WalltimeGuard checkpoints a workload before its Slurm allocation expires.

It watches three sources, because no single one is trustworthy on its own: the
computed deadline can be wrong if the scheduler shortens the job, the
pre-walltime signal is documented to arrive up to ~60s early, and SIGTERM
arrives too late to be the only plan for a large checkpoint. Whichever fires
first wins; the rest become no-ops.

The guard's ctx must NOT be a context cancelled by SIGTERM — a last-chance
checkpoint has to outlive the signal that asked for it.
*/
type WalltimeGuard struct {
	svc      *CheckpointService
	target   CheckpointTarget
	opts     CreateOptions
	deadline DeadlineProvider
	cfg      WalltimeOptions

	checkpointed atomic.Bool
	checkpointID atomic.Value // string, readable after a successful checkpoint
	ready        chan struct{}
}

// NewWalltimeGuard builds a guard for one workload's process.
func NewWalltimeGuard(svc *CheckpointService, target CheckpointTarget, opts CreateOptions, deadline DeadlineProvider, cfg WalltimeOptions) *WalltimeGuard {
	if len(cfg.PreWalltimeSignals) == 0 {
		cfg.PreWalltimeSignals = []os.Signal{DefaultPreWalltimeSignal}
	}
	// With CheckpointOnSigterm off the guard never watches SIGTERM, which
	// leaves it to main.go's ordinary shutdown path.
	if cfg.CheckpointOnSigterm && len(cfg.LastChanceSignals) == 0 {
		cfg.LastChanceSignals = []os.Signal{syscall.SIGTERM}
	}
	return &WalltimeGuard{svc: svc, target: target, opts: opts, deadline: deadline, cfg: cfg, ready: make(chan struct{})}
}

// Watching is closed once the guard's signal handlers are installed, so a
// caller can know when a scheduler signal will actually be caught.
func (g *WalltimeGuard) Watching() <-chan struct{} { return g.ready }

// CheckpointID reports the checkpoint the guard wrote, or "" if it never fired.
func (g *WalltimeGuard) CheckpointID() string {
	id, _ := g.checkpointID.Load().(string)
	return id
}

/*
Start validates the guard and runs it in the background.

Only process_id targets are supported: the guard has to race the application's
own completion, and only a process linkspan tracks has a channel to race.
*/
func (g *WalltimeGuard) Start(ctx context.Context) error {
	if g.cfg.Margin <= 0 {
		return fmt.Errorf("walltime checkpoint margin must be greater than 0")
	}
	if g.target.Kind != TargetKindProcessID {
		return fmt.Errorf("walltime checkpointing currently only supports process_id targets")
	}
	if err := g.svc.Configured(); err != nil {
		return err
	}

	info, err := pm.GlobalProcessManager.GetInfo(g.target.ProcessID)
	if err != nil {
		return fmt.Errorf("failed to look up process %s: %w", g.target.ProcessID, err)
	}

	go g.run(ctx, info.Done)
	return nil
}

func (g *WalltimeGuard) run(ctx context.Context, appDone <-chan error) {
	watched := append(append([]os.Signal{}, g.cfg.PreWalltimeSignals...), g.cfg.LastChanceSignals...)
	sigCh := make(chan os.Signal, len(watched)+1)
	signal.Notify(sigCh, watched...)
	defer signal.Stop(sigCh)
	close(g.ready)

	// A missing deadline is not fatal: the scheduler signals are an
	// independent trigger and are often the only one on a job with no
	// finite end time.
	var timerC <-chan time.Time
	if at, err := g.checkpointAt(ctx); err != nil {
		log.Printf("[Walltime] no allocation deadline available (%v); relying on scheduler signals only", err)
	} else {
		wait := time.Until(at)
		if wait <= 0 {
			log.Printf("[Walltime] allocation is already inside the %s checkpoint margin; checkpointing now", g.cfg.Margin)
			wait = 0
		} else {
			log.Printf("[Walltime] will checkpoint workload %s at %s (%s from now, %s before the allocation ends)",
				g.opts.WorkloadID, at.Format(time.RFC3339), wait.Truncate(time.Second), g.cfg.Margin)
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timerC = timer.C
	}

	for {
		select {
		case <-timerC:
			if g.attempt(ctx, TriggerWalltime, "allocation walltime approaching") {
				g.awaitShutdownSignal(ctx, sigCh)
				return
			}
			// Keep waiting for a signal: a failed walltime checkpoint may
			// still succeed when the scheduler gives its own warning.
			timerC = nil

		case s := <-sigCh:
			lastChance := g.isLastChance(s)
			ok := g.attempt(ctx, TriggerSignal, fmt.Sprintf("received %s from the scheduler", s))
			if lastChance {
				// SIGTERM must always end in shutdown, checkpoint or not —
				// swallowing it would leave the allocation hung until SIGKILL.
				controller.TriggerShutdown(fmt.Sprintf("received %s", s))
				return
			}
			if ok {
				g.awaitShutdownSignal(ctx, sigCh)
				return
			}

		case <-appDone:
			log.Printf("[Walltime] application %s finished before the deadline; no checkpoint needed", g.target.ProcessID)
			g.svc.MarkWorkloadCompleted(g.opts.WorkloadID)
			g.awaitShutdownSignal(ctx, sigCh)
			return

		case <-ctx.Done():
			log.Printf("[Walltime] linkspan is shutting down; stopping the walltime guard for workload %s", g.opts.WorkloadID)
			return
		}
	}
}

/*
awaitShutdownSignal keeps honouring last-chance signals once there is nothing
left to checkpoint.

The guard holds the only SIGTERM registration while it runs, so if it simply
stopped listening the signal would revert to its default disposition and kill
linkspan outright — no server shutdown, no resource cleanup.
*/
func (g *WalltimeGuard) awaitShutdownSignal(ctx context.Context, sigCh <-chan os.Signal) {
	for {
		select {
		case s := <-sigCh:
			if g.isLastChance(s) {
				controller.TriggerShutdown(fmt.Sprintf("received %s", s))
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// checkpointAt is the deadline arithmetic the whole feature rests on: leave
// margin for the dump to finish before the scheduler kills the allocation.
func (g *WalltimeGuard) checkpointAt(ctx context.Context) (time.Time, error) {
	if g.deadline == nil {
		return time.Time{}, fmt.Errorf("no deadline provider configured")
	}
	end, err := g.deadline.Deadline(ctx)
	if err != nil {
		return time.Time{}, err
	}
	return end.Add(-g.cfg.Margin), nil
}

func (g *WalltimeGuard) isPreWalltime(s os.Signal) bool {
	for _, pre := range g.cfg.PreWalltimeSignals {
		if s == pre {
			return true
		}
	}
	return false
}

func (g *WalltimeGuard) isLastChance(s os.Signal) bool {
	for _, last := range g.cfg.LastChanceSignals {
		if s == last {
			return true
		}
	}
	return false
}

/*
attempt is the guard's single funnel: the walltime timer, the scheduler signal,
and the SIGTERM fallback all reach CheckpointService.CreateCheckpoint through
here and nowhere else, so they cannot drift apart. It reports whether a durable
checkpoint now exists.
*/
func (g *WalltimeGuard) attempt(ctx context.Context, trigger CheckpointTrigger, reason string) bool {
	if g.checkpointed.Load() {
		log.Printf("[Walltime] %s, but workload %s is already checkpointed (%s); ignoring", reason, g.opts.WorkloadID, g.CheckpointID())
		return true
	}

	opts := g.opts
	opts.Trigger = trigger
	opts.Reason = reason

	log.Printf("[Walltime] %s; checkpointing workload %s (trigger=%s)", reason, opts.WorkloadID, trigger)
	result, err := g.svc.CreateCheckpoint(ctx, g.target, opts)
	if err != nil {
		log.Printf("[Walltime] checkpoint of workload %s failed: %v", opts.WorkloadID, err)
		return false
	}

	g.checkpointed.Store(true)
	g.checkpointID.Store(result.CheckpointID)
	log.Printf("[Walltime] checkpoint %s written to %s; restore it in the next allocation with --restore-checkpoint-id %s",
		result.CheckpointID, result.CheckpointDir, result.CheckpointID)

	if g.cfg.ShutdownAfterCheckpoint {
		controller.TriggerShutdown(fmt.Sprintf("checkpoint %s written before walltime", result.CheckpointID))
	}
	return true
}

// ParseSignal maps a configured signal name onto the signal itself, so
// --checkpoint-signal can name whatever the site's sbatch --signal uses.
func ParseSignal(name string) (os.Signal, error) {
	normalized := strings.ToUpper(strings.TrimSpace(name))
	normalized = strings.TrimPrefix(normalized, "SIG")
	switch normalized {
	case "USR1":
		return syscall.SIGUSR1, nil
	case "USR2":
		return syscall.SIGUSR2, nil
	case "TERM":
		return syscall.SIGTERM, nil
	case "INT":
		return syscall.SIGINT, nil
	case "HUP":
		return syscall.SIGHUP, nil
	default:
		return nil, fmt.Errorf("unsupported signal %q, expected one of SIGUSR1, SIGUSR2, SIGTERM, SIGINT, SIGHUP", name)
	}
}
