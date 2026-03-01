package fuse

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	rpc "github.com/willscott/go-nfs-client/nfs/rpc"
)

// startNFSStack spins up the full NFS proxy stack backed by a temp directory:
//
//	FUSE TCP Server → Client → billy adapter → NFS Server → NFS Client
//
// It returns an NFS Target for making NFS calls, the backing temp directory,
// and a cleanup function.
func startNFSStack(t *testing.T) (target *nfsc.Target, dir string, cleanup func()) {
	t.Helper()

	// 1. Start a FUSE TCP server backed by a temp directory.
	dir = t.TempDir()
	srv := NewServer(dir)
	srvLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("FUSE server listen: %v", err)
	}
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		srv.Serve(srvLn) //nolint:errcheck
	}()

	// 2. Connect a FUSE TCP client.
	client, err := NewClient(srvLn.Addr().String())
	if err != nil {
		srv.Stop()
		<-srvDone
		t.Fatalf("NewClient: %v", err)
	}

	// 3. Start an NFS server backed by the FUSE client.
	nfsSrv := NewNFSServer(client)
	if err := nfsSrv.Start(); err != nil {
		client.Close()
		srv.Stop()
		<-srvDone
		t.Fatalf("NFS server start: %v", err)
	}

	// 4. Connect an NFS client to the NFS server.
	nfsAddr := fmt.Sprintf("127.0.0.1:%d", nfsSrv.Port())
	rpcClient, err := rpc.DialTCP("tcp", nfsAddr, false)
	if err != nil {
		nfsSrv.Stop()
		client.Close()
		srv.Stop()
		<-srvDone
		t.Fatalf("NFS client dial: %v", err)
	}

	var mounter nfsc.Mount
	mounter.Client = rpcClient
	target, err = mounter.Mount("/", rpc.AuthNull)
	if err != nil {
		rpcClient.Close()
		nfsSrv.Stop()
		client.Close()
		srv.Stop()
		<-srvDone
		t.Fatalf("NFS mount: %v", err)
	}

	cleanup = func() {
		mounter.Unmount() //nolint:errcheck
		rpcClient.Close()
		nfsSrv.Stop()
		client.Close()
		srv.Stop()
		<-srvDone
	}
	return target, dir, cleanup
}

// ---------------------------------------------------------------------------
// TestNFSReadExistingFile: read a file created on disk through the NFS stack.
// ---------------------------------------------------------------------------

func TestNFSReadExistingFile(t *testing.T) {
	target, dir, cleanup := startNFSStack(t)
	defer cleanup()

	content := []byte("Hello from the FUSE server!\n")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	f, err := target.Open("/hello.txt")
	if err != nil {
		t.Fatalf("NFS Open: %v", err)
	}
	defer f.Close()

	buf := make([]byte, len(content))
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("NFS Read: %v", err)
	}
	if n != len(content) {
		t.Errorf("Read %d bytes, want %d", n, len(content))
	}
	if !bytes.Equal(buf[:n], content) {
		t.Errorf("content mismatch:\ngot  %q\nwant %q", buf[:n], content)
	}
}

// ---------------------------------------------------------------------------
// TestNFSCreateAndWriteFile: create a file via NFS, write to it, read back.
// ---------------------------------------------------------------------------

func TestNFSCreateAndWriteFile(t *testing.T) {
	target, dir, cleanup := startNFSStack(t)
	defer cleanup()

	// Create file via NFS.
	_, err := target.Create("/newfile.txt", 0o644)
	if err != nil {
		t.Fatalf("NFS Create: %v", err)
	}

	// Write to it via NFS.
	data := []byte("written via NFS proxy")
	f, err := target.OpenFile("/newfile.txt", 0o644)
	if err != nil {
		t.Fatalf("NFS OpenFile: %v", err)
	}
	n, err := f.Write(data)
	if err != nil {
		t.Fatalf("NFS Write: %v", err)
	}
	if n != len(data) {
		t.Errorf("wrote %d bytes, want %d", n, len(data))
	}
	f.Close()

	// Read it back via NFS.
	f2, err := target.Open("/newfile.txt")
	if err != nil {
		t.Fatalf("NFS Open for read-back: %v", err)
	}
	defer f2.Close()

	buf := make([]byte, len(data))
	n, err = f2.Read(buf)
	if err != nil {
		t.Fatalf("NFS Read: %v", err)
	}
	if !bytes.Equal(buf[:n], data) {
		t.Errorf("read-back mismatch:\ngot  %q\nwant %q", buf[:n], data)
	}

	// Verify the file also exists on the underlying filesystem.
	diskData, err := os.ReadFile(filepath.Join(dir, "newfile.txt"))
	if err != nil {
		t.Fatalf("ReadFile from disk: %v", err)
	}
	if !bytes.Equal(diskData, data) {
		t.Errorf("disk content mismatch:\ngot  %q\nwant %q", diskData, data)
	}
}

