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

// serverConn starts a Server backed by a temporary directory, connects a
// client via net.Pipe, and returns the client connection along with a cleanup
// function. The server goroutine is stopped when cleanup is called.
func serverConn(t *testing.T) (clientConn net.Conn, srv *Server, dir string, cleanup func()) {
	t.Helper()

	dir = t.TempDir()
	srv = NewServer(dir)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ln) //nolint:errcheck
	}()

	clientConn, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatalf("net.Dial: %v", err)
	}

	cleanup = func() {
		clientConn.Close()
		srv.Stop()
		<-done
	}
	return clientConn, srv, dir, cleanup
}

// roundTrip sends req over conn and returns the decoded Response.
func roundTrip(t *testing.T, conn net.Conn, req *Request) *Response {
	t.Helper()
	if err := WriteRequest(conn, req); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	resp, err := ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	return resp
}

// requireOK asserts that resp.Status is StatusOK.
func requireOK(t *testing.T, resp *Response) {
	t.Helper()
	if resp.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %d", resp.Status)
	}
}

// requireStatus asserts that resp.Status matches want.
func requireStatus(t *testing.T, resp *Response, want Status) {
	t.Helper()
	if resp.Status != want {
		t.Fatalf("expected status %d, got %d", want, resp.Status)
	}
}

// ---------------------------------------------------------------------------
// TestServerGetAttr: GetAttr on an existing file returns the correct size.
// ---------------------------------------------------------------------------

func TestServerGetAttr(t *testing.T) {
	conn, _, dir, cleanup := serverConn(t)
	defer cleanup()

	content := []byte("hello, fuse server")
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	resp := roundTrip(t, conn, &Request{
		ReqID:   1,
		Op:      OpGetAttr,
		Payload: EncodePathPayload("hello.txt"),
	})
	requireOK(t, resp)

	attr, err := DecodeAttrInfo(resp.Payload)
	if err != nil {
		t.Fatalf("DecodeAttrInfo: %v", err)
	}
	if attr.Size != int64(len(content)) {
		t.Errorf("size: got %d, want %d", attr.Size, len(content))
	}
}

// ---------------------------------------------------------------------------
// TestServerReadDir: ReadDir returns both files and subdirectories.
// ---------------------------------------------------------------------------

func TestServerReadDir(t *testing.T) {
	conn, _, dir, cleanup := serverConn(t)
	defer cleanup()

	// Create a file and a subdirectory inside the temp root.
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	resp := roundTrip(t, conn, &Request{
		ReqID:   2,
		Op:      OpReadDir,
		Payload: EncodePathPayload("."),
	})
	requireOK(t, resp)

	entries, err := DecodeDirEntries(resp.Payload)
	if err != nil {
		t.Fatalf("DecodeDirEntries: %v", err)
	}

	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name] = true
	}

	if !names["file.txt"] {
		t.Error("ReadDir: expected 'file.txt' in result")
	}
	if !names["subdir"] {
		t.Error("ReadDir: expected 'subdir' in result")
	}
}

// ---------------------------------------------------------------------------
// TestServerReadWriteRoundTrip: create, write, read back, verify data.
// ---------------------------------------------------------------------------

func TestServerReadWriteRoundTrip(t *testing.T) {
	conn, _, _, cleanup := serverConn(t)
	defer cleanup()

	const filename = "roundtrip.bin"
	data := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ")

	// Create the file.
	createResp := roundTrip(t, conn, &Request{
		ReqID:   10,
		Op:      OpCreate,
		Payload: EncodeCreateReq(CreateReq{Path: filename, Mode: 0o644, Flags: 0}),
	})
	requireOK(t, createResp)

	// Write data at offset 0.
	writeResp := roundTrip(t, conn, &Request{
		ReqID:   11,
		Op:      OpWrite,
		Payload: EncodeWriteReq(WriteReq{Path: filename, Offset: 0, Data: data}),
	})
	requireOK(t, writeResp)

	// Read the data back.
	readResp := roundTrip(t, conn, &Request{
		ReqID: 12,
		Op:    OpRead,
		Payload: EncodeReadReq(ReadReq{
			Path:   filename,
			Offset: 0,
			Size:   uint32(len(data)),
		}),
	})
	requireOK(t, readResp)

	if !bytes.Equal(readResp.Payload, data) {
		t.Errorf("read data mismatch:\ngot  %q\nwant %q", readResp.Payload, data)
	}
}

// ---------------------------------------------------------------------------
// TestServerPathTraversalRejected: "../../../etc/passwd" must be rejected.
// ---------------------------------------------------------------------------

func TestServerPathTraversalRejected(t *testing.T) {
	conn, _, _, cleanup := serverConn(t)
	defer cleanup()

	traversalPaths := []string{
		"../../../etc/passwd",
		"../../etc/shadow",
		"subdir/../../../../../../etc/passwd",
	}

	for i, p := range traversalPaths {
		resp := roundTrip(t, conn, &Request{
			ReqID:   uint32(100 + i),
			Op:      OpGetAttr,
			Payload: EncodePathPayload(p),
		})
		if resp.Status == StatusOK {
			t.Errorf("traversal path %q: expected non-OK status, got StatusOK", p)
		}
	}
}

// ---------------------------------------------------------------------------
// TestServerGetAttrNotFound: GetAttr on nonexistent file returns ENOENT.
// ---------------------------------------------------------------------------

func TestServerGetAttrNotFound(t *testing.T) {
	conn, _, _, cleanup := serverConn(t)
	defer cleanup()

	resp := roundTrip(t, conn, &Request{
		ReqID:   20,
		Op:      OpGetAttr,
		Payload: EncodePathPayload("does_not_exist.txt"),
	})
	requireStatus(t, resp, ENOENT)
}

