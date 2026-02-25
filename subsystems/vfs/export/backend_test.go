package export

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cyber-shuttle/linkspan/subsystems/vfs/wire"
)

func TestBackendReadWriteChecksum(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initial := []byte("initial content")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), initial, 0644); err != nil {
		t.Fatal(err)
	}
	paths := []*wire.ExportPath{{LocalPath: dir, VirtualName: "data"}}
	b := NewBackend(paths)
	vpath := "data/file.txt"

	// Open read-write so we can write later
	openResp := b.HandleRequest(ctx, &wire.Request{
		Op: wire.OpOpen, Path: vpath, Flags: 2, // O_RDWR
	})
	if openResp.Errno != 0 {
		t.Fatalf("open: errno=%d", openResp.Errno)
	}
	handleID := openResp.HandleID

	// Read and verify initial content
	readResp := b.HandleRequest(ctx, &wire.Request{
		Op: wire.OpRead, Path: vpath, HandleID: handleID, Offset: 0, Size: 256,
	})
	if readResp.Errno != 0 {
		t.Fatalf("read: errno=%d", readResp.Errno)
	}
	got := readResp.Data
	if string(got) != string(initial) {
		t.Fatalf("read content: got %q, want %q", got, initial)
	}

	// Write new content
	newContent := []byte("written from remote")
	writeResp := b.HandleRequest(ctx, &wire.Request{
		Op: wire.OpWrite, Path: vpath, HandleID: handleID, Offset: 0, Data: newContent,
	})
	if writeResp.Errno != 0 {
		t.Fatalf("write: errno=%d", writeResp.Errno)
	}
	if writeResp.Written != uint32(len(newContent)) {
		t.Fatalf("write: written=%d, want %d", writeResp.Written, len(newContent))
	}

	// Release and re-open to read back from disk
	b.HandleRequest(ctx, &wire.Request{
		Op: wire.OpRelease, Path: vpath, HandleID: handleID,
	})
	openResp2 := b.HandleRequest(ctx, &wire.Request{
		Op: wire.OpOpen, Path: vpath, Flags: 0, // read-only for final read
	})
	if openResp2.Errno != 0 {
		t.Fatalf("re-open: errno=%d", openResp2.Errno)
	}
	handleID2 := openResp2.HandleID
	readResp2 := b.HandleRequest(ctx, &wire.Request{
		Op: wire.OpRead, Path: vpath, HandleID: handleID2, Offset: 0, Size: 256,
	})
	if readResp2.Errno != 0 {
		t.Fatalf("read after write: errno=%d", readResp2.Errno)
	}
	got2 := readResp2.Data
	if string(got2) != string(newContent) {
		t.Fatalf("read after write: got %q, want %q", got2, newContent)
	}

	// Verify on disk
	disk, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(disk) != string(newContent) {
		t.Fatalf("file on disk: got %q, want %q", disk, newContent)
	}
}
