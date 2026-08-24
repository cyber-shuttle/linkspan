package operations

import (
	"context"
	"flag"
	"fmt"
	"github.com/cyber-shuttle/linkspan/internal/config"
	pm "github.com/cyber-shuttle/linkspan/internal/process"
	"github.com/cyber-shuttle/linkspan/subsystems/checkpoint"
	"github.com/cyber-shuttle/linkspan/subsystems/fork"
	"github.com/cyber-shuttle/linkspan/subsystems/mount"
	"github.com/cyber-shuttle/linkspan/subsystems/tunnel"
	"github.com/cyber-shuttle/linkspan/subsystems/vscode"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func StartAPIDevTunnel(tunnelToken string, tunnelID string,
	tunnelRetries int, tunnelRetryDelay time.Duration,
	tunnelAttemptTimeout time.Duration,
	tunnelCluster string, serverPort int,
	ctx context.Context) error {
	if tunnelToken == "" {
		log.Fatalf("devtunnel: warning — --tunnel-auth-token not provided; tunnel startup will fail")
	}
	go func() {
		// Host a client-created tunnel when an id is supplied; otherwise create our own.
		if tunnelID == "" {
			tunnelID = fmt.Sprintf("linkspan-tunnel-%d", time.Now().UnixNano())
		}

		// cleanupAttempt kills any host CLI process and removes the tunnel
		// from the manager so a timed-out or failed attempt doesn't leak.
		cleanupAttempt := func() {
			info, err := tunnel.GlobalDevTunnelManager.Find(tunnelID)
			if err != nil {
				return // not registered yet, nothing to clean up
			}
			if info.HostCmdID != "" {
				_ = pm.GlobalProcessManager.Kill(info.HostCmdID)
			}
			tunnel.GlobalDevTunnelManager.Remove(tunnelID)
		}

		for attempt := 1; attempt <= tunnelRetries; attempt++ {
			log.Printf("devtunnel: attempt %d/%d to bring up tunnel %s", attempt, tunnelRetries, tunnelID)

			ch := make(chan error, 1)
			go func() {
				conn, err := tunnel.DevTunnelSetup(tunnelID, "1d", tunnelToken, tunnelID != "", tunnelCluster, serverPort)
				if err != nil {
					log.Printf("devtunnel bring-up error: %v", err)
					ch <- err
					return
				}

				log.Printf("Connect to agent using the URL: %s", conn.ConnectionURL)
				log.Printf("DevTunnel ID: %s", conn.DevTunnelInfo.TunnelID)
				log.Printf("DevTunnel Token: %s", conn.Token)
				log.Printf("DevTunnel forwarded ports: %v", conn.DevTunnelInfo.Ports)
				log.Printf("Devtunnel cluster id: %s", conn.DevTunnelInfo.ClusterID)
				ch <- nil
			}()

			attemptCtx, cancel := context.WithTimeout(ctx, tunnelAttemptTimeout)
			select {
			case err := <-ch:
				cancel()
				if err == nil {
					log.Printf("devtunnel: successfully created %s", tunnelID)
					return
				}
				log.Printf("devtunnel: attempt %d failed: %v", attempt, err)
				cleanupAttempt()
			case <-attemptCtx.Done():
				log.Printf("devtunnel: attempt %d timed out after %s", attempt, tunnelAttemptTimeout.String())
				cancel()
				cleanupAttempt()
			}

			if attempt < tunnelRetries {
				time.Sleep(tunnelRetryDelay)
			}
		}

		log.Fatalf("devtunnel: failed to create tunnel %s after %d attempts", tunnelID, tunnelRetries)
	}()
	return nil
}

// repeatableFlag collects a flag given more than once, for values that
// cannot be comma-separated because they are shell commands or paths.
type repeatableFlag []string

func (r *repeatableFlag) String() string { return strings.Join(*r, ", ") }

func (r *repeatableFlag) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func ProcessCommandArguments(c *config.LinkspanConfig) error {
	versionFlag := flag.Bool("version", false, "print version information and exit")
	verboseVersionFlag := flag.Bool("verbose-version", false, "print verbose version information and exit")

	tunnelAPI := flag.String("tunnel-api", "devtunnels", "tunnel API provider name (e.g. devtunnels)")
	tunnelEnable := flag.Bool("tunnel-enable", false, "enable tunnel startup")
	tunnelID := flag.String("tunnel-id", "", "host this client-created dev tunnel id instead of creating one; the client owns its lifecycle")
	tunnelCluster := flag.String("tunnel-cluster", "", "cluster id of the client-created tunnel (required with --tunnel-id to resolve it)")
	tunnelAuthToken := flag.String("tunnel-auth-token", "", "Microsoft Entra ID bearer token for the Dev Tunnels service")
	tunnelRetries := flag.Int("tunnel-retries", 3, "number of retries for tunnel startup")
	tunnelRetryDelay := flag.Duration("tunnel-retry-delay", 2*time.Second, "delay between tunnel startup retries")
	tunnelAttemptTimeout := flag.Duration("tunnel-attempt-timeout", 10*time.Second, "timeout per tunnel setup attempt")
	serverPortFlag := flag.Int("port", 8080, "port for the HTTP server to listen on")
	serverHostFlag := flag.String("host", "0.0.0.0", "host/IP for the HTTP server to bind to")
	forkCommand := flag.String("fork-command", "", "command to execute as a fork process")
	shutdownOnForkCompletionFlag := flag.String("shutdown-on-fork-completion", "false", "gracefully shutdown when fork process completes (true/false)")
	socketPath := flag.String("socket", "", "also listen on this unix socket path (in-cluster access via `srun --jobid`)")
	criuPath := flag.String("criu-path", "", "path to the CRIU binary")
	supportGpuCheckpointFlag := flag.String("support-gpu-checkpoint", "false", "enable GPU checkpoint support (true/false)")
	additionalCriuOptsFlag := flag.String("additional-criu-opts", "", "comma-separated list of additional CRIU options")
	checkpointMode := flag.String("checkpoint-mode", "auto", "checkpoint mode: auto (engage GPU support when the process uses a GPU), cpu, or gpu")
	cudaCheckpointPath := flag.String("cuda-checkpoint-path", "", "path to NVIDIA's cuda-checkpoint binary; looked up on PATH when unset")
	criuLibDir := flag.String("criu-libdir", "", "CRIU plugin directory (passed as --libdir), holding cuda_plugin.so; probed under /usr/local/lib/criu, /usr/lib/criu, /usr/lib64/criu when unset")
	criuPluginDir := flag.String("criu-plugin-dir", "", "deprecated alias for --criu-libdir")
	checkpointNetwork := flag.String("checkpoint-network", "reconstruct", "network state policy: reconstruct (rebuild sockets in the new allocation) or migrate (carry established TCP connections, adds CRIU --tcp-established)")
	checkpointOnSigtermFlag := flag.String("checkpoint-on-sigterm", "true", "take a last-chance checkpoint on SIGTERM when walltime checkpointing is armed (true/false)")
	checkpointRoot := flag.String("checkpoint-root", "", "root directory for durable checkpoint storage; must be shared storage reachable from any allocation (e.g. Lustre, GPFS, NFS, project scratch)")
	workloadID := flag.String("workload-id", "", "logical workload identity checkpoints are grouped under; auto-generated and logged if not provided")
	checkpointForkAfterDelay := flag.Int64("checkpoint-fork-after-delay", 0, "EXPERIMENTAL test path: seconds after the fork process starts before checkpointing it; use --checkpoint-before-walltime for real workloads")
	restoreCheckpoint := flag.String("restore-checkpoint", "", "checkpoint id to restore (its workload is resolved automatically)")
	restoreCheckpointID := flag.String("restore-checkpoint-id", "", "deprecated alias for --restore-checkpoint")
	checkpointBeforeWalltime := flag.String("checkpoint-before-walltime", "", "checkpoint this long before the Slurm allocation ends, e.g. 5m or 90s; empty or 0 disables automatic walltime checkpointing")
	checkpointSignal := flag.String("checkpoint-signal", "SIGUSR1", "signal the scheduler sends as an early walltime warning (match sbatch --signal=<sig>@<seconds>)")
	restoreForceFlag := flag.String("restore-force", "false", "restore even when compatibility checks fail, downgrading their errors to warnings (true/false)")

	var restorePreCommands, restoreEnsureDirs, restoreRequireFiles repeatableFlag
	flag.Var(&restorePreCommands, "restore-pre-command", "shell command to run before a CRIU restore to reconstruct the environment (mount storage, load modules, stage credentials); repeatable, runs in order")
	flag.Var(&restoreEnsureDirs, "restore-ensure-dir", "directory that must exist before a CRIU restore; created if missing (repeatable)")
	flag.Var(&restoreRequireFiles, "restore-require-file", "file that must exist before a CRIU restore, e.g. a credential (repeatable)")

	allowedCheckpointUsersFlag := flag.String("allowed-checkpoint-users", "", "comma-separated list of usernames/uids allowed to be checkpointed (default: linkspan's own user only); use \"*\" to allow any user")
	flag.Parse()

	// Parse boolean flags
	shutdownOnForkCompletion, err := strconv.ParseBool(*shutdownOnForkCompletionFlag)
	if err != nil {
		log.Fatalf("invalid value for --shutdown-on-fork-completion: %s (expected true or false)", *shutdownOnForkCompletionFlag)
	}

	supportGpuCheckpoint, err := strconv.ParseBool(*supportGpuCheckpointFlag)
	if err != nil {
		log.Fatalf("invalid value for --support-gpu-checkpoint: %s (expected true or false)", *supportGpuCheckpointFlag)
	}

	restoreForce, err := strconv.ParseBool(*restoreForceFlag)
	if err != nil {
		log.Fatalf("invalid value for --restore-force: %s (expected true or false)", *restoreForceFlag)
	}

	// Parse additional CRIU options (comma-separated)
	var additionalCriuOpts []string
	if *additionalCriuOptsFlag != "" {
		additionalCriuOpts = strings.Split(*additionalCriuOptsFlag, ",")
		for i := range additionalCriuOpts {
			additionalCriuOpts[i] = strings.TrimSpace(additionalCriuOpts[i])
		}
	}

	// An unparseable margin must not silently disable walltime checkpointing:
	// the whole point of the flag is that the job is expected to be saved.
	var checkpointBeforeWalltimeDuration time.Duration
	if trimmed := strings.TrimSpace(*checkpointBeforeWalltime); trimmed != "" {
		parsed, err := time.ParseDuration(trimmed)
		if err != nil {
			log.Fatalf("invalid value for --checkpoint-before-walltime: %s (expected a duration such as 5m or 90s)", *checkpointBeforeWalltime)
		}
		if parsed < 0 {
			log.Fatalf("invalid value for --checkpoint-before-walltime: %s (must not be negative)", *checkpointBeforeWalltime)
		}
		checkpointBeforeWalltimeDuration = parsed
	}
	// A bad mode must fail at startup, not at the first checkpoint: by then
	// the allocation may be minutes from expiring.
	if err := checkpoint.ValidateMode(*checkpointMode); err != nil {
		log.Fatalf("invalid value for --checkpoint-mode: %v", err)
	}
	if _, err := checkpoint.ParseSignal(*checkpointSignal); err != nil {
		log.Fatalf("invalid value for --checkpoint-signal: %v", err)
	}

	// Parse allowed checkpoint users (comma-separated)
	var allowedCheckpointUsers []string
	if *allowedCheckpointUsersFlag != "" {
		allowedCheckpointUsers = strings.Split(*allowedCheckpointUsersFlag, ",")
		for i := range allowedCheckpointUsers {
			allowedCheckpointUsers[i] = strings.TrimSpace(allowedCheckpointUsers[i])
		}
	}

	c.TunnelApi = *tunnelAPI
	c.EnableAPITunnelAtStartup = *tunnelEnable
	c.TunnelId = *tunnelID
	c.TunnelCluster = *tunnelCluster
	c.TunnelAuthToken = *tunnelAuthToken
	c.TunnelRetries = *tunnelRetries
	c.TunnelRetryDelay = *tunnelRetryDelay
	c.TunnelAttemptTimeout = *tunnelAttemptTimeout
	c.ServerPort = *serverPortFlag
	c.ServerHost = *serverHostFlag
	c.ForkCommand = *forkCommand
	c.ShutdownOnForkCompletion = shutdownOnForkCompletion
	c.RestoreCheckpointID = *restoreCheckpointID
	c.SocketPath = *socketPath
	c.CRIUPath = *criuPath
	c.SupportGpuCheckpoint = supportGpuCheckpoint
	c.AdditionalCriuOpts = additionalCriuOpts
	c.CheckpointRoot = *checkpointRoot
	c.CheckpointMode = *checkpointMode
	c.CudaCheckpointPath = *cudaCheckpointPath
	c.CriuPluginDir = *criuPluginDir
	c.WorkloadID = *workloadID
	c.AllowedCheckpointUsers = allowedCheckpointUsers
	c.CheckpointForkAfterDelay = *checkpointForkAfterDelay
	c.CheckpointBeforeWalltime = checkpointBeforeWalltimeDuration
	c.CheckpointSignal = *checkpointSignal
	c.RestorePreCommands = restorePreCommands
	c.RestoreEnsureDirs = restoreEnsureDirs
	c.RestoreRequireFiles = restoreRequireFiles
	c.RestoreForce = restoreForce
	if *versionFlag {
		fmt.Printf("%s\n", c.Version)
		os.Exit(0)
	}

	if *verboseVersionFlag {
		fmt.Printf("%s\n", c.Version)
		fmt.Printf("  commit:    %s\n", c.Commit)
		fmt.Printf("  built:     %s\n", c.Date)
		fmt.Printf("  built by:  %s\n", c.BuiltBy)
		fmt.Printf("  go:        %s\n", runtime.Version())
		fmt.Printf("  platform:  %s/%s\n", runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}
	return nil
}

// StartForkProcess starts a fork process if a command is provided in the config.
// Returns the process ID if started, or empty string if not started.
func StartForkProcess(c config.LinkspanConfig) (string, error) {
	if c.ForkCommand == "" {
		return "", nil
	}

	log.Printf("Starting fork process: %s with shutdown on completion: %v", c.ForkCommand, c.ShutdownOnForkCompletion)
	fp, err := fork.GlobalForkProcessManager.RunForkProcess(c.ForkCommand, c.ShutdownOnForkCompletion)
	if err != nil {
		return "", fmt.Errorf("failed to start fork process: %w", err)
	}

	log.Printf("Fork process started with ID: %s (shutdown on completion: %v)", fp.InternalProcessId, fp.ShutdownOnCompletion)
	return fp.InternalProcessId, nil
}

func CleanupResources(c config.LinkspanConfig) {
	log.Println("Cleaning up resources before shutdown...")
	mount.CleanupAll()
	fork.GlobalForkProcessManager.KillAllForkProcesses()
	pm.GlobalProcessManager.KillAll()
	tunnel.GlobalDevTunnelManager.CleanAll(c.TunnelAuthToken)
	tunnel.DeleteAllFRPTunnels()
	vscode.StopAllSSHServers()
	log.Println("Resource cleanup completed.")
}
