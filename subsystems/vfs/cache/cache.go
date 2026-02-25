// Package cache implements a caching layer for the remotefs FUSE client.
// It provides data caching, metadata caching, and directory entry caching
// with bounded-staleness consistency: every read validates the file's mtime
// from the server (coalesced within ~1 second) before serving cached data.
package cache

import (
	"context"
	"sync"
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/subsystems/vfs/fileproto"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/wire"
)

// BlockCache defines the interface for block-level data caching.
// Both DataCache (in-memory) and MmapDataCache (file-backed) implement this interface.
type BlockCache interface {
	// Read attempts to read data from cache for the given path, offset, and size.
	Read(path string, offset, size int64) ([]byte, bool)

	// ReadWithMtime reads from cache but validates against source file mtime.
	ReadWithMtime(path string, offset, size, mtime int64) ([]byte, bool)

	// Write caches data for the given path and offset.
	Write(path string, offset int64, data []byte)

	// WriteWithMtime caches data with the source file's mtime for stale detection.
	WriteWithMtime(path string, offset int64, data []byte, mtime int64)

	// Invalidate removes cached blocks that overlap with the given range.
	Invalidate(path string, offset, size int64)

	// InvalidateAll removes all cached blocks for a given path.
	InvalidateAll(path string)

	// InvalidateBeyond removes cached blocks that extend beyond the given size.
	InvalidateBeyond(path string, size int64)

	// Clear removes all entries from the cache.
	Clear()

	// Cleanup removes expired entries (returns count removed).
	Cleanup() int

	// SetMtime records the mtime for a path.
	SetMtime(path string, mtime int64)

	// GetMtime returns the tracked mtime for a path.
	GetMtime(path string) int64

	// IsMtimeStale checks if the given mtime differs from cached.
	IsMtimeStale(path string, mtime int64) bool

	// Size returns the current cache size in bytes.
	Size() int64

	// BlockCount returns the number of blocks cached.
	BlockCount() int
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		MaxDataCacheSize:    256 * 1024 * 1024, // 256 MB
		BlockSize:           256 * 1024,         // 256 KB
		DataTTL:             5 * time.Minute,
		MetadataTTL:         30 * time.Second,
		DirectoryTTL:        30 * time.Second,
		Enabled:             true,
		PrefetchBlocks:      4,
		MaxParallelFetches:  8,
		EnablePrefetch:      true,
		EnableParallelFetch: true,
		UseMmapCache:        false,
		MmapCacheDir:        "",
	}
}

// Config holds cache configuration options.
type Config struct {
	// MaxDataCacheSize is the maximum size in bytes for the data cache.
	MaxDataCacheSize int64

	// BlockSize is the size of each cached block in bytes.
	BlockSize int64

	DataTTL             time.Duration // How long data blocks remain valid (default: 5m).
	MetadataTTL         time.Duration // How long metadata entries remain valid (default: 30s).
	DirectoryTTL        time.Duration // How long directory entries remain valid (default: 30s).
	Enabled             bool
	PrefetchBlocks      int  // Blocks to prefetch ahead during sequential reads.
	MaxParallelFetches  int  // Max concurrent block fetches.
	EnablePrefetch      bool // Enable read-ahead prefetching.
	EnableParallelFetch bool // Enable parallel fetching of multiple blocks.
	UseMmapCache        bool   // Use file-backed mmap block cache instead of in-memory.
	MmapCacheDir        string // Directory for mmap cache files (required when UseMmapCache is true).
}

// readTracker tracks sequential read patterns for prefetching.
type readTracker struct {
	lastPath   string
	lastOffset int64
	lastSize   int64
	sequential bool
	lastAccess time.Time
}

// mtimeCheck records the result of a server mtime validation.
// Used to coalesce many FUSE block reads for the same file into
// a single server round-trip.
type mtimeCheck struct {
	mtime     int64
	checkedAt time.Time
}

// mtimeCheckInterval controls how often handleRead re-validates a file's mtime
// from the server. Between checks, burst reads reuse the cached result.
// This bounds the staleness window for read operations.
const mtimeCheckInterval = 1 * time.Second

