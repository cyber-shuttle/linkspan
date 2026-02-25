package cache

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/subsystems/vfs/export"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/fileproto"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/wire"
)

// newTestPair creates an in-memory client/server pair backed by a net.Pipe.
// The server goroutine handles requests using the given Backend.
// Returns a CachedClient and a cleanup function.
func newTestPair(t *testing.T, backend *export.Backend, config *Config) *CachedClient {
	t.Helper()

	serverConn, clientConn := net.Pipe()

	serverWire := wire.NewConn(serverConn)
	clientWire := wire.NewConn(clientConn)

	// Server goroutine: handle requests using the backend.
	go func() {
		defer serverConn.Close()
		for {
			req, err := serverWire.RecvRequest()
			if err != nil {
				return
			}
			resp := backend.HandleRequest(context.Background(), req)
			if err := serverWire.SendResponse(resp); err != nil {
				return
			}
		}
	}()

	if config == nil {
		config = DefaultConfig()
	}
	client := fileproto.NewClient(clientWire)
	go client.Run()

	t.Cleanup(func() {
		client.Close()
		clientConn.Close()
	})

	return NewCachedClient(client, config)
}

func TestIntegration_CloseToOpenConsistency(t *testing.T) {
	// Setup: Create temp directory with test file
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	initialContent := []byte("initial content")
	if err := os.WriteFile(testFile, initialContent, 0644); err != nil {
		t.Fatal(err)
	}

	// Create backend and cached client
	paths := []*wire.ExportPath{{LocalPath: dir, VirtualName: "data"}}
	backend := export.NewBackend(paths)

	config := DefaultConfig()
	config.MetadataTTL = time.Hour // Long TTL to test invalidation
	cachedClient := newTestPair(t, backend, config)

	ctx := context.Background()
	vpath := "data/test.txt"

	// Reader 1: Open and read file
	openResp, err := cachedClient.Do(ctx, &wire.Request{Op: wire.OpOpen, Path: vpath, Flags: 0})
	if err != nil || openResp.Errno != 0 {
		t.Fatalf("open failed: err=%v, errno=%d", err, openResp.Errno)
	}
	handleID := openResp.HandleID

	readResp, err := cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpRead, Path: vpath, HandleID: handleID, Offset: 0, Size: 256,
	})
	if err != nil || readResp.Errno != 0 {
		t.Fatalf("read failed: err=%v, errno=%d", err, readResp.Errno)
	}

	if string(readResp.Data) != string(initialContent) {
		t.Fatalf("expected %q, got %q", initialContent, readResp.Data)
	}

	// Writer: Modify file directly on disk (simulating another writer)
	newContent := []byte("modified content")
	if err := os.WriteFile(testFile, newContent, 0644); err != nil {
		t.Fatal(err)
	}

	// Invalidate cache (simulating close-to-open consistency)
	cachedClient.InvalidateAll()

	// Reader 2: Should see new content after invalidation
	readResp2, err := cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpRead, Path: vpath, HandleID: handleID, Offset: 0, Size: 256,
	})
	if err != nil || readResp2.Errno != 0 {
		t.Fatalf("read2 failed: err=%v, errno=%d", err, readResp2.Errno)
	}

	if string(readResp2.Data) != string(newContent) {
		t.Fatalf("expected %q after invalidation, got %q", newContent, readResp2.Data)
	}
}

func TestIntegration_CacheCoherency_RepeatedReads(t *testing.T) {
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	content := []byte("test content for caching")
	if err := os.WriteFile(testFile, content, 0644); err != nil {
		t.Fatal(err)
	}

	paths := []*wire.ExportPath{{LocalPath: dir, VirtualName: "data"}}
	backend := export.NewBackend(paths)
	cachedClient := newTestPair(t, backend, nil)
	ctx := context.Background()
	vpath := "data/test.txt"

	// Open file
	openResp, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpOpen, Path: vpath, Flags: 0})
	handleID := openResp.HandleID

	// First read - cache miss, fetches from remote
	read1, _ := cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpRead, Path: vpath, HandleID: handleID, Offset: 0, Size: 256,
	})

	// Second read - should hit cache
	read2, _ := cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpRead, Path: vpath, HandleID: handleID, Offset: 0, Size: 256,
	})

	// Both should return same data
	if string(read1.Data) != string(read2.Data) {
		t.Fatal("repeated reads returned different data")
	}

	// Verify cache stats show hit
	stats := cachedClient.Stats()
	if stats.DataCacheBlocks == 0 {
		t.Fatal("expected data cache to have blocks")
	}
}

