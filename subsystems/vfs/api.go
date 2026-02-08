package vfs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"syscall"

	pb "github.com/cyber-shuttle/linkspan/subsystems/vfs/proto/gen/remotefs"
	utils "github.com/cyber-shuttle/linkspan/utils"
	"github.com/gorilla/mux"
)

// --- Connect session management ---

// ListConnects returns all active connect sessions.
func ListConnects(w http.ResponseWriter, r *http.Request) {
	list := GlobalConnectManager.ListConnects()
	utils.RespondJSON(w, http.StatusOK, list)
}

// CreateConnect establishes a gRPC connect session to a remote publish server.
func CreateConnect(w http.ResponseWriter, r *http.Request) {
	var cfg ConnectConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if cfg.ServerAddr == "" && (cfg.Token == "" || cfg.FRPConnection == "") {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "server_addr or (token and frp_connection) required"})
		return
	}
	id, err := GlobalConnectManager.Connect(cfg)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// DisconnectConnect closes a connect session by ID.
func DisconnectConnect(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return
	}
	err := GlobalConnectManager.Disconnect(id)
	if err != nil {
		if err == ErrConnectNotFound {
			utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusNoContent, nil)
}

// GetConnectStats returns cache statistics for a connect session.
func GetConnectStats(w http.ResponseWriter, r *http.Request) {
	ent, err := GlobalConnectManager.Get(mux.Vars(r)["id"])
	if err != nil {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	stats := ent.CachedClient.Stats()
	utils.RespondJSON(w, http.StatusOK, stats)
}

// --- File operations on connect sessions ---

// getConnectEntry is a helper that extracts the connect entry from the request.
func getConnectEntry(w http.ResponseWriter, r *http.Request) *ConnectEntry {
	id := mux.Vars(r)["id"]
	ent, err := GlobalConnectManager.Get(id)
	if err != nil {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return nil
	}
	return ent
}

// fileInfoFromAttr converts a proto Attr to a JSON-friendly struct.
type FileAttr struct {
	Size  uint64 `json:"size"`
	Mode  uint32 `json:"mode"`
	IsDir bool   `json:"is_dir"`
	Uid   uint32 `json:"uid"`
	Gid   uint32 `json:"gid"`
	Atime uint64 `json:"atime"`
	Mtime uint64 `json:"mtime"`
	Ctime uint64 `json:"ctime"`
	Ino   uint64 `json:"ino"`
}

func attrToFileAttr(a *pb.Attr) FileAttr {
	if a == nil {
		return FileAttr{}
	}
	return FileAttr{
		Size:  a.Size,
		Mode:  a.Mode,
		IsDir: a.Mode&syscall.S_IFDIR != 0,
		Uid:   a.Uid,
		Gid:   a.Gid,
		Atime: a.Atime,
		Mtime: a.Mtime,
		Ctime: a.Ctime,
		Ino:   a.Ino,
	}
}

// ConnectStat returns file attributes for a path.
// GET /fs/connect/{id}/stat?path=...
func ConnectStat(w http.ResponseWriter, r *http.Request) {
	ent := getConnectEntry(w, r)
	if ent == nil {
		return
	}
	path := r.URL.Query().Get("path")
	resp, err := ent.CachedClient.Do(context.Background(), &pb.FileRequest{
		Op: &pb.FileRequest_GetAttr{GetAttr: &pb.GetAttrRequest{Path: path}},
	})
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if resp.Errno != 0 {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": syscall.Errno(resp.Errno).Error()})
		return
	}
	ga := resp.GetGetAttr()
	if ga == nil || ga.Attr == nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": "no attr in response"})
		return
	}
	utils.RespondJSON(w, http.StatusOK, attrToFileAttr(ga.Attr))
}

// DirEntryInfo is a JSON-friendly directory entry.
type DirEntryInfo struct {
	Name  string `json:"name"`
	Mode  uint32 `json:"mode"`
	IsDir bool   `json:"is_dir"`
	Ino   uint64 `json:"ino"`
}

