package fuse

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

// startClientServer starts a Server backed by a temporary directory, binds
// a listener on a random port, and returns a connected Client plus a cleanup
// function. The cleanup function closes the client and stops the server.
func startClientServer(t *testing.T) (client *Client, dir string, cleanup func()) {
	t.Helper()

	dir = t.TempDir()
	srv := NewServer(dir)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ln) //nolint:errcheck
	}()

	client, err = NewClient(ln.Addr().String())
	if err != nil {
		srv.Stop()
		<-done
		t.Fatalf("NewClient: %v", err)
	}

	cleanup = func() {
		client.Close()
		srv.Stop()
		<-done
	}
	return client, dir, cleanup
}

// ---------------------------------------------------------------------------
// TestClientReadDir: listing a directory with a file and a subdirectory.
// ---------------------------------------------------------------------------

func TestClientReadDir(t *testing.T) {
	c, dir, cleanup := startClientServer(t)
	defer cleanup()

	if err := os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "beta"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	entries, err := c.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name] = true
	}

	if !names["alpha.txt"] {
		t.Error("ReadDir: expected 'alpha.txt' in result")
	}
	if !names["beta"] {
		t.Error("ReadDir: expected 'beta' in result")
	}
}

// ---------------------------------------------------------------------------
// TestClientReadWriteRoundTrip: write then read back the same bytes.
// ---------------------------------------------------------------------------

func TestClientReadWriteRoundTrip(t *testing.T) {
	c, _, cleanup := startClientServer(t)
	defer cleanup()

	const filename = "rw_roundtrip.bin"
	want := []byte("Hello, TCP FUSE client!")

	if err := c.Create(filename, 0o644); err != nil {
		t.Fatalf("Create: %v", err)
	}

	n, err := c.Write(filename, 0, want)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(want) {
		t.Fatalf("Write: wrote %d bytes, want %d", n, len(want))
	}

	got, err := c.Read(filename, 0, uint32(len(want)))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Read data mismatch:\ngot  %q\nwant %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// TestClientGetAttrSize: GetAttr returns the correct file size.
// ---------------------------------------------------------------------------

func TestClientGetAttrSize(t *testing.T) {
	c, dir, cleanup := startClientServer(t)
	defer cleanup()

	content := []byte("size check content")
	if err := os.WriteFile(filepath.Join(dir, "sizefile.txt"), content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	attr, err := c.GetAttr("sizefile.txt")
	if err != nil {
		t.Fatalf("GetAttr: %v", err)
	}
	if attr.Size != int64(len(content)) {
		t.Errorf("GetAttr size: got %d, want %d", attr.Size, len(content))
	}
}

// ---------------------------------------------------------------------------
// TestClientCreateWriteRead: create a file via the client, write, read back.
// ---------------------------------------------------------------------------

func TestClientCreateWriteRead(t *testing.T) {
	c, _, cleanup := startClientServer(t)
	defer cleanup()

	const filename = "newfile.txt"
	data := []byte("created by client")

	if err := c.Create(filename, 0o644); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := c.Write(filename, 0, data); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := c.Read(filename, 0, uint32(len(data)))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("data mismatch:\ngot  %q\nwant %q", got, data)
	}

	// Verify size via GetAttr.
	attr, err := c.GetAttr(filename)
	if err != nil {
		t.Fatalf("GetAttr after write: %v", err)
	}
	if attr.Size != int64(len(data)) {
		t.Errorf("GetAttr size after write: got %d, want %d", attr.Size, len(data))
	}
}

// ---------------------------------------------------------------------------
// TestClientConnectNonexistent: dialing a server that is not listening returns
// an error.
// ---------------------------------------------------------------------------

func TestClientConnectNonexistent(t *testing.T) {
	// Port 1 is reserved and should not have a listening server; in practice
	// the OS will refuse the connection immediately.
	_, err := NewClient("127.0.0.1:1")
	if err == nil {
		t.Fatal("NewClient: expected error when connecting to nonexistent server, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestClientMkdir: create a directory and verify it is a dir via GetAttr.
// ---------------------------------------------------------------------------

func TestClientMkdir(t *testing.T) {
	c, _, cleanup := startClientServer(t)
	defer cleanup()

	if err := c.Mkdir("mydir", 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	attr, err := c.GetAttr("mydir")
	if err != nil {
		t.Fatalf("GetAttr: %v", err)
	}
	if attr.Mode&0o170000 != 0o040000 {
		t.Errorf("expected directory mode (S_IFDIR), got %o", attr.Mode)
	}
}

// ---------------------------------------------------------------------------
// TestClientUnlink: create a file then unlink it; subsequent GetAttr fails.
// ---------------------------------------------------------------------------

func TestClientUnlink(t *testing.T) {
	c, _, cleanup := startClientServer(t)
	defer cleanup()

	if err := c.Create("todelete.txt", 0o644); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := c.Unlink("todelete.txt"); err != nil {
		t.Fatalf("Unlink: %v", err)
	}

	_, err := c.GetAttr("todelete.txt")
	if err == nil {
		t.Fatal("GetAttr after Unlink: expected error, got nil")
	}
	var fuseErr *FuseError
	if fe, ok := err.(*FuseError); !ok || fe.Errno != uint32(ENOENT) {
		t.Errorf("expected FuseError with ENOENT, got %v", err)
	} else {
		_ = fuseErr
	}
}

// ---------------------------------------------------------------------------
// TestClientRename: rename a file and verify old is gone, new exists.
// ---------------------------------------------------------------------------

func TestClientRename(t *testing.T) {
	c, _, cleanup := startClientServer(t)
	defer cleanup()

	if err := c.Create("before.txt", 0o644); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := c.Rename("before.txt", "after.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if _, err := c.GetAttr("before.txt"); err == nil {
		t.Error("expected GetAttr on old name to fail after rename, got nil")
	}

	if _, err := c.GetAttr("after.txt"); err != nil {
		t.Errorf("GetAttr on new name failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestClientStatFS: StatFS returns sensible non-zero values.
// ---------------------------------------------------------------------------

func TestClientStatFS(t *testing.T) {
	c, _, cleanup := startClientServer(t)
	defer cleanup()

	info, err := c.StatFS()
	if err != nil {
		t.Fatalf("StatFS: %v", err)
	}
	if info.Bsize == 0 {
		t.Error("StatFS: Bsize should be non-zero")
	}
	if info.Blocks == 0 {
		t.Error("StatFS: Blocks should be non-zero")
	}
}

// ---------------------------------------------------------------------------
// TestClientGetAttrNotFound: GetAttr on a missing path returns FuseError ENOENT.
// ---------------------------------------------------------------------------

func TestClientGetAttrNotFound(t *testing.T) {
	c, _, cleanup := startClientServer(t)
	defer cleanup()

	_, err := c.GetAttr("does_not_exist.txt")
	if err == nil {
		t.Fatal("GetAttr: expected error for missing file, got nil")
	}
	fe, ok := err.(*FuseError)
	if !ok {
		t.Fatalf("expected *FuseError, got %T: %v", err, err)
	}
	if fe.Errno != uint32(ENOENT) {
		t.Errorf("expected ENOENT (%d), got errno %d", ENOENT, fe.Errno)
	}
}

// ---------------------------------------------------------------------------
// TestClientFuseErrorMessage: FuseError.Error() is non-empty.
// ---------------------------------------------------------------------------

func TestClientFuseErrorMessage(t *testing.T) {
	fe := &FuseError{Errno: uint32(ENOENT)}
	if fe.Error() == "" {
		t.Error("FuseError.Error() returned empty string")
	}
}
