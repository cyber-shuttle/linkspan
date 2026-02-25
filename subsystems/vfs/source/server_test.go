package source

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/cyber-shuttle/linkspan/subsystems/vfs/export"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/wire"
)

func TestHandleConnLookupOpenRead(t *testing.T) {
	// Create a temp directory with a test file.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	virtualName := "data"
	paths := []*wire.ExportPath{{LocalPath: dir, VirtualName: virtualName}}
	backend := export.NewBackend(paths)
	srv := NewServer(backend)

	// Start a TCP listener on a random port.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- Run(ctx, lis, srv)
	}()

	// Connect a client.
	rawConn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer rawConn.Close()
	c := wire.NewConn(rawConn)

	var nextID uint32 = 1
	send := func(req *wire.Request) *wire.Response {
		t.Helper()
		req.ID = nextID
		nextID++
		if err := c.SendRequest(req); err != nil {
			t.Fatalf("send request (op=0x%02x): %v", req.Op, err)
		}
		resp, err := c.RecvResponse()
		if err != nil {
			t.Fatalf("recv response (op=0x%02x): %v", req.Op, err)
		}
		return resp
	}

	// --- Lookup root -> "data" ---
	lookupResp := send(&wire.Request{
		Op:   wire.OpLookup,
		Path: "",
		Name: virtualName,
	})
	if lookupResp.Errno != 0 {
		t.Fatalf("lookup root/%s: errno=%d", virtualName, lookupResp.Errno)
	}
	if lookupResp.Attr == nil {
		t.Fatal("lookup root/data: expected non-nil Attr")
	}

	// --- Lookup "data" -> "hello.txt" ---
	lookupResp2 := send(&wire.Request{
		Op:   wire.OpLookup,
		Path: virtualName,
		Name: "hello.txt",
	})
	if lookupResp2.Errno != 0 {
		t.Fatalf("lookup %s/hello.txt: errno=%d", virtualName, lookupResp2.Errno)
	}
	if lookupResp2.Attr == nil {
		t.Fatal("lookup data/hello.txt: expected non-nil Attr")
	}

	// --- Open "data/hello.txt" ---
	openResp := send(&wire.Request{
		Op:    wire.OpOpen,
		Path:  virtualName + "/hello.txt",
		Flags: 0, // O_RDONLY
	})
	if openResp.Errno != 0 {
		t.Fatalf("open: errno=%d", openResp.Errno)
	}
	handleID := openResp.HandleID

	// --- Read from the open handle ---
	readResp := send(&wire.Request{
		Op:       wire.OpRead,
		Path:     virtualName + "/hello.txt",
		HandleID: handleID,
		Offset:   0,
		Size:     10,
	})
	if readResp.Errno != 0 {
		t.Fatalf("read: errno=%d", readResp.Errno)
	}
	if string(readResp.Data) != "hello" {
		t.Fatalf("read content: got %q, want %q", readResp.Data, "hello")
	}

	// --- Release the handle ---
	releaseResp := send(&wire.Request{
		Op:       wire.OpRelease,
		Path:     virtualName + "/hello.txt",
		HandleID: handleID,
	})
	if releaseResp.Errno != 0 {
		t.Fatalf("release: errno=%d", releaseResp.Errno)
	}

	// --- GetAttr on "data/hello.txt" ---
	getattrResp := send(&wire.Request{
		Op:   wire.OpGetAttr,
		Path: virtualName + "/hello.txt",
	})
	if getattrResp.Errno != 0 {
		t.Fatalf("getattr: errno=%d", getattrResp.Errno)
	}
	if getattrResp.Attr == nil {
		t.Fatal("getattr: expected non-nil Attr")
	}
	if getattrResp.Attr.Size != 5 {
		t.Fatalf("getattr size: got %d, want 5", getattrResp.Attr.Size)
	}
}

func TestRunContextStopsOnClosedChannel(t *testing.T) {
	dir := t.TempDir()
	paths := []*wire.ExportPath{{LocalPath: dir, VirtualName: "data"}}
	backend := export.NewBackend(paths)
	srv := NewServer(backend)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	stopCh := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- RunContext(stopCh, lis, srv)
	}()

	// Signal stop.
	close(stopCh)

	// RunContext must return after the listener is closed.
	if err := <-done; err == nil {
		// A nil error is acceptable — some platforms return nil on listener close.
		return
	}
}
