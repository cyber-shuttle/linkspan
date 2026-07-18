package config

import (
	"time"
)

type LinkspanConfig struct {
	ServerPort int    `yaml:"server_port"`
	ServerHost string `yaml:"server_host"`

	CRIUPath             string   `yaml:"criu_path"`
	SupportGpuCheckpoint bool     `yaml:"support_gpu_checkpoint"`
	AdditionalCriuOpts   []string `yaml:"additional_criu_opts"`
	DumpDirRoot          string   `yaml:"dump_dir_root"`

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
}

func NewDefaultLinkspanConfig() *LinkspanConfig {
	return &LinkspanConfig{
		ServerPort:               8080,
		ServerHost:               "0.0.0.0",
		CRIUPath:                 "/usr/sbin/criu",
		SupportGpuCheckpoint:     false,
		AdditionalCriuOpts:       []string{},
		DumpDirRoot:              "/tmp/linkspan_dumps",
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
	}
}