// CachedClient wraps a fileproto.Client with caching capabilities.
// It intercepts operations to provide cached responses where appropriate
// and maintains cache consistency on mutations.
type CachedClient struct {
	client    *fileproto.Client
	config    *Config
	dataCache BlockCache // can be DataCache or MmapDataCache
	metaCache *MetadataCache
	dirCache  *DirectoryCache
	mu        sync.RWMutex

	// Read pattern tracking for prefetching
	readTrackers   map[string]*readTracker
	readTrackersMu sync.Mutex

	// Mtime validation tracking for read consistency.
	// Ensures reads always see fresh data by periodically checking
	// the server, while coalescing checks within mtimeCheckInterval.
	mtimeChecks   map[string]mtimeCheck
	mtimeChecksMu sync.RWMutex

	// Prefetch coordination
	prefetchQueue chan prefetchRequest
	prefetchDone  chan struct{}

	// Cleanup coordination
	cleanupDone chan struct{}
}

// prefetchRequest represents a request to prefetch blocks.
type prefetchRequest struct {
	path       string
	blockStart int64
	blockCount int
	mtime      int64 // source file mtime for cache consistency
}

// NewCachedClient creates a new CachedClient wrapping the given fileproto.Client.
func NewCachedClient(client *fileproto.Client, config *Config) *CachedClient {
	if config == nil {
		config = DefaultConfig()
	}

	// Use DataTTL for data cache; fall back to MetadataTTL for backward compatibility
	dataTTL := config.DataTTL
	if dataTTL <= 0 {
		dataTTL = 5 * time.Minute
	}

	// Create the appropriate block cache based on config
	var dataCache BlockCache
	if config.UseMmapCache && config.MmapCacheDir != "" {
		mmapCache, err := NewMmapDataCache(config.MmapCacheDir, config.MaxDataCacheSize, config.BlockSize)
		if err != nil {
			// Fall back to in-memory cache on error
			dataCache = NewDataCacheWithTTL(config.MaxDataCacheSize, config.BlockSize, dataTTL)
		} else {
			dataCache = mmapCache
		}
	} else {
		dataCache = NewDataCacheWithTTL(config.MaxDataCacheSize, config.BlockSize, dataTTL)
	}

	cc := &CachedClient{
		client:        client,
		config:        config,
		dataCache:     dataCache,
		metaCache:     NewMetadataCache(config.MetadataTTL),
		dirCache:      NewDirectoryCache(config.DirectoryTTL),
		readTrackers:  make(map[string]*readTracker),
		mtimeChecks:   make(map[string]mtimeCheck),
		prefetchQueue: make(chan prefetchRequest, 100),
		prefetchDone:  make(chan struct{}),
		cleanupDone:   make(chan struct{}),
	}

	// Start prefetch worker if enabled
	if config.EnablePrefetch {
		go cc.prefetchWorker()
	}

	// Start background cleanup worker
	go cc.cleanupWorker()

	return cc
}

// cleanupWorker periodically removes expired entries from all caches.
func (c *CachedClient) cleanupWorker() {
	// Use a ticker for periodic cleanup (every 30 seconds)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.runCleanup()
		case <-c.cleanupDone:
			return
		}
	}
}

// runCleanup removes expired entries from all caches.
func (c *CachedClient) runCleanup() {
	// Cleanup data cache (expired blocks)
	c.dataCache.Cleanup()

	// Cleanup metadata cache (expired entries)
	c.metaCache.Cleanup()

	// Cleanup directory cache (expired entries)
	c.dirCache.Cleanup()

	// Cleanup stale read trackers and mtime check entries
	c.cleanupReadTrackers()
	c.cleanupMtimeChecks()
}

// invalidateMtimeCheck removes the mtime check entry for a path, forcing
// the next read to re-validate from the server.
func (c *CachedClient) invalidateMtimeCheck(path string) {
	c.mtimeChecksMu.Lock()
	delete(c.mtimeChecks, path)
	c.mtimeChecksMu.Unlock()
}

// cleanupMtimeChecks removes mtime check entries that haven't been used recently.
func (c *CachedClient) cleanupMtimeChecks() {
	c.mtimeChecksMu.Lock()
	defer c.mtimeChecksMu.Unlock()

	threshold := time.Now().Add(-5 * time.Minute)
	for path, check := range c.mtimeChecks {
		if check.checkedAt.Before(threshold) {
			delete(c.mtimeChecks, path)
		}
	}
}