// ConnectListDir lists directory entries for a path.
// GET /fs/connect/{id}/list?path=...
func ConnectListDir(w http.ResponseWriter, r *http.Request) {
	ent := getConnectEntry(w, r)
	if ent == nil {
		return
	}
	path := r.URL.Query().Get("path")
	ctx := context.Background()

	// Open directory
	resp, err := ent.CachedClient.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Opendir{Opendir: &pb.OpendirRequest{Path: path}},
	})
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if resp.Errno != 0 {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": syscall.Errno(resp.Errno).Error()})
		return
	}
	openResp := resp.GetOpen()
	if openResp == nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": "no open result"})
		return
	}
	handleID := openResp.HandleId

	// Read directory entries
	resp2, err := ent.CachedClient.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Readdir{Readdir: &pb.ReaddirRequest{Path: path, HandleId: handleID}},
	})

	// Release directory handle
	_, _ = ent.CachedClient.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Releasedir{Releasedir: &pb.ReleasedirRequest{Path: path, HandleId: handleID}},
	})

	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if resp2.Errno != 0 {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": syscall.Errno(resp2.Errno).Error()})
		return
	}
	rd := resp2.GetReaddir()
	if rd == nil {
		utils.RespondJSON(w, http.StatusOK, []DirEntryInfo{})
		return
	}
	entries := make([]DirEntryInfo, 0, len(rd.Entries))
	for _, e := range rd.Entries {
		entries = append(entries, DirEntryInfo{
			Name:  e.Name,
			Mode:  e.Mode,
			IsDir: e.Mode&syscall.S_IFDIR != 0,
			Ino:   e.Ino,
		})
	}
	utils.RespondJSON(w, http.StatusOK, entries)
}

// ConnectReadFile reads file content. Returns base64-encoded data.
// GET /fs/connect/{id}/read?path=...&offset=0&size=4096
func ConnectReadFile(w http.ResponseWriter, r *http.Request) {
	ent := getConnectEntry(w, r)
	if ent == nil {
		return
	}
	path := r.URL.Query().Get("path")
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	size, _ := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
	if size <= 0 {
		size = 65536 // default 64KB
	}
	if size > 10*1024*1024 {
		size = 10 * 1024 * 1024 // cap at 10MB per request
	}
	ctx := context.Background()

	// Open file
	openResp, err := ent.CachedClient.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Open{Open: &pb.OpenRequest{Path: path, Flags: syscall.O_RDONLY}},
	})
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if openResp.Errno != 0 {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": syscall.Errno(openResp.Errno).Error()})
		return
	}
	or := openResp.GetOpen()
	if or == nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": "no open result"})
		return
	}
	handleID := or.HandleId

	// Read data
	readResp, err := ent.CachedClient.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Read{Read: &pb.ReadRequest{
			Path: path, HandleId: handleID, Offset: offset, Size: uint32(size),
		}},
	})

	// Release file handle
	_, _ = ent.CachedClient.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Release{Release: &pb.ReleaseRequest{Path: path, HandleId: handleID}},
	})

	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if readResp.Errno != 0 {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": syscall.Errno(readResp.Errno).Error()})
		return
	}
	rr := readResp.GetRead()
	data := []byte{}
	if rr != nil {
		data = rr.Data
	}
	utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
		"path":   path,
		"offset": offset,
		"size":   len(data),
		"data":   base64.StdEncoding.EncodeToString(data),
	})
}

// WriteFileRequest is the JSON body for ConnectWriteFile.
type WriteFileRequest struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Data   string `json:"data"` // base64-encoded
}

// ConnectWriteFile writes data to a file.
// POST /fs/connect/{id}/write
func ConnectWriteFile(w http.ResponseWriter, r *http.Request) {
	ent := getConnectEntry(w, r)
	if ent == nil {
		return
	}
	var req WriteFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Path == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "path required"})
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid base64 data"})
		return
	}
	ctx := context.Background()

	// Open file for writing (create if not exists)
	openResp, err := ent.CachedClient.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Open{Open: &pb.OpenRequest{Path: req.Path, Flags: syscall.O_WRONLY}},
	})
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if openResp.Errno != 0 {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": syscall.Errno(openResp.Errno).Error()})
		return
	}
	or := openResp.GetOpen()
	if or == nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": "no open result"})
		return
	}
	handleID := or.HandleId

	// Write data
	writeResp, err := ent.CachedClient.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Write{Write: &pb.WriteRequest{
			Path: req.Path, HandleId: handleID, Offset: req.Offset, Data: data,
		}},
	})

	// Flush and release
	_, _ = ent.CachedClient.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Flush{Flush: &pb.FlushRequest{Path: req.Path, HandleId: handleID}},
	})
	_, _ = ent.CachedClient.Do(ctx, &pb.FileRequest{
		Op: &pb.FileRequest_Release{Release: &pb.ReleaseRequest{Path: req.Path, HandleId: handleID}},
	})

	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if writeResp.Errno != 0 {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": syscall.Errno(writeResp.Errno).Error()})
		return
	}
	wr := writeResp.GetWrite()
	written := uint32(0)
	if wr != nil {
		written = wr.Written
	}
	utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
		"path":    req.Path,
		"written": written,
	})
}

