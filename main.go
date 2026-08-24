package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/config"
	"github.com/cyber-shuttle/linkspan/internal/controller"
	"github.com/cyber-shuttle/linkspan/internal/logstream"
	ops "github.com/cyber-shuttle/linkspan/internal/operations"
	"github.com/cyber-shuttle/linkspan/internal/workflow"
	"github.com/cyber-shuttle/linkspan/subsystems/checkpoint"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs"
	"github.com/gorilla/mux"
)

// Version information set via ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
	builtBy = "unknown"
)

// VFS providers initialized at startup, cleaned up on shutdown.
var (
	vfsSyncProvider  *vfs.SyncProvider
	vfsMountProvider *vfs.MountProvider
)

func main() {

	c := config.NewDefaultLinkspanConfig()
	c.Commit = commit
	c.BuiltBy = builtBy
	c.Date = date
	c.Version = version
	ops.ProcessCommandArguments(c)

	// Install log broadcaster so connected clients receive log output in
	// real time.  Must happen before any log.* calls.
	logBroadcaster := logstream.New(os.Stderr)
	logBroadcaster.Install()

	// Support users passing `--tunnel-api=devtunnels` by trimming leading '='
	apiTunnelType := strings.TrimLeft(c.TunnelApi, "=")

	// The walltime guard takes ownership of SIGTERM when it is armed: a
	// last-chance checkpoint has to run *because* of that signal, and a
	// context cancelled by it would abort the very dump it asked for.
	// Ctrl+C still shuts down immediately either way.
	walltimeArmed := c.CheckpointBeforeWalltime > 0 && c.CRIUPath != "" && c.CheckpointRoot != ""
	shutdownSignals := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if walltimeArmed && c.CheckpointOnSigterm {
		shutdownSignals = []os.Signal{os.Interrupt}
	}

	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals...)
	defer stop()

	// guardCtx deliberately does not descend from ctx, so the guard outlives
	// the shutdown signal long enough to finish writing a checkpoint.
	guardCtx, cancelGuard := context.WithCancel(context.Background())
	defer cancelGuard()

	restoreRequested := c.RestoreCheckpointID != ""
	if restoreRequested && c.ForkCommand != "" {
		log.Fatalf("Can not perform restore and fork execution at same time")
	}

	// A restore inherits its workload id from the checkpoint, so only a
	// fresh workload needs one minted here.
	if c.WorkloadID == "" && !restoreRequested {
		c.WorkloadID = checkpoint.NewWorkloadID()
		log.Printf("No --workload-id provided; generated workload id %s (record this to restore this workload later)", c.WorkloadID)
	}

	// svc is the only thing main.go talks to for checkpoint/restore — all
	// CRIU mechanics live behind it in the checkpoint package. Installed
	// before the routes are served so /checkpoints has something to act on.
	svc := checkpoint.NewCheckpointService(c)
	checkpoint.GlobalCheckpointService = svc

	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	RegisterRoutes(api)

	// Use the configured server host and port from CLI flags.
	// Port 0 means "let the OS pick a free port".
	if c.ServerPort < 0 || c.ServerPort > 65535 {
		log.Fatalf("invalid server port: %d", c.ServerPort)
	}
	addr := fmt.Sprintf("%s:%d", c.ServerHost, c.ServerPort)

	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	// Create listener first so the port is bound before starting any
	// external tunnel process that expects the port to be open.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}

	// When port 0 was requested, update serverPort to the actual bound port.
	if c.ServerPort == 0 {
		c.ServerPort = listener.Addr().(*net.TCPAddr).Port
	}
	log.Printf("listening on %s:%d", c.ServerHost, c.ServerPort)

	if apiTunnelType == "devtunnels" && c.EnableAPITunnelAtStartup {
		ops.StartAPIDevTunnel(c.TunnelAuthToken, c.TunnelId, c.TunnelRetries,
			c.TunnelRetryDelay, c.TunnelAttemptTimeout, c.TunnelCluster,
			c.ServerPort, ctx)
	} else if apiTunnelType == "devtunnels" {
		log.Println("devtunnel startup skipped (disabled via flag)")
	}

	if c.SocketPath != "" {
		if _, err := listenUnix(srv, c.SocketPath); err != nil {
			log.Fatalf("failed to listen on unix socket %s: %v", c.SocketPath, err)
		}
		log.Printf("also listening on unix socket %s", c.SocketPath)
	}

	if restoreRequested {
		log.Printf("Restoring checkpoint %s", c.RestoreCheckpointID)
		result, err := svc.RestoreCheckpoint(ctx, c.RestoreCheckpointID, checkpoint.RestoreOptions{
			ShutdownOnCompletion: c.ShutdownOnForkCompletion,
			PreRestoreCommands:   c.RestorePreCommands,
			EnsureDirs:           c.RestoreEnsureDirs,
			RequireFiles:         c.RestoreRequireFiles,
			Force:                c.RestoreForce,
		})
		if err != nil {
			log.Fatalf("Failed to restore checkpoint %s: %v", c.RestoreCheckpointID, err)
		}

		// The restored workload keeps the identity recorded in the
		// checkpoint, so this allocation reports the same workload id as
		// the allocation that checkpointed it.
		c.WorkloadID = result.WorkloadID
		svc.SetDefaultWorkloadID(result.WorkloadID)
		log.Printf("Restore completed successfully (workload=%s process_id=%s pid=%d)", result.WorkloadID, result.ProcessID, result.Pid)

		armWalltimeGuard(guardCtx, c, svc, result.WorkloadID, result.ProcessID, walltimeArmed)

		if c.CheckpointForkAfterDelay > 0 {
			log.Printf("waiting %d seconds before re-checkpointing restored process %s", c.CheckpointForkAfterDelay, result.ProcessID)
			target := checkpoint.TargetFromProcessID(result.ProcessID)
			opts := checkpoint.CreateOptions{WorkloadID: result.WorkloadID, Trigger: checkpoint.TriggerManual}
			delay := time.Duration(c.CheckpointForkAfterDelay) * time.Second
			if err := svc.ScheduleCheckpoint(ctx, target, opts, delay); err != nil {
				log.Printf("failed to schedule checkpoint for restored process %s: %v", result.ProcessID, err)
			}
		}
	}

	// Start fork process if specified
	if c.ForkCommand != "" {
		internalProcessId, err := ops.StartForkProcess(*c)
		if err != nil {
			log.Fatalf("Failed to start fork process: %v", err)
		}

		armWalltimeGuard(guardCtx, c, svc, c.WorkloadID, internalProcessId, walltimeArmed)

		if c.CheckpointForkAfterDelay > 0 && c.CRIUPath != "" {
			log.Printf("waiting %d seconds before checkpointing fork process %s", c.CheckpointForkAfterDelay, internalProcessId)
			target := checkpoint.TargetFromProcessID(internalProcessId)
			opts := checkpoint.CreateOptions{WorkloadID: c.WorkloadID, Trigger: checkpoint.TriggerManual}
			delay := time.Duration(c.CheckpointForkAfterDelay) * time.Second
			if err := svc.ScheduleCheckpoint(ctx, target, opts, delay); err != nil {
				log.Printf("failed to schedule checkpoint for process %s: %v", internalProcessId, err)
			}
		}
	}

	// Run server
	serverErr := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		serverErr <- err
	}()

	// The workflow starts only once the server is listening: its actions drive
	// linkspan's own subsystems, and a tunnel or vscode step expects the bound
	// port to already be real.
	if c.WorkflowPath != "" {
		if err := startWorkflow(ctx, c); err != nil {
			log.Fatalf("failed to load workflow %s: %v", c.WorkflowPath, err)
		}
	}

	select {
	case <-ctx.Done():
		log.Println("Shutdown signal received...")
	case reason := <-controller.ExternalShutdownChannel:
		log.Printf("Shutdown triggered: %s", reason)
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("server error: %v", err)
		}
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown error: %v — forcing close", err)
		// Shutdown did not complete within the deadline; force-close all
		// remaining connections so the process does not hang indefinitely.
		if closeErr := srv.Close(); closeErr != nil {
			log.Printf("server force-close error: %v", closeErr)
		}
	}

	ops.CleanupResources(*c)

	log.Println("Server gracefully stopped.")
}