// ---------------------------------------------------------------------------
// TestServerMkdir: create a directory and verify it via GetAttr.
// ---------------------------------------------------------------------------

func TestServerMkdir(t *testing.T) {
	conn, _, _, cleanup := serverConn(t)
	defer cleanup()

	mkdirResp := roundTrip(t, conn, &Request{
		ReqID:   30,
		Op:      OpMkdir,
		Payload: EncodeMkdirReq(MkdirReq{Path: "newdir", Mode: 0o755}),
	})
	requireOK(t, mkdirResp)

	// Verify the directory exists via GetAttr.
	attrResp := roundTrip(t, conn, &Request{
		ReqID:   31,
		Op:      OpGetAttr,
		Payload: EncodePathPayload("newdir"),
	})
	requireOK(t, attrResp)

	attr, err := DecodeAttrInfo(attrResp.Payload)
	if err != nil {
		t.Fatalf("DecodeAttrInfo: %v", err)
	}
	if attr.Mode&0o170000 != 0o040000 {
		t.Errorf("expected directory mode (S_IFDIR), got %o", attr.Mode)
	}
}

// ---------------------------------------------------------------------------
// TestServerUnlink: create a file, unlink it, verify it is gone.
// ---------------------------------------------------------------------------

func TestServerUnlink(t *testing.T) {
	conn, _, dir, cleanup := serverConn(t)
	defer cleanup()

	// Create a file on disk directly (bypassing the server) to keep the test
	// focused on the Unlink operation.
	if err := os.WriteFile(filepath.Join(dir, "todelete.txt"), []byte("bye"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	unlinkResp := roundTrip(t, conn, &Request{
		ReqID:   40,
		Op:      OpUnlink,
		Payload: EncodePathPayload("todelete.txt"),
	})
	requireOK(t, unlinkResp)

	// Confirm the file no longer exists.
	attrResp := roundTrip(t, conn, &Request{
		ReqID:   41,
		Op:      OpGetAttr,
		Payload: EncodePathPayload("todelete.txt"),
	})
	requireStatus(t, attrResp, ENOENT)
}

// ---------------------------------------------------------------------------
// TestServerUnknownOpcode: unknown opcode returns EINVAL.
// ---------------------------------------------------------------------------

func TestServerUnknownOpcode(t *testing.T) {
	conn, _, _, cleanup := serverConn(t)
	defer cleanup()

	resp := roundTrip(t, conn, &Request{
		ReqID:   50,
		Op:      Opcode(0xFF),
		Payload: nil,
	})
	requireStatus(t, resp, EINVAL)
}

// ---------------------------------------------------------------------------
// TestServerStatFS: StatFS returns hardcoded reasonable values.
// ---------------------------------------------------------------------------

func TestServerStatFS(t *testing.T) {
	conn, _, _, cleanup := serverConn(t)
	defer cleanup()

	resp := roundTrip(t, conn, &Request{
		ReqID:   60,
		Op:      OpStatFS,
		Payload: nil,
	})
	requireOK(t, resp)

	info, err := DecodeStatFSInfo(resp.Payload)
	if err != nil {
		t.Fatalf("DecodeStatFSInfo: %v", err)
	}
	if info.Bsize == 0 {
		t.Error("StatFS: Bsize should be non-zero")
	}
	if info.Blocks == 0 {
		t.Error("StatFS: Blocks should be non-zero")
	}
}

// ---------------------------------------------------------------------------
// TestServerRename: rename a file and verify new name exists.
// ---------------------------------------------------------------------------

func TestServerRename(t *testing.T) {
	conn, _, dir, cleanup := serverConn(t)
	defer cleanup()

	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	renameResp := roundTrip(t, conn, &Request{
		ReqID:   70,
		Op:      OpRename,
		Payload: EncodeRenameReq(RenameReq{OldPath: "old.txt", NewPath: "new.txt"}),
	})
	requireOK(t, renameResp)

	// Old path should be gone.
	oldResp := roundTrip(t, conn, &Request{
		ReqID:   71,
		Op:      OpGetAttr,
		Payload: EncodePathPayload("old.txt"),
	})
	requireStatus(t, oldResp, ENOENT)

	// New path should exist.
	newResp := roundTrip(t, conn, &Request{
		ReqID:   72,
		Op:      OpGetAttr,
		Payload: EncodePathPayload("new.txt"),
	})
	requireOK(t, newResp)
}

// ---------------------------------------------------------------------------
// TestServerMultipleConnections: two concurrent connections are handled independently.
// ---------------------------------------------------------------------------

func TestServerMultipleConnections(t *testing.T) {
	srv := NewServer(t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ln) //nolint:errcheck
	}()
	defer func() {
		srv.Stop()
		<-done
	}()

	connect := func() net.Conn {
		c, dialErr := net.Dial("tcp", ln.Addr().String())
		if dialErr != nil {
			t.Fatalf("Dial: %v", dialErr)
		}
		return c
	}

	c1, c2 := connect(), connect()
	defer c1.Close()
	defer c2.Close()

	// Both connections should get ENOENT for a missing file.
	resp1 := roundTrip(t, c1, &Request{ReqID: 1, Op: OpGetAttr, Payload: EncodePathPayload("nope.txt")})
	resp2 := roundTrip(t, c2, &Request{ReqID: 2, Op: OpGetAttr, Payload: EncodePathPayload("nope.txt")})

	requireStatus(t, resp1, ENOENT)
	requireStatus(t, resp2, ENOENT)

	if resp1.ReqID != 1 {
		t.Errorf("c1: expected ReqID 1, got %d", resp1.ReqID)
	}
	if resp2.ReqID != 2 {
		t.Errorf("c2: expected ReqID 2, got %d", resp2.ReqID)
	}
}
