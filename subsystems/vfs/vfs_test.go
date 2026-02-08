package vfs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cyber-shuttle/linkspan/subsystems/vfs/cache"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/export"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/fileproto"
	pb "github.com/cyber-shuttle/linkspan/subsystems/vfs/proto/gen/remotefs"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/source"
)

// startTestServer starts a gRPC publish server with the given directory and returns
// the server address and a cleanup function.
func startTestServer(t *testing.T, dir string) (string, func()) {
	t.Helper()
	paths := []*pb.ExportPath{{LocalPath: dir, VirtualName: "data"}}
	backend := export.NewBackend(paths)
	srv := source.NewServer(backend)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterRemotefsCoordinatorServer(grpcServer, srv)
	go grpcServer.Serve(lis)

	cleanup := func() {
		grpcServer.GracefulStop()
		lis.Close()
	}
	return lis.Addr().String(), cleanup
}

// connectToServer creates a CachedClient connected to the test server.
func connectToServer(t *testing.T, addr string) (*cache.CachedClient, *fileproto.Client, *grpc.ClientConn) {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	client := pb.NewRemotefsCoordinatorClient(conn)
	stream, err := client.ConnectSink(context.Background())
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if err := stream.Send(&pb.FileMessage{
		Payload: &pb.FileMessage_ConnectSink{ConnectSink: &pb.ConnectSinkRequest{}},
	}); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	fpClient := fileproto.NewClient(stream)
	go fpClient.Run()
	time.Sleep(50 * time.Millisecond)

	cachedClient := cache.NewCachedClient(fpClient, cache.DefaultConfig())
	return cachedClient, fpClient, conn
}

// --- Integration tests: publish → connect → file operations ---

func TestPublishConnectReadFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello world"), 0644)

	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	cc, fpClient, conn := connectToServer(t, addr)
	defer cc.Close()
	defer fpClient.Close()
	defer conn.Close()

	ctx := context.Background()

	// Lookup the file
	resp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Lookup{Lookup: &pb.LookupRequest{Path: "data", Name: "test.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Errno != 0 {
		t.Fatalf("lookup: errno=%d", resp.Errno)
	}

	// Open file
	openResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Open{Open: &pb.OpenRequest{Path: "data/test.txt", Flags: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if openResp.Errno != 0 {
		t.Fatalf("open: errno=%d", openResp.Errno)
	}
	handleID := openResp.GetOpen().HandleId

	// Read file
	readResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Read{Read: &pb.ReadRequest{
			Path: "data/test.txt", HandleId: handleID, Offset: 0, Size: 100,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if readResp.Errno != 0 {
		t.Fatalf("read: errno=%d", readResp.Errno)
	}
	if string(readResp.GetRead().Data) != "hello world" {
		t.Fatalf("got %q, want %q", readResp.GetRead().Data, "hello world")
	}

	// Release
	cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Release{Release: &pb.ReleaseRequest{Path: "data/test.txt", HandleId: handleID}},
	})
}

func TestPublishConnectWriteFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "writable.txt"), []byte("original"), 0644)

	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	cc, fpClient, conn := connectToServer(t, addr)
	defer cc.Close()
	defer fpClient.Close()
	defer conn.Close()

	ctx := context.Background()

	// Open file for writing
	openResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Open{Open: &pb.OpenRequest{Path: "data/writable.txt", Flags: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if openResp.Errno != 0 {
		t.Fatalf("open: errno=%d", openResp.Errno)
	}
	handleID := openResp.GetOpen().HandleId

	// Write new data
	writeResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Write{Write: &pb.WriteRequest{
			Path: "data/writable.txt", HandleId: handleID, Offset: 0, Data: []byte("modified"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if writeResp.Errno != 0 {
		t.Fatalf("write: errno=%d", writeResp.Errno)
	}
	if writeResp.GetWrite().Written != 8 {
		t.Fatalf("written=%d, want 8", writeResp.GetWrite().Written)
	}

	// Flush and release
	cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Flush{Flush: &pb.FlushRequest{Path: "data/writable.txt", HandleId: handleID}},
	})
	cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Release{Release: &pb.ReleaseRequest{Path: "data/writable.txt", HandleId: handleID}},
	})

	// Verify on disk
	data, err := os.ReadFile(filepath.Join(dir, "writable.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "modified" {
		t.Fatalf("disk content: got %q, want %q", data, "modified")
	}
}

func TestPublishConnectListDirectory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0755)

	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	cc, fpClient, conn := connectToServer(t, addr)
	defer cc.Close()
	defer fpClient.Close()
	defer conn.Close()

	ctx := context.Background()

	// Open directory
	openResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Opendir{Opendir: &pb.OpendirRequest{Path: "data"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if openResp.Errno != 0 {
		t.Fatalf("opendir: errno=%d", openResp.Errno)
	}
	handleID := openResp.GetOpen().HandleId

	// Read directory
	readResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Readdir{Readdir: &pb.ReaddirRequest{Path: "data", HandleId: handleID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if readResp.Errno != 0 {
		t.Fatalf("readdir: errno=%d", readResp.Errno)
	}

	// Release
	cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Releasedir{Releasedir: &pb.ReleasedirRequest{Path: "data", HandleId: handleID}},
	})

	entries := readResp.GetReaddir().Entries
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["a.txt"] || !names["b.txt"] || !names["subdir"] {
		t.Fatalf("expected a.txt, b.txt, subdir in entries, got %v", names)
	}
}

func TestPublishConnectCreateAndDelete(t *testing.T) {
	dir := t.TempDir()

	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	cc, fpClient, conn := connectToServer(t, addr)
	defer cc.Close()
	defer fpClient.Close()
	defer conn.Close()

	ctx := context.Background()

	// Create a file
	createResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Create{Create: &pb.CreateRequest{
			Path: "data", Name: "new.txt", Flags: 0x41, Mode: 0644, // O_WRONLY|O_CREAT
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if createResp.Errno != 0 {
		t.Fatalf("create: errno=%d", createResp.Errno)
	}
	handleID := createResp.GetCreate().HandleId
	cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Release{Release: &pb.ReleaseRequest{Path: "data/new.txt", HandleId: handleID}},
	})

	// Verify file exists on disk
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatalf("file not created: %v", err)
	}

	// Delete the file
	unlinkResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Unlink{Unlink: &pb.UnlinkRequest{Path: "data", Name: "new.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if unlinkResp.Errno != 0 {
		t.Fatalf("unlink: errno=%d", unlinkResp.Errno)
	}

	// Verify file is deleted
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("file should be deleted")
	}
}