// CreateFileRequest is the JSON body for ConnectCreateFile.
type CreateFileRequest struct {
	Path string `json:"path"` // parent directory path
	Name string `json:"name"` // file name to create
	Mode uint32 `json:"mode"` // file mode (default 0644)
}

// ConnectCreateFile creates a new file.
// POST /fs/connect/{id}/create
func ConnectCreateFile(w http.ResponseWriter, r *http.Request) {
	ent := getConnectEntry(w, r)
	if ent == nil {
		return
	}
	var req CreateFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	if req.Mode == 0 {
		req.Mode = 0644
	}
	resp, err := ent.CachedClient.Do(context.Background(), &pb.FileRequest{
		Op: &pb.FileRequest_Create{Create: &pb.CreateRequest{
			Path: req.Path, Name: req.Name, Flags: syscall.O_WRONLY | syscall.O_CREAT, Mode: req.Mode,
		}},
	})
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if resp.Errno != 0 {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": syscall.Errno(resp.Errno).Error()})
		return
	}
	cr := resp.GetCreate()
	if cr == nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": "no create result"})
		return
	}
	// Release the handle immediately since we're just creating
	_, _ = ent.CachedClient.Do(context.Background(), &pb.FileRequest{
		Op: &pb.FileRequest_Release{Release: &pb.ReleaseRequest{Path: req.Path + "/" + req.Name, HandleId: cr.HandleId}},
	})
	result := map[string]interface{}{"name": req.Name}
	if cr.Attr != nil {
		result["attr"] = attrToFileAttr(cr.Attr)
	}
	utils.RespondJSON(w, http.StatusCreated, result)
}

// MkdirRequest is the JSON body for ConnectMkdir.
type MkdirRequest struct {
	Path string `json:"path"` // parent directory path
	Name string `json:"name"` // directory name
	Mode uint32 `json:"mode"` // directory mode (default 0755)
}

// ConnectMkdir creates a new directory.
// POST /fs/connect/{id}/mkdir
func ConnectMkdir(w http.ResponseWriter, r *http.Request) {
	ent := getConnectEntry(w, r)
	if ent == nil {
		return
	}
	var req MkdirRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	if req.Mode == 0 {
		req.Mode = 0755
	}
	resp, err := ent.CachedClient.Do(context.Background(), &pb.FileRequest{
		Op: &pb.FileRequest_Mkdir{Mkdir: &pb.MkdirRequest{Path: req.Path, Name: req.Name, Mode: req.Mode}},
	})
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if resp.Errno != 0 {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": syscall.Errno(resp.Errno).Error()})
		return
	}
	mr := resp.GetMkdir()
	result := map[string]interface{}{"name": req.Name}
	if mr != nil && mr.Attr != nil {
		result["attr"] = attrToFileAttr(mr.Attr)
	}
	utils.RespondJSON(w, http.StatusCreated, result)
}

// ConnectDeleteFile deletes a file (unlink).
// DELETE /fs/connect/{id}/file?path=...&name=...
func ConnectDeleteFile(w http.ResponseWriter, r *http.Request) {
	ent := getConnectEntry(w, r)
	if ent == nil {
		return
	}
	dirPath := r.URL.Query().Get("path")
	name := r.URL.Query().Get("name")
	if name == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	resp, err := ent.CachedClient.Do(context.Background(), &pb.FileRequest{
		Op: &pb.FileRequest_Unlink{Unlink: &pb.UnlinkRequest{Path: dirPath, Name: name}},
	})
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if resp.Errno != 0 {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": syscall.Errno(resp.Errno).Error()})
		return
	}
	utils.RespondJSON(w, http.StatusNoContent, nil)
}