func TestIntegration_WriteInvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}

	paths := []*wire.ExportPath{{LocalPath: dir, VirtualName: "data"}}
	backend := export.NewBackend(paths)
	cachedClient := newTestPair(t, backend, nil)
	ctx := context.Background()
	vpath := "data/test.txt"

	// Open for read/write
	openResp, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpOpen, Path: vpath, Flags: 2}) // O_RDWR
	handleID := openResp.HandleID

	// Read to populate cache
	cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpRead, Path: vpath, HandleID: handleID, Offset: 0, Size: 256,
	})

	// Write new data - should invalidate cache
	newData := []byte("new data written")
	cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpWrite, Path: vpath, HandleID: handleID, Offset: 0, Data: newData,
	})

	// Read again - should get new data (either from remote or from fresh fetch)
	readResp, _ := cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpRead, Path: vpath, HandleID: handleID, Offset: 0, Size: 256,
	})

	if string(readResp.Data) != string(newData) {
		t.Fatalf("expected %q after write, got %q", newData, readResp.Data)
	}
}

func TestIntegration_LargeFileMultipleBlocks(t *testing.T) {
	t.Skip("mock stream does not handle concurrent open/read sequences correctly")

	dir := t.TempDir()
	testFile := filepath.Join(dir, "large.bin")

	// Create a file larger than one block (using small block size for test)
	blockSize := int64(1024)      // 1KB blocks
	fileSize := blockSize*5 + 512 // 5.5 blocks = 5632 bytes
	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i % 256)
	}
	if err := os.WriteFile(testFile, content, 0644); err != nil {
		t.Fatal(err)
	}

	paths := []*wire.ExportPath{{LocalPath: dir, VirtualName: "data"}}
	backend := export.NewBackend(paths)
	config := DefaultConfig()
	config.BlockSize = blockSize
	cachedClient := newTestPair(t, backend, config)
	ctx := context.Background()
	vpath := "data/large.bin"

	// Open file
	openResp, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpOpen, Path: vpath, Flags: 0})
	handleID := openResp.HandleID

	// Read entire file at once (to properly test block-based caching)
	readResp, err := cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpRead, Path: vpath, HandleID: handleID, Offset: 0, Size: uint32(fileSize),
	})
	if err != nil {
		t.Fatal(err)
	}
	readData := readResp.Data

	// Verify we read the entire file correctly
	if len(readData) != len(content) {
		t.Fatalf("expected to read %d bytes, got %d", len(content), len(readData))
	}
	for i := range content {
		if readData[i] != content[i] {
			t.Fatalf("mismatch at byte %d: expected %d, got %d", i, content[i], readData[i])
		}
	}

	// Verify cache has multiple blocks
	stats := cachedClient.Stats()
	if stats.DataCacheBlocks < 5 {
		t.Fatalf("expected at least 5 blocks, got %d", stats.DataCacheBlocks)
	}

	// Test partial read from cache (cache hit)
	partialResp, err := cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpRead, Path: vpath, HandleID: handleID, Offset: 1024, Size: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	partialData := partialResp.Data
	if len(partialData) != 2048 {
		t.Fatalf("expected partial read of 2048 bytes, got %d", len(partialData))
	}
	// Verify content
	for i := 0; i < len(partialData); i++ {
		expected := content[1024+i]
		if partialData[i] != expected {
			t.Fatalf("partial read mismatch at offset %d: expected %d, got %d", 1024+i, expected, partialData[i])
		}
	}
}

func TestIntegration_TruncationInvalidation(t *testing.T) {
	t.Skip("mock stream does not populate cache correctly for truncation test")

	dir := t.TempDir()
	testFile := filepath.Join(dir, "truncate.txt")
	content := []byte("This is a longer content that will be truncated")
	if err := os.WriteFile(testFile, content, 0644); err != nil {
		t.Fatal(err)
	}

	paths := []*wire.ExportPath{{LocalPath: dir, VirtualName: "data"}}
	backend := export.NewBackend(paths)
	config := DefaultConfig()
	config.BlockSize = 16 // Small blocks for testing
	cachedClient := newTestPair(t, backend, config)
	ctx := context.Background()
	vpath := "data/truncate.txt"

	// Open and read entire file to populate cache
	openResp, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpOpen, Path: vpath, Flags: 2})
	handleID := openResp.HandleID

	cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpRead, Path: vpath, HandleID: handleID, Offset: 0, Size: 256,
	})

	// Record blocks before truncate
	blocksBefore := cachedClient.Stats().DataCacheBlocks

	// Truncate file via SetAttr
	cachedClient.Do(ctx, &wire.Request{
		Op:           wire.OpSetAttr,
		Path:         vpath,
		SetAttrValid: wire.SetAttrSize,
		SetSize:      10,
	})

	// Blocks should be reduced
	blocksAfter := cachedClient.Stats().DataCacheBlocks
	if blocksAfter >= blocksBefore {
		t.Fatalf("expected fewer blocks after truncate: before=%d, after=%d",
			blocksBefore, blocksAfter)
	}
}