// cleanupReadTrackers removes stale read tracker entries.
func (c *CachedClient) cleanupReadTrackers() {
	c.readTrackersMu.Lock()
	defer c.readTrackersMu.Unlock()

	now := time.Now()
	staleThreshold := now.Add(-5 * time.Minute)

	for path, tracker := range c.readTrackers {
		if tracker.lastAccess.Before(staleThreshold) {
			delete(c.readTrackers, path)
		}
	}
}

// prefetchWorker handles background prefetching of blocks.
func (c *CachedClient) prefetchWorker() {
	for {
		select {
		case req := <-c.prefetchQueue:
			c.doPrefetch(req)
		case <-c.prefetchDone:
			return
		}
	}
}

// doPrefetch fetches blocks in the background.
func (c *CachedClient) doPrefetch(req prefetchRequest) {
	// Use a short-lived context with timeout to prevent prefetches from running indefinitely
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	blockSize := c.config.BlockSize

	for i := 0; i < req.blockCount; i++ {
		// Check if we should stop
		select {
		case <-c.prefetchDone:
			return
		case <-ctx.Done():
			return
		default:
		}

		blockIdx := req.blockStart + int64(i)
		offset := blockIdx * blockSize

		if _, complete := c.dataCache.ReadWithMtime(req.path, offset, blockSize, req.mtime); complete {
			continue
		}

		// Fetch the block
		resp, err := c.client.Do(ctx, &wire.Request{
			Op:     wire.OpRead,
			Path:   req.path,
			Offset: offset,
			Size:   uint32(blockSize),
		})
		if err != nil || resp.Errno != 0 {
			continue
		}

		if len(resp.Data) > 0 {
			c.dataCache.WriteWithMtime(req.path, offset, resp.Data, req.mtime)
		}
	}
}

// Close stops the prefetch and cleanup workers.
func (c *CachedClient) Close() {
	if c.config.EnablePrefetch {
		close(c.prefetchDone)
	}
	close(c.cleanupDone)
}

// Do routes a file request through the cache layer.
func (c *CachedClient) Do(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	if !c.config.Enabled {
		return c.client.Do(ctx, req)
	}

	switch req.Op {
	case wire.OpGetAttr:
		return c.handleGetAttr(ctx, req)
	case wire.OpLookup:
		return c.handleLookup(ctx, req)
	case wire.OpRead:
		return c.handleRead(ctx, req)
	case wire.OpWrite:
		return c.handleWrite(ctx, req)
	case wire.OpReaddir:
		return c.handleReaddir(ctx, req)
	case wire.OpCreate:
		return c.handleCreate(ctx, req)
	case wire.OpMkdir:
		return c.handleMkdir(ctx, req)
	case wire.OpUnlink:
		return c.handleUnlink(ctx, req)
	case wire.OpRmdir:
		return c.handleRmdir(ctx, req)
	case wire.OpRename:
		return c.handleRename(ctx, req)
	case wire.OpSetAttr:
		return c.handleSetAttr(ctx, req)
	default:
		// For other operations, pass through without caching
		return c.client.Do(ctx, req)
	}
}

// handleGetAttr returns cached metadata if valid, otherwise fetches and caches.
func (c *CachedClient) handleGetAttr(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	// Try to get from cache
	if attr, ok := c.metaCache.Get(req.Path); ok {
		return &wire.Response{
			ID:   req.ID,
			Op:   req.Op,
			Attr: attr,
		}, nil
	}

	// Cache miss - fetch from remote
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}

	// Cache the result if successful
	if resp.Errno == 0 && resp.Attr != nil {
		c.metaCache.Set(req.Path, resp.Attr)
	}

	return resp, nil
}

// handleLookup returns cached metadata if valid, otherwise fetches and caches.
func (c *CachedClient) handleLookup(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	childPath := req.Name
	if req.Path != "" && req.Path != "/" {
		childPath = req.Path + "/" + req.Name
	}

	// Try to get from cache
	if attr, ok := c.metaCache.Get(childPath); ok {
		return &wire.Response{
			ID:   req.ID,
			Op:   req.Op,
			Attr: attr,
		}, nil
	}

	// Cache miss - fetch from remote
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}

	// Cache the result if successful
	if resp.Errno == 0 && resp.Attr != nil {
		c.metaCache.Set(childPath, resp.Attr)
	}

	return resp, nil
}

