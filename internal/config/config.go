package config

import (
	"time"
)

type LinkspanConfig struct {
	ServerPort int    `yaml:"server_port"`
	ServerHost string `yaml:"server_host"`
	SocketPath string `yaml:"socket_path"`

	CRIUPath               string   `yaml:"criu_path"`
	SupportGpuCheckpoint   bool     `yaml:"support_gpu_checkpoint"`
	AdditionalCriuOpts     []string `yaml:"additional_criu_opts"`
	CheckpointRoot         string   `yaml:"checkpoint_root"`
	WorkloadID             string   `yaml:"workload_id"`
	AllowedCheckpointUsers []string `yaml:"allowed_checkpoint_users"`

	TunnelApi                string        `yaml:"tunnel_api"`
	EnableAPITunnelAtStartup bool          `yaml:"enable_api_tunnel_at_startup"`
	TunnelId                 string        `yaml:"tunnel_id"`
	TunnelCluster            string        `yaml:"tunnel_cluster"`
	TunnelAuthToken          string        `yaml:"tunnel_auth_token"`
	TunnelRetries            int           `yaml:"tunnel_retries"`
	TunnelRetryDelay         time.Duration `yaml:"tunnel_retry_delay"`
	TunnelAttemptTimeout     time.Duration `yaml:"tunnel_attempt_timeout"`

	Commit  string `yaml:"commit"`
	BuiltBy string `yaml:"built_by"`
	Date    string `yaml:"date"`
	Version string `yaml:"version"`

	ForkCommand              string `yaml:"fork_command"`
	ShutdownOnForkCompletion bool   `yaml:"shutdown_on_fork_completion"`
	CheckpointForkAfterDelay int64  `yaml:"checkpoint_fork_after_delay"`
	RestoreCheckpointID      string `yaml:"restore_checkpoint_id"`

	// Prerequisites the new allocation must satisfy before a CRIU restore.
	RestorePreCommands  []string `yaml:"restore_pre_commands"`
	RestoreEnsureDirs   []string `yaml:"restore_ensure_dirs"`
	RestoreRequireFiles []string `yaml:"restore_require_files"`
	RestoreForce        bool     `yaml:"restore_force"`

	// Automatic checkpointing before a Slurm allocation expires.
	CheckpointBeforeWalltime time.Duration `yaml:"checkpoint_before_walltime"`
	CheckpointSignal         string        `yaml:"checkpoint_signal"`
}

func NewDefaultLinkspanConfig() *LinkspanConfig {
	return &LinkspanConfig{
		ServerPort:               8080,
		ServerHost:               "0.0.0.0",
		SocketPath:               "",
		CRIUPath:                 "",
		SupportGpuCheckpoint:     false,
		AdditionalCriuOpts:       []string{},
		CheckpointRoot:           "",
		WorkloadID:               "",
		AllowedCheckpointUsers:   []string{},
		TunnelApi:                "devtunnels",
		EnableAPITunnelAtStartup: false,
		TunnelId:                 "",
		TunnelCluster:            "",
		TunnelAuthToken:          "",
		TunnelRetries:            3,
		TunnelRetryDelay:         2 * time.Second,
		TunnelAttemptTimeout:     10 * time.Second,
		ForkCommand:              "",
		ShutdownOnForkCompletion: false,
		CheckpointForkAfterDelay: 0,
		RestoreCheckpointID:      "",
		RestorePreCommands:       []string{},
		RestoreEnsureDirs:        []string{},
		RestoreRequireFiles:      []string{},
		RestoreForce:             false,
		CheckpointBeforeWalltime: 0,
		CheckpointSignal:         "SIGUSR1",
	}
}
