package vfs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type DataCache struct {
	SessionID  string
	CacheDir   string
	mutagenBin string
}

func NewDataCache(sessionID string) (*DataCache, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}
	return &DataCache{
		SessionID: sessionID,
		CacheDir:  filepath.Join(home, "sessions", sessionID),
	}, nil
}

func (d *DataCache) EnsureCacheDir() error {
	return os.MkdirAll(d.CacheDir, 0755)
}

func (d *DataCache) Cleanup() error {
	return os.RemoveAll(d.CacheDir)
}

func (d *DataCache) ResolveMutagenBin() string {
	if d.mutagenBin != "" {
		return d.mutagenBin
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	candidates := []string{
		filepath.Join(home, ".cybershuttle", "bin", "mutagen"),
		"/opt/homebrew/bin/mutagen",
		"/usr/local/bin/mutagen",
		"/usr/bin/mutagen",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			d.mutagenBin = p
			return p
		}
	}
	if p, err := exec.LookPath("mutagen"); err == nil {
		d.mutagenBin = p
		return p
	}
	return ""
}

func (d *DataCache) EnsureMutagen() (string, error) {
	if bin := d.ResolveMutagenBin(); bin != "" {
		return bin, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	binDir := filepath.Join(home, ".cybershuttle", "bin")
	binPath := filepath.Join(binDir, "mutagen")

	osName := runtime.GOOS
	archName := runtime.GOARCH
	if osName != "darwin" && osName != "linux" {
		return "", fmt.Errorf("unsupported platform: %s/%s", osName, archName)
	}

	out, err := exec.Command("bash", "-c",
		`curl -fsSL "https://api.github.com/repos/mutagen-io/mutagen/releases/latest" | grep '"tag_name"' | head -1`,
	).Output()
	if err != nil {
		return "", fmt.Errorf("failed to fetch mutagen version: %w", err)
	}
	version := ""
	for _, part := range strings.Split(string(out), `"`) {
		if strings.HasPrefix(part, "v") && strings.Contains(part, ".") {
			version = part
			break
		}
	}
	if version == "" {
		return "", fmt.Errorf("failed to parse mutagen version from: %s", string(out))
	}

	asset := fmt.Sprintf("mutagen_%s_%s_%s.tar.gz", osName, archName, version)
	url := fmt.Sprintf("https://github.com/mutagen-io/mutagen/releases/latest/download/%s", asset)

	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", err
	}

	cmd := exec.Command("bash", "-c", fmt.Sprintf(
		`curl -fsSL "%s" | tar -xz -C "%s" mutagen mutagen-agents.tar.gz && chmod +x "%s"`,
		url, binDir, binPath,
	))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to download mutagen: %w", err)
	}

	d.mutagenBin = binPath
	return binPath, nil
}
