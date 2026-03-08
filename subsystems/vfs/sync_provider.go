package vfs

import (
	"fmt"
	"log"
	"os/exec"
)

type SyncProvider struct {
	DataCache   *DataCache
	SessionName string
}

func NewSyncProvider(dc *DataCache) *SyncProvider {
	return &SyncProvider{DataCache: dc}
}

func (s *SyncProvider) Start() error {
	if err := s.DataCache.EnsureCacheDir(); err != nil {
		return fmt.Errorf("failed to create cache dir: %w", err)
	}

	bin, err := s.DataCache.EnsureMutagen()
	if err != nil {
		return fmt.Errorf("failed to ensure mutagen: %w", err)
	}

	_ = exec.Command(bin, "daemon", "start").Run()
	log.Printf("[vfs/sync] Cache dir ready at %s, mutagen daemon started", s.DataCache.CacheDir)
	return nil
}

func (s *SyncProvider) Stop() {
	bin := s.DataCache.ResolveMutagenBin()

	if s.SessionName != "" && bin != "" {
		if err := exec.Command(bin, "sync", "flush", s.SessionName).Run(); err != nil {
			log.Printf("[vfs/sync] Warning: flush failed for %s: %v", s.SessionName, err)
		}
		if err := exec.Command(bin, "sync", "terminate", s.SessionName).Run(); err != nil {
			log.Printf("[vfs/sync] Warning: terminate failed for %s: %v", s.SessionName, err)
		} else {
			log.Printf("[vfs/sync] Terminated session: %s", s.SessionName)
		}
		s.SessionName = ""
	}

	// Stop the daemon even if no named session was created.
	if bin != "" {
		_ = exec.Command(bin, "daemon", "stop").Run()
	}

	if err := s.DataCache.Cleanup(); err != nil {
		log.Printf("[vfs/sync] Warning: cache cleanup failed: %v", err)
	}
}