func TestPublishConnectMkdirAndRmdir(t *testing.T) {
	dir := t.TempDir()

	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	cc, fpClient, conn := connectToServer(t, addr)
	defer cc.Close()
	defer fpClient.Close()
	defer conn.Close()

	ctx := context.Background()

	// Mkdir
	mkdirResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Mkdir{Mkdir: &pb.MkdirRequest{Path: "data", Name: "newdir", Mode: 0755}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mkdirResp.Errno != 0 {
		t.Fatalf("mkdir: errno=%d", mkdirResp.Errno)
	}

	info, err := os.Stat(filepath.Join(dir, "newdir"))
	if err != nil {
		t.Fatalf("dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}

	// Rmdir
	rmdirResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Rmdir{Rmdir: &pb.RmdirRequest{Path: "data", Name: "newdir"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rmdirResp.Errno != 0 {
		t.Fatalf("rmdir: errno=%d", rmdirResp.Errno)
	}
	if _, err := os.Stat(filepath.Join(dir, "newdir")); !os.IsNotExist(err) {
		t.Fatal("dir should be deleted")
	}
}

func TestPublishConnectRename(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "old.txt"), []byte("content"), 0644)

	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	cc, fpClient, conn := connectToServer(t, addr)
	defer cc.Close()
	defer fpClient.Close()
	defer conn.Close()

	ctx := context.Background()

	renameResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Rename{Rename: &pb.RenameRequest{
			Path: "data", OldName: "old.txt", NewPath: "data", NewName: "new.txt",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if renameResp.Errno != 0 {
		t.Fatalf("rename: errno=%d", renameResp.Errno)
	}

	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
		t.Fatal("old file should not exist")
	}
	data, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Fatalf("got %q, want %q", data, "content")
	}
}

func TestCachingServesFromCache(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cached.txt"), []byte("cached data"), 0644)

	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	cc, fpClient, conn := connectToServer(t, addr)
	defer cc.Close()
	defer fpClient.Close()
	defer conn.Close()

	ctx := context.Background()

	// First GetAttr populates metadata cache
	resp1, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_GetAttr{GetAttr: &pb.GetAttrRequest{Path: "data/cached.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp1.Errno != 0 {
		t.Fatalf("getattr: errno=%d", resp1.Errno)
	}
	attr1 := resp1.GetGetAttr().Attr

	// Second GetAttr should come from cache (same result)
	resp2, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_GetAttr{GetAttr: &pb.GetAttrRequest{Path: "data/cached.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp2.Errno != 0 {
		t.Fatalf("getattr2: errno=%d", resp2.Errno)
	}
	attr2 := resp2.GetGetAttr().Attr

	if attr1.Size != attr2.Size || attr1.Mtime != attr2.Mtime {
		t.Fatal("cached attrs should match")
	}

	// Check stats show metadata was cached
	stats := cc.Stats()
	if stats.MetadataEntries == 0 {
		t.Fatal("expected metadata cache entries > 0")
	}
}

func TestCacheInvalidationOnWrite(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "inv.txt"), []byte("before"), 0644)

	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	cc, fpClient, conn := connectToServer(t, addr)
	defer cc.Close()
	defer fpClient.Close()
	defer conn.Close()

	ctx := context.Background()

	// Populate cache with a read
	cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_GetAttr{GetAttr: &pb.GetAttrRequest{Path: "data/inv.txt"}},
	})

	// Write should invalidate metadata cache
	openResp, _ := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Open{Open: &pb.OpenRequest{Path: "data/inv.txt", Flags: 2}},
	})
	handleID := openResp.GetOpen().HandleId
	cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Write{Write: &pb.WriteRequest{
			Path: "data/inv.txt", HandleId: handleID, Offset: 0, Data: []byte("after!"),
		}},
	})
	cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Flush{Flush: &pb.FlushRequest{Path: "data/inv.txt", HandleId: handleID}},
	})
	cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Release{Release: &pb.ReleaseRequest{Path: "data/inv.txt", HandleId: handleID}},
	})

	// Re-read should show new content (cache was invalidated by write)
	openResp2, _ := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Open{Open: &pb.OpenRequest{Path: "data/inv.txt", Flags: 0}},
	})
	handleID2 := openResp2.GetOpen().HandleId
	readResp, err := cc.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Read{Read: &pb.ReadRequest{
			Path: "data/inv.txt", HandleId: handleID2, Offset: 0, Size: 100,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if readResp.Errno != 0 {
		t.Fatalf("read: errno=%d", readResp.Errno)
	}
	if string(readResp.GetRead().Data) != "after!" {
		t.Fatalf("got %q, want %q", readResp.GetRead().Data, "after!")
	}
}

// --- Manager tests ---

func TestPublishManagerLifecycle(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello"), 0644)

	pm := &PublishManager{publishes: make(map[string]*PublishEntry)}
	result, err := pm.Publish(PublishConfig{
		Folder:     dir,
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID == "" {
		t.Fatal("expected non-empty ID")
	}

	list := pm.ListPublishes()
	if len(list) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(list))
	}
	if list[0].ID != result.ID {
		t.Fatalf("expected ID %s, got %s", result.ID, list[0].ID)
	}

	if err := pm.Stop(result.ID); err != nil {
		t.Fatal(err)
	}

	list = pm.ListPublishes()
	if len(list) != 0 {
		t.Fatalf("expected 0 publishes after stop, got %d", len(list))
	}

	if err := pm.Stop(result.ID); err != ErrPublishNotFound {
		t.Fatalf("expected ErrPublishNotFound, got %v", err)
	}
}

func TestConnectManagerLifecycle(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello"), 0644)

	// Start a publish server
	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	cm := &ConnectManager{connects: make(map[string]*ConnectEntry)}
	id, err := cm.Connect(ConnectConfig{ServerAddr: addr})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}

	ent, err := cm.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if ent.CachedClient == nil {
		t.Fatal("expected non-nil CachedClient")
	}

	list := cm.ListConnects()
	if len(list) != 1 {
		t.Fatalf("expected 1 connect, got %d", len(list))
	}

	// Verify we can do file operations through the session
	ctx := context.Background()
	resp, err := ent.CachedClient.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Lookup{Lookup: &pb.LookupRequest{Path: "data", Name: "test.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Errno != 0 {
		t.Fatalf("lookup through connect session: errno=%d", resp.Errno)
	}

	if err := cm.Disconnect(id); err != nil {
		t.Fatal(err)
	}

	list = cm.ListConnects()
	if len(list) != 0 {
		t.Fatalf("expected 0 connects after disconnect, got %d", len(list))
	}

	if err := cm.Disconnect(id); err != ErrConnectNotFound {
		t.Fatalf("expected ErrConnectNotFound, got %v", err)
	}
}

func TestConnectManagerDisconnectAll(t *testing.T) {
	dir := t.TempDir()
	addr, cleanup := startTestServer(t, dir)
	defer cleanup()

	cm := &ConnectManager{connects: make(map[string]*ConnectEntry)}
	_, err := cm.Connect(ConnectConfig{ServerAddr: addr})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cm.Connect(ConnectConfig{ServerAddr: addr})
	if err != nil {
		t.Fatal(err)
	}

	if len(cm.ListConnects()) != 2 {
		t.Fatal("expected 2 connects")
	}

	cm.DisconnectAll()

	if len(cm.ListConnects()) != 0 {
		t.Fatal("expected 0 connects after DisconnectAll")
	}
}

// --- REST API handler tests ---

func setupTestRouter(t *testing.T) (*mux.Router, string, func()) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello world"), 0644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0755)

	addr, cleanup := startTestServer(t, dir)

	// Create a fresh connect manager and set up a session
	oldCM := GlobalConnectManager
	newCM := &ConnectManager{connects: make(map[string]*ConnectEntry)}
	GlobalConnectManager = newCM

	id, err := newCM.Connect(ConnectConfig{ServerAddr: addr})
	if err != nil {
		GlobalConnectManager = oldCM
		cleanup()
		t.Fatal(err)
	}

	fullCleanup := func() {
		newCM.DisconnectAll()
		GlobalConnectManager = oldCM
		cleanup()
	}

	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/fs/connects", ListConnects).Methods("GET")
	api.HandleFunc("/fs/connect", CreateConnect).Methods("POST")
	api.HandleFunc("/fs/connect/{id}", DisconnectConnect).Methods("DELETE")
	api.HandleFunc("/fs/connect/{id}/stats", GetConnectStats).Methods("GET")
	api.HandleFunc("/fs/connect/{id}/list", ConnectListDir).Methods("GET")
	api.HandleFunc("/fs/connect/{id}/stat", ConnectStat).Methods("GET")
	api.HandleFunc("/fs/connect/{id}/read", ConnectReadFile).Methods("GET")
	api.HandleFunc("/fs/connect/{id}/write", ConnectWriteFile).Methods("POST")
	api.HandleFunc("/fs/connect/{id}/create", ConnectCreateFile).Methods("POST")
	api.HandleFunc("/fs/connect/{id}/mkdir", ConnectMkdir).Methods("POST")
	api.HandleFunc("/fs/connect/{id}/file", ConnectDeleteFile).Methods("DELETE")
	api.HandleFunc("/fs/connect/{id}/dir", ConnectDeleteDir).Methods("DELETE")
	api.HandleFunc("/fs/connect/{id}/rename", ConnectRename).Methods("POST")

	return r, id, fullCleanup
}