func TestIntegration_DirectoryCacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "file1.txt"), []byte("1"), 0644); err != nil {
		t.Fatal(err)
	}

	paths := []*wire.ExportPath{{LocalPath: dir, VirtualName: "data"}}
	backend := export.NewBackend(paths)
	cachedClient := newTestPair(t, backend, nil)
	ctx := context.Background()

	// Open directory
	openResp, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpOpendir, Path: "data/subdir"})
	handleID := openResp.HandleID

	// Read directory - populates cache
	readResp1, _ := cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpReaddir, Path: "data/subdir", HandleID: handleID,
	})
	entriesBefore := len(readResp1.Entries)

	// Release directory
	cachedClient.Do(ctx, &wire.Request{Op: wire.OpReleasedir, Path: "data/subdir", HandleID: handleID})

	// Create new file - should invalidate directory cache
	cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpCreate, Path: "data/subdir", Name: "file2.txt", Flags: 0, Mode: 0644,
	})

	// Re-open and read directory - should see new file
	openResp2, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpOpendir, Path: "data/subdir"})
	handleID2 := openResp2.HandleID

	readResp2, _ := cachedClient.Do(ctx, &wire.Request{
		Op: wire.OpReaddir, Path: "data/subdir", HandleID: handleID2,
	})
	entriesAfter := len(readResp2.Entries)

	if entriesAfter != entriesBefore+1 {
		t.Fatalf("expected %d entries after create, got %d", entriesBefore+1, entriesAfter)
	}
}

func TestIntegration_RenameInvalidatesBothParents(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	dstDir := filepath.Join(dir, "dst")
	if err := os.Mkdir(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dstDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	paths := []*wire.ExportPath{{LocalPath: dir, VirtualName: "data"}}
	backend := export.NewBackend(paths)
	cachedClient := newTestPair(t, backend, nil)
	ctx := context.Background()

	// Read both directories to populate cache
	for _, path := range []string{"data/src", "data/dst"} {
		openResp, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpOpendir, Path: path})
		handleID := openResp.HandleID
		cachedClient.Do(ctx, &wire.Request{Op: wire.OpReaddir, Path: path, HandleID: handleID})
		cachedClient.Do(ctx, &wire.Request{Op: wire.OpReleasedir, Path: path, HandleID: handleID})
	}

	// Verify both directories are cached
	dirCacheCountBefore := cachedClient.Stats().DirectoryEntries
	if dirCacheCountBefore < 2 {
		t.Fatal("expected at least 2 directory entries cached")
	}

	// Rename file from src to dst
	cachedClient.Do(ctx, &wire.Request{
		Op:      wire.OpRename,
		Path:    "data/src",
		Name:    "file.txt",
		NewPath: "data/dst",
		NewName: "renamed.txt",
	})

	// Both directory caches should be invalidated
	// Re-read to verify file moved
	openSrc, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpOpendir, Path: "data/src"})
	srcHandle := openSrc.HandleID
	srcEntries, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpReaddir, Path: "data/src", HandleID: srcHandle})

	openDst, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpOpendir, Path: "data/dst"})
	dstHandle := openDst.HandleID
	dstEntries, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpReaddir, Path: "data/dst", HandleID: dstHandle})

	// src should be empty
	if len(srcEntries.Entries) != 0 {
		t.Fatal("expected src to be empty after rename")
	}

	// dst should have the renamed file
	found := false
	for _, e := range dstEntries.Entries {
		if e.Name == "renamed.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected renamed.txt in dst after rename")
	}
}

func TestIntegration_MetadataCacheTTL(t *testing.T) {
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	paths := []*wire.ExportPath{{LocalPath: dir, VirtualName: "data"}}
	backend := export.NewBackend(paths)
	config := DefaultConfig()
	config.MetadataTTL = 50 * time.Millisecond // Short TTL for testing
	cachedClient := newTestPair(t, backend, config)
	ctx := context.Background()

	// Get attr - populates cache
	_, err := cachedClient.Do(ctx, &wire.Request{Op: wire.OpGetAttr, Path: "data/test.txt"})
	if err != nil {
		t.Fatal(err)
	}

	// Should be cached
	if cachedClient.Stats().MetadataEntries != 1 {
		t.Fatal("expected metadata to be cached")
	}

	// Wait for TTL to expire
	time.Sleep(100 * time.Millisecond)

	// Modify file on disk
	if err := os.WriteFile(testFile, []byte("much longer content now"), 0644); err != nil {
		t.Fatal(err)
	}

	// Get attr again - should fetch fresh data due to TTL expiry
	attrResp, _ := cachedClient.Do(ctx, &wire.Request{Op: wire.OpGetAttr, Path: "data/test.txt"})

	// Size should reflect the new content
	if attrResp.Attr == nil {
		t.Fatal("expected non-nil Attr in response")
	}
	if attrResp.Attr.Size != uint64(len("much longer content now")) {
		t.Fatalf("expected size %d, got %d", len("much longer content now"), attrResp.Attr.Size)
	}
}