// handleRead returns cached data blocks if valid, otherwise fetches and caches.
// Uses parallel fetching for large reads and triggers prefetching.
//
// Consistency: mtime is always validated from the server (coalesced within
// mtimeCheckInterval to avoid per-block round-trips). This ensures stale
// data is never served beyond the check interval.
func (c *CachedClient) handleRead(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	blockSize := c.config.BlockSize
	size := int64(req.Size)

	// Validate mtime from server. Checks are coalesced within mtimeCheckInterval
	// so burst reads (many FUSE blocks for one file) share a single round-trip.
	mtime := c.getValidatedMtime(ctx, req.Path)

	data, complete := c.dataCache.ReadWithMtime(req.Path, req.Offset, size, mtime)
	if complete {
		c.triggerPrefetch(req.Path, req.Offset, size, mtime)
		return &wire.Response{
			ID:   req.ID,
			Op:   req.Op,
			Data: data,
		}, nil
	}

	// Calculate which blocks we need
	startBlock := req.Offset / blockSize
	endBlock := (req.Offset + size - 1) / blockSize
	numBlocks := int(endBlock - startBlock + 1)

	// For small reads or if parallel fetch is disabled, use simple fetch
	if numBlocks <= 1 || !c.config.EnableParallelFetch {
		resp, err := c.client.Do(ctx, req)
		if err != nil {
			return nil, err
		}

		if resp.Errno == 0 && len(resp.Data) > 0 {
			c.dataCache.WriteWithMtime(req.Path, req.Offset, resp.Data, mtime)
			c.triggerPrefetch(req.Path, req.Offset, size, mtime)
		}
		return resp, nil
	}

	// Parallel fetch for multiple blocks
	type blockResult struct {
		blockIdx int64
		data     []byte
		err      error
	}

	results := make(chan blockResult, numBlocks)
	semaphore := make(chan struct{}, c.config.MaxParallelFetches)

	// Fetch each block in parallel
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		go func(idx int64) {
			// Acquire semaphore, but respect context cancellation
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- blockResult{blockIdx: idx, err: ctx.Err()}
				return
			}

			blockOffset := idx * blockSize
			blockReadSize := blockSize

			// Adjust for partial first/last blocks
			readStart := int64(0)
			if idx == startBlock {
				readStart = req.Offset - blockOffset
			}
			readEnd := blockSize
			if idx == endBlock {
				readEnd = (req.Offset + size) - blockOffset
				if readEnd > blockSize {
					readEnd = blockSize
				}
			}
			_ = readStart // used in assembly below

			if cached, ok := c.dataCache.ReadWithMtime(req.Path, blockOffset, blockReadSize, mtime); ok {
				results <- blockResult{blockIdx: idx, data: cached}
				return
			}

			// Fetch from remote
			resp, err := c.client.Do(ctx, &wire.Request{
				Op:       wire.OpRead,
				Path:     req.Path,
				HandleID: req.HandleID,
				Offset:   blockOffset,
				Size:     uint32(blockReadSize),
			})

			if err != nil {
				results <- blockResult{blockIdx: idx, err: err}
				return
			}

			if resp.Errno != 0 {
				results <- blockResult{blockIdx: idx, err: errnoFromU32(resp.Errno)}
				return
			}

			_ = readEnd // used in assembly below
			c.dataCache.WriteWithMtime(req.Path, blockOffset, resp.Data, mtime)
			results <- blockResult{blockIdx: idx, data: resp.Data}
		}(blockIdx)
	}

	// Collect results
	blockData := make(map[int64][]byte)
	var firstErr error
	for i := 0; i < numBlocks; i++ {
		result := <-results
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
		if result.data != nil {
			blockData[result.blockIdx] = result.data
		}
	}

	if firstErr != nil {
		return nil, firstErr
	}

	// Assemble the result from blocks
	resultData := make([]byte, 0, size)
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		bd, ok := blockData[blockIdx]
		if !ok {
			continue
		}

		blockOffset := blockIdx * blockSize
		readStart := int64(0)
		if blockIdx == startBlock {
			readStart = req.Offset - blockOffset
		}
		readEnd := int64(len(bd))
		if blockIdx == endBlock {
			maxEnd := (req.Offset + size) - blockOffset
			if maxEnd < readEnd {
				readEnd = maxEnd
			}
		}

		if readStart < int64(len(bd)) && readEnd <= int64(len(bd)) && readStart < readEnd {
			resultData = append(resultData, bd[readStart:readEnd]...)
		}
	}

	// Trigger prefetch for sequential reads
	c.triggerPrefetch(req.Path, req.Offset, size, mtime)

	return &wire.Response{
		ID:   req.ID,
		Op:   req.Op,
		Data: resultData,
	}, nil
}