func TestAPIListConnects(t *testing.T) {
	r, _, cleanup := setupTestRouter(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/fs/connects", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var list []ConnectEntry
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("expected 1 connect, got %d", len(list))
	}
}

func TestAPIConnectStat(t *testing.T) {
	r, id, cleanup := setupTestRouter(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/fs/connect/"+id+"/stat?path=data/hello.txt", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var attr FileAttr
	json.Unmarshal(w.Body.Bytes(), &attr)
	if attr.Size != 11 {
		t.Fatalf("expected size 11, got %d", attr.Size)
	}
	if attr.IsDir {
		t.Fatal("expected not a directory")
	}
}

func TestAPIConnectListDir(t *testing.T) {
	r, id, cleanup := setupTestRouter(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/fs/connect/"+id+"/list?path=data", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var entries []DirEntryInfo
	json.Unmarshal(w.Body.Bytes(), &entries)
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["hello.txt"] {
		t.Fatal("expected hello.txt in entries")
	}
	if !names["subdir"] {
		t.Fatal("expected subdir in entries")
	}
}

func TestAPIConnectReadFile(t *testing.T) {
	r, id, cleanup := setupTestRouter(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/fs/connect/"+id+"/read?path=data/hello.txt&size=100", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &result)
	dataB64, ok := result["data"].(string)
	if !ok {
		t.Fatal("expected data field")
	}
	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatalf("got %q, want %q", data, "hello world")
	}
}

func TestAPIConnectWriteFile(t *testing.T) {
	r, id, cleanup := setupTestRouter(t)
	defer cleanup()

	body := `{"path":"data/hello.txt","offset":0,"data":"` + base64.StdEncoding.EncodeToString([]byte("updated!")) + `"}`
	req := httptest.NewRequest("POST", "/api/v1/fs/connect/"+id+"/write", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &result)
	if result["written"].(float64) != 8 {
		t.Fatalf("expected written=8, got %v", result["written"])
	}
}

func TestAPIConnectCreateFile(t *testing.T) {
	r, id, cleanup := setupTestRouter(t)
	defer cleanup()

	body := `{"path":"data","name":"created.txt","mode":420}`
	req := httptest.NewRequest("POST", "/api/v1/fs/connect/"+id+"/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAPIConnectMkdir(t *testing.T) {
	r, id, cleanup := setupTestRouter(t)
	defer cleanup()

	body := `{"path":"data","name":"newdir"}`
	req := httptest.NewRequest("POST", "/api/v1/fs/connect/"+id+"/mkdir", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAPIConnectDeleteFile(t *testing.T) {
	r, id, cleanup := setupTestRouter(t)
	defer cleanup()

	req := httptest.NewRequest("DELETE", "/api/v1/fs/connect/"+id+"/file?path=data&name=hello.txt", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAPIConnectRename(t *testing.T) {
	r, id, cleanup := setupTestRouter(t)
	defer cleanup()

	body := `{"path":"data","old_name":"hello.txt","new_path":"data","new_name":"renamed.txt"}`
	req := httptest.NewRequest("POST", "/api/v1/fs/connect/"+id+"/rename", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAPIConnectNotFound(t *testing.T) {
	r, _, cleanup := setupTestRouter(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/fs/connect/nonexistent/stat?path=foo", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestAPIGetConnectStats(t *testing.T) {
	r, id, cleanup := setupTestRouter(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/fs/connect/"+id+"/stats", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var stats cache.CacheStats
	json.Unmarshal(w.Body.Bytes(), &stats)
	// Stats should be valid (all zeros is fine for a fresh connection)
}