// ---------------------------------------------------------------------------
// TestNFSReadDir: list directory entries via NFS.
// ---------------------------------------------------------------------------

func TestNFSReadDir(t *testing.T) {
	target, dir, cleanup := startNFSStack(t)
	defer cleanup()

	// Create some entries on disk.
	os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "beta.txt"), []byte("b"), 0o644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0o755)

	entries, err := target.ReadDirPlus("/")
	if err != nil {
		t.Fatalf("NFS ReadDirPlus: %v", err)
	}

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}

	for _, want := range []string{"alpha.txt", "beta.txt", "subdir"} {
		if !names[want] {
			t.Errorf("ReadDirPlus: missing expected entry %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// TestNFSMkdir: create a directory via NFS and verify it exists.
// ---------------------------------------------------------------------------

func TestNFSMkdir(t *testing.T) {
	target, dir, cleanup := startNFSStack(t)
	defer cleanup()

	_, err := target.Mkdir("/mydir", 0o755)
	if err != nil {
		t.Fatalf("NFS Mkdir: %v", err)
	}

	// Verify on disk.
	info, err := os.Stat(filepath.Join(dir, "mydir"))
	if err != nil {
		t.Fatalf("Stat on disk: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory on disk, got file")
	}

	// Verify via NFS lookup.
	fi, _, err := target.Lookup("/mydir")
	if err != nil {
		t.Fatalf("NFS Lookup: %v", err)
	}
	if !fi.IsDir() {
		t.Error("NFS Lookup: expected directory")
	}
}

// ---------------------------------------------------------------------------
// TestNFSRemoveFile: create a file, remove it via NFS, verify it's gone.
// ---------------------------------------------------------------------------

func TestNFSRemoveFile(t *testing.T) {
	target, dir, cleanup := startNFSStack(t)
	defer cleanup()

	os.WriteFile(filepath.Join(dir, "todelete.txt"), []byte("bye"), 0o644)

	if err := target.Remove("/todelete.txt"); err != nil {
		t.Fatalf("NFS Remove: %v", err)
	}

	// Verify on disk.
	if _, err := os.Stat(filepath.Join(dir, "todelete.txt")); !os.IsNotExist(err) {
		t.Error("file still exists on disk after NFS Remove")
	}

	// Verify via NFS.
	_, _, err := target.Lookup("/todelete.txt")
	if err == nil {
		t.Error("NFS Lookup succeeded for removed file")
	}
}

// ---------------------------------------------------------------------------
// TestNFSRename: rename a file via NFS and verify old is gone, new exists.
// ---------------------------------------------------------------------------

func TestNFSRename(t *testing.T) {
	target, dir, cleanup := startNFSStack(t)
	defer cleanup()

	os.WriteFile(filepath.Join(dir, "old.txt"), []byte("data"), 0o644)

	if err := target.Rename("/old.txt", "/new.txt"); err != nil {
		t.Fatalf("NFS Rename: %v", err)
	}

	// Old should be gone on disk.
	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
		t.Error("old file still exists on disk after rename")
	}

	// New should exist on disk.
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Errorf("new file not found on disk: %v", err)
	}

	// New should be accessible via NFS.
	_, _, err := target.Lookup("/new.txt")
	if err != nil {
		t.Errorf("NFS Lookup for new.txt failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestNFSNestedDir: read a file inside a nested directory.
// ---------------------------------------------------------------------------

func TestNFSNestedDir(t *testing.T) {
	target, dir, cleanup := startNFSStack(t)
	defer cleanup()

	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	content := []byte("nested content")
	os.WriteFile(filepath.Join(dir, "a", "b", "deep.txt"), content, 0o644)

	f, err := target.Open("/a/b/deep.txt")
	if err != nil {
		t.Fatalf("NFS Open nested: %v", err)
	}
	defer f.Close()

	buf := make([]byte, len(content))
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("NFS Read nested: %v", err)
	}
	if !bytes.Equal(buf[:n], content) {
		t.Errorf("nested read mismatch:\ngot  %q\nwant %q", buf[:n], content)
	}
}

// ---------------------------------------------------------------------------
// TestNFSWriteAndReadLargeFile: write and read back a larger payload to
// exercise chunked I/O.
// ---------------------------------------------------------------------------

func TestNFSWriteAndReadLargeFile(t *testing.T) {
	target, _, cleanup := startNFSStack(t)
	defer cleanup()

	// Generate ~128KB of data.
	data := make([]byte, 128*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	_, err := target.Create("/large.bin", 0o644)
	if err != nil {
		t.Fatalf("NFS Create: %v", err)
	}

	f, err := target.OpenFile("/large.bin", 0o644)
	if err != nil {
		t.Fatalf("NFS OpenFile: %v", err)
	}
	n, err := f.Write(data)
	if err != nil {
		t.Fatalf("NFS Write: %v", err)
	}
	if n != len(data) {
		t.Fatalf("wrote %d bytes, want %d", n, len(data))
	}
	f.Close()

	// Read back.
	f2, err := target.Open("/large.bin")
	if err != nil {
		t.Fatalf("NFS Open for read: %v", err)
	}
	defer f2.Close()

	got := make([]byte, len(data))
	total := 0
	for total < len(data) {
		nn, err := f2.Read(got[total:])
		total += nn
		if err != nil {
			break
		}
	}
	if total != len(data) {
		t.Fatalf("read %d bytes, want %d", total, len(data))
	}
	if !bytes.Equal(got, data) {
		t.Error("large file content mismatch")
	}
}

// ---------------------------------------------------------------------------
// TestNFSGetAttr: verify file attributes via NFS match disk.
// ---------------------------------------------------------------------------

func TestNFSGetAttr(t *testing.T) {
	target, dir, cleanup := startNFSStack(t)
	defer cleanup()

	content := []byte("attribute check")
	os.WriteFile(filepath.Join(dir, "attrs.txt"), content, 0o644)

	fi, _, err := target.Lookup("/attrs.txt")
	if err != nil {
		t.Fatalf("NFS Lookup: %v", err)
	}
	if fi.Size() != int64(len(content)) {
		t.Errorf("NFS size: got %d, want %d", fi.Size(), len(content))
	}
	if fi.IsDir() {
		t.Error("expected regular file, got directory")
	}
}

// ---------------------------------------------------------------------------
// TestNFSRmDir: create and remove a directory via NFS.
// ---------------------------------------------------------------------------

func TestNFSRmDir(t *testing.T) {
	target, dir, cleanup := startNFSStack(t)
	defer cleanup()

	_, err := target.Mkdir("/rmme", 0o755)
	if err != nil {
		t.Fatalf("NFS Mkdir: %v", err)
	}

	if err := target.RmDir("/rmme"); err != nil {
		t.Fatalf("NFS RmDir: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "rmme")); !os.IsNotExist(err) {
		t.Error("directory still exists on disk after NFS RmDir")
	}
}

// ---------------------------------------------------------------------------
// TestNFSManyFiles: create many files and verify ReadDirPlus returns all.
// ---------------------------------------------------------------------------

func TestNFSManyFiles(t *testing.T) {
	target, dir, cleanup := startNFSStack(t)
	defer cleanup()

	const count = 50
	expected := make([]string, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("file-%03d.txt", i)
		expected[i] = name
		os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644)
	}

	entries, err := target.ReadDirPlus("/")
	if err != nil {
		t.Fatalf("NFS ReadDirPlus: %v", err)
	}

	actual := make([]string, 0, len(entries))
	for _, e := range entries {
		actual = append(actual, e.Name())
	}

	sort.Strings(expected)
	sort.Strings(actual)

	for _, want := range expected {
		found := false
		for _, got := range actual {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing file in ReadDirPlus: %s", want)
		}
	}
}