// listenUnix serves srv on a unix socket in a background goroutine.
func listenUnix(srv *http.Server, path string) (net.Listener, error) {
	os.Remove(path) // clear a stale socket; bind fails if the path exists
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("unix socket server error: %v", err)
		}
	}()
	return ln, nil
}

/*
armWalltimeGuard turns on automatic checkpointing before the Slurm allocation
expires, for whichever application this allocation is running — one just
started, or one just restored from an earlier allocation.

Linkspan never submits the next allocation itself: it writes the checkpoint and
logs the id, and an external script hands that id to the next sbatch.
*/
func armWalltimeGuard(ctx context.Context, c *config.LinkspanConfig, svc *checkpoint.CheckpointService, workloadID, processID string, armed bool) {
	if !armed {
		return
	}
	if !checkpoint.InSlurm() {
		log.Printf("--checkpoint-before-walltime is set but SLURM_JOB_ID is unset; walltime checkpointing only applies inside a Slurm allocation")
		return
	}

	sig, err := checkpoint.ParseSignal(c.CheckpointSignal)
	if err != nil {
		log.Printf("walltime checkpointing disabled: %v", err)
		return
	}

	guard := checkpoint.NewWalltimeGuard(
		svc,
		checkpoint.TargetFromProcessID(processID),
		checkpoint.CreateOptions{WorkloadID: workloadID},
		checkpoint.NewSlurmDeadlineProvider(),
		checkpoint.WalltimeOptions{
			Margin:              c.CheckpointBeforeWalltime,
			PreWalltimeSignals:  []os.Signal{sig},
			CheckpointOnSigterm: c.CheckpointOnSigterm,
			// The allocation is ending anyway, so release it as soon as the
			// checkpoint is durable rather than idling until walltime.
			ShutdownAfterCheckpoint: true,
		},
	)

	if err := guard.Start(ctx); err != nil {
		log.Printf("failed to arm walltime checkpointing for process %s: %v", processID, err)
		return
	}
	log.Printf("walltime checkpointing armed for Slurm job %s: workload %s will be checkpointed %s before the allocation ends, or on %s",
		checkpoint.SlurmJobID(), workloadID, c.CheckpointBeforeWalltime, sig)
}

