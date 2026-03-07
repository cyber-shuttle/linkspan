package vfs

import (
	"fmt"
	"log"
	"os/exec"
)

type MountProvider struct {
	DataCache   *DataCache
	SessionName string
}

func NewMountProvider(dc *DataCache) *MountProvider {
	return &MountProvider{DataCache: dc}
}

func (m *MountProvider) Start() error {
	if err := m.DataCache.EnsureCacheDir(); err != nil {
		return fmt.Errorf("failed to create cache dir: %w", err)
	}

	bin, err := m.DataCache.EnsureMutagen()
	if err != nil {
		return fmt.Errorf("failed to ensure mutagen: %w", err)
	}

	_ = exec.Command(bin, "daemon", "start").Run()
	log.Printf("[vfs/mount] Cache dir ready at %s, mutagen daemon started for cache warming", m.DataCache.CacheDir)
	return nil
}

func (m *MountProvider) Stop() {
	if m.SessionName == "" {
		return
	}
	bin := m.DataCache.ResolveMutagenBin()
	if bin == "" {
		m.SessionName = ""
		return
	}
	if err := exec.Command(bin, "sync", "flush", m.SessionName).Run(); err != nil {
		log.Printf("[vfs/mount] Warning: flush failed for %s: %v", m.SessionName, err)
	}
	if err := exec.Command(bin, "sync", "terminate", m.SessionName).Run(); err != nil {
		log.Printf("[vfs/mount] Warning: terminate failed for %s: %v", m.SessionName, err)
	} else {
		log.Printf("[vfs/mount] Terminated cache-warmer: %s", m.SessionName)
	}
	m.SessionName = ""

	if err := m.DataCache.Cleanup(); err != nil {
		log.Printf("[vfs/mount] Warning: cache cleanup failed: %v", err)
	}
}