// getValidatedMtime returns a server-validated mtime for the given path.
// Within mtimeCheckInterval of the last check, returns the cached result to
// coalesce burst FUSE reads. After the interval, fetches fresh mtime from
// the server. If the server mtime differs from the data cache's mtime,
// stale data blocks are proactively evicted.
func (c *CachedClient) getValidatedMtime(ctx context.Context, path string) int64 {
	now := time.Now()

	c.mtimeChecksMu.RLock()
	if check, ok := c.mtimeChecks[path]; ok && now.Sub(check.checkedAt) < mtimeCheckInterval {
		c.mtimeChecksMu.RUnlock()
		return check.mtime
	}
	c.mtimeChecksMu.RUnlock()

	// Fetch fresh mtime from server.
	resp, err := c.client.Do(ctx, &wire.Request{Op: wire.OpGetAttr, Path: path})
	if err != nil || resp.Errno != 0 || resp.Attr == nil {
		return 0
	}

	mtime := int64(resp.Attr.Mtime)

	// If mtime changed, proactively evict stale data blocks.
	if oldMtime := c.dataCache.GetMtime(path); oldMtime > 0 && oldMtime != mtime {
		c.dataCache.InvalidateAll(path)
	}

	// Record the check result.
	c.mtimeChecksMu.Lock()
	c.mtimeChecks[path] = mtimeCheck{mtime: mtime, checkedAt: now}
	c.mtimeChecksMu.Unlock()

	// Also refresh metadata cache (benefits getattr/lookup).
	c.metaCache.Set(path, resp.Attr)

	return mtime
}

// triggerPrefetch queues prefetch requests for sequential read patterns.
func (c *CachedClient) triggerPrefetch(path string, offset, size, mtime int64) {
	if !c.config.EnablePrefetch || c.config.PrefetchBlocks <= 0 {
		return
	}

	c.readTrackersMu.Lock()
	tracker, ok := c.readTrackers[path]
	if !ok {
		tracker = &readTracker{}
		c.readTrackers[path] = tracker
	}

	// Check if this is a sequential read
	isSequential := tracker.lastPath == path &&
		offset == tracker.lastOffset+tracker.lastSize

	now := time.Now()
	tracker.lastPath = path
	tracker.lastOffset = offset
	tracker.lastSize = size
	tracker.sequential = isSequential
	tracker.lastAccess = now

	// Periodically clean up stale read trackers to prevent memory leak
	// Do this probabilistically to avoid doing it on every read
	if len(c.readTrackers) > 100 && now.UnixNano()%10 == 0 {
		staleThreshold := now.Add(-5 * time.Minute)
		for p, t := range c.readTrackers {
			if t.lastAccess.Before(staleThreshold) {
				delete(c.readTrackers, p)
			}
		}
	}
	c.readTrackersMu.Unlock()

	// Only prefetch if sequential pattern detected
	if isSequential {
		blockSize := c.config.BlockSize
		nextBlockStart := (offset + size + blockSize - 1) / blockSize

		select {
		case c.prefetchQueue <- prefetchRequest{
			path:       path,
			blockStart: nextBlockStart,
			blockCount: c.config.PrefetchBlocks,
			mtime:      mtime,
		}:
		default:
			// Queue full, skip prefetch
		}
	}
}

// errnoFromU32 converts a uint32 errno to an error.
func errnoFromU32(e uint32) error {
	if e == 0 {
		return nil
	}
	return syscall.Errno(e)
}

// handleWrite sends write to remote and invalidates affected cache blocks.
func (c *CachedClient) handleWrite(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	// Write-through: send to remote first
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp.Errno == 0 {
		c.dataCache.Invalidate(req.Path, req.Offset, int64(len(req.Data)))
		c.metaCache.Invalidate(req.Path)
		c.invalidateMtimeCheck(req.Path)
	}

	return resp, nil
}

// handleReaddir returns cached directory entries if valid, otherwise fetches and caches.
func (c *CachedClient) handleReaddir(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	// Try to get from cache
	if entries, ok := c.dirCache.Get(req.Path); ok {
		return &wire.Response{
			ID:      req.ID,
			Op:      req.Op,
			Entries: entries,
		}, nil
	}

	// Cache miss - fetch from remote
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}

	// Cache the result if successful
	if resp.Errno == 0 {
		c.dirCache.Set(req.Path, resp.Entries)
	}

	return resp, nil
}