// ConnectDeleteDir deletes a directory (rmdir).
// DELETE /fs/connect/{id}/dir?path=...&name=...
func ConnectDeleteDir(w http.ResponseWriter, r *http.Request) {
	ent := getConnectEntry(w, r)
	if ent == nil {
		return
	}
	dirPath := r.URL.Query().Get("path")
	name := r.URL.Query().Get("name")
	if name == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	resp, err := ent.CachedClient.Do(context.Background(), &pb.FileRequest{
		Op: &pb.FileRequest_Rmdir{Rmdir: &pb.RmdirRequest{Path: dirPath, Name: name}},
	})
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if resp.Errno != 0 {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": syscall.Errno(resp.Errno).Error()})
		return
	}
	utils.RespondJSON(w, http.StatusNoContent, nil)
}

// RenameRequest is the JSON body for ConnectRename.
type RenameRequest struct {
	Path    string `json:"path"`     // old parent directory
	OldName string `json:"old_name"` // old file/dir name
	NewPath string `json:"new_path"` // new parent directory
	NewName string `json:"new_name"` // new file/dir name
}

// ConnectRename renames a file or directory.
// POST /fs/connect/{id}/rename
func ConnectRename(w http.ResponseWriter, r *http.Request) {
	ent := getConnectEntry(w, r)
	if ent == nil {
		return
	}
	var req RenameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.OldName == "" || req.NewName == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "old_name and new_name required"})
		return
	}
	resp, err := ent.CachedClient.Do(context.Background(), &pb.FileRequest{
		Op: &pb.FileRequest_Rename{Rename: &pb.RenameRequest{
			Path: req.Path, OldName: req.OldName, NewPath: req.NewPath, NewName: req.NewName,
		}},
	})
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if resp.Errno != 0 {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": syscall.Errno(resp.Errno).Error()})
		return
	}
	utils.RespondJSON(w, http.StatusOK, map[string]string{"result": "ok"})
}

// --- FUSE mount management ---

// ListMounts returns all active FUSE mounts.
func ListMounts(w http.ResponseWriter, r *http.Request) {
	list := GlobalMountManager.List()
	utils.RespondJSON(w, http.StatusOK, list)
}

// CreateMount mounts a remote FS at the given mountpoint (POST body: MountConfig JSON).
func CreateMount(w http.ResponseWriter, r *http.Request) {
	var cfg MountConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if cfg.Mountpoint == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "mountpoint required"})
		return
	}
	if cfg.ServerAddr == "" && (cfg.Token == "" || cfg.FRPConnection == "") {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "server_addr or (token and frp_connection) required"})
		return
	}
	if cfg.Token != "" && cfg.FRPConnection == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "frp_connection required when using token"})
		return
	}
	id, err := GlobalMountManager.Mount(cfg)
	if err != nil {
		if err == ErrUnsupported {
			utils.RespondJSON(w, http.StatusNotImplemented, map[string]string{"error": err.Error()})
			return
		}
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// UnmountMount unmounts by ID.
func UnmountMount(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return
	}
	err := GlobalMountManager.Unmount(id)
	if err != nil {
		if err == ErrMountNotFound {
			utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusNoContent, nil)
}

// --- Publish management ---

// ListPublishes returns all active gRPC publish servers.
func ListPublishes(w http.ResponseWriter, r *http.Request) {
	list := GlobalPublishManager.ListPublishes()
	utils.RespondJSON(w, http.StatusOK, list)
}

// CreatePublish starts publishing a folder (POST body: PublishConfig JSON).
func CreatePublish(w http.ResponseWriter, r *http.Request) {
	var cfg PublishConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if cfg.Folder == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "folder required"})
		return
	}
	result, err := GlobalPublishManager.Publish(cfg)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp := map[string]string{"id": result.ID}
	if result.Token != "" {
		resp["token"] = result.Token
	}
	utils.RespondJSON(w, http.StatusCreated, resp)
}

// StopPublish stops a publish server by ID.
func StopPublish(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return
	}
	err := GlobalPublishManager.Stop(id)
	if err != nil {
		if err == ErrPublishNotFound {
			utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusNoContent, nil)
}