/*
startWorkflow loads the workflow and runs it in the background.

A failing step is logged rather than fatal, and the HTTP server keeps serving:
the workflow is one way to drive linkspan, not the reason the process exists —
an operator still needs /status to see what failed, and the tunnel to reach it.

ctx is the shutdown context, which Engine.Run checks between steps, so a signal
interrupts a workflow without waiting for the current step.
*/
func startWorkflow(ctx context.Context, c *config.LinkspanConfig) error {
	var (
		wf  *workflow.WorkflowConfig
		err error
	)
	if c.WorkflowPath == "-" {
		wf, err = workflow.LoadReader(os.Stdin)
	} else {
		wf, err = workflow.LoadFile(c.WorkflowPath)
	}
	if err != nil {
		return err
	}

	// Seed the variables a workflow cannot discover for itself, so steps can
	// interpolate this allocation's identity with {{.WorkloadID}} and friends.
	engine := workflow.NewEngine(workflow.DefaultRegistry(), map[string]any{
		"WorkloadID":     c.WorkloadID,
		"ServerHost":     c.ServerHost,
		"ServerPort":     c.ServerPort,
		"CheckpointRoot": c.CheckpointRoot,
		"SlurmJobID":     checkpoint.SlurmJobID(),
	})
	workflow.GlobalEngine = engine

	log.Printf("running workflow %q (%d steps) from %s", wf.Name, len(wf.Steps), c.WorkflowPath)
	go func() {
		if err := engine.Run(ctx, wf); err != nil {
			log.Printf("workflow %q failed: %v", wf.Name, err)
		}
	}()
	return nil
}