// handleCreate sends to remote and invalidates parent directory cache.
func (c *CachedClient) handleCreate(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}

	// Invalidate parent directory cache
	if resp.Errno == 0 {
		c.dirCache.Invalidate(req.Path)
	}

	return resp, nil
}

// handleMkdir sends to remote and invalidates parent directory cache.
func (c *CachedClient) handleMkdir(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}

	// Invalidate parent directory cache
	if resp.Errno == 0 {
		c.dirCache.Invalidate(req.Path)
	}

	return resp, nil
}

// handleUnlink sends to remote and invalidates parent directory cache and file caches.
func (c *CachedClient) handleUnlink(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp.Errno == 0 {
		childPath := req.Name
		if req.Path != "" && req.Path != "/" {
			childPath = req.Path + "/" + req.Name
		}
		// Invalidate parent directory cache
		c.dirCache.Invalidate(req.Path)
		// Invalidate deleted file's metadata and data
		c.metaCache.Invalidate(childPath)
		c.dataCache.InvalidateAll(childPath)
	}

	return resp, nil
}

// handleRmdir sends to remote and invalidates parent directory cache.
func (c *CachedClient) handleRmdir(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp.Errno == 0 {
		childPath := req.Name
		if req.Path != "" && req.Path != "/" {
			childPath = req.Path + "/" + req.Name
		}
		// Invalidate parent directory cache
		c.dirCache.Invalidate(req.Path)
		// Invalidate deleted directory's metadata and entries
		c.metaCache.Invalidate(childPath)
		c.dirCache.Invalidate(childPath)
	}

	return resp, nil
}

// handleRename sends to remote and invalidates both old and new parent directory caches.
func (c *CachedClient) handleRename(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp.Errno == 0 {
		oldChildPath := req.Name
		if req.Path != "" && req.Path != "/" {
			oldChildPath = req.Path + "/" + req.Name
		}
		newChildPath := req.NewName
		if req.NewPath != "" && req.NewPath != "/" {
			newChildPath = req.NewPath + "/" + req.NewName
		}

		// Invalidate both parent directories
		c.dirCache.Invalidate(req.Path)
		c.dirCache.Invalidate(req.NewPath)

		// Invalidate old path's metadata and data
		c.metaCache.Invalidate(oldChildPath)
		c.dataCache.InvalidateAll(oldChildPath)

		// Invalidate new path if it existed (overwrite case)
		c.metaCache.Invalidate(newChildPath)
		c.dataCache.InvalidateAll(newChildPath)
	}

	return resp, nil
}

// handleSetAttr sends to remote and invalidates metadata cache.
func (c *CachedClient) handleSetAttr(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp.Errno == 0 {
		c.metaCache.Invalidate(req.Path)
		c.invalidateMtimeCheck(req.Path)

		if req.SetAttrValid&wire.SetAttrSize != 0 {
			c.dataCache.InvalidateBeyond(req.Path, int64(req.SetSize))
		}
	}

	return resp, nil
}

// GetUnderlyingClient returns the underlying fileproto.Client for operations
// that need direct access (e.g., streaming).
func (c *CachedClient) GetUnderlyingClient() *fileproto.Client {
	return c.client
}

// InvalidateAll clears all caches. Useful for forcing a refresh.
func (c *CachedClient) InvalidateAll() {
	c.dataCache.Clear()
	c.metaCache.Clear()
	c.dirCache.Clear()

	c.mtimeChecksMu.Lock()
	c.mtimeChecks = make(map[string]mtimeCheck)
	c.mtimeChecksMu.Unlock()
}

// Stats returns current cache statistics.
func (c *CachedClient) Stats() CacheStats {
	return CacheStats{
		DataCacheSize:    c.dataCache.Size(),
		DataCacheBlocks:  c.dataCache.BlockCount(),
		MetadataEntries:  c.metaCache.Count(),
		DirectoryEntries: c.dirCache.Count(),
	}
}

// CacheStats holds statistics about cache usage.
type CacheStats struct {
	DataCacheSize    int64
	DataCacheBlocks  int
	MetadataEntries  int
	DirectoryEntries int
}
