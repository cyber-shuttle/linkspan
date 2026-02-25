// Package export implements the backend that serves file operations over the wire protocol.
package export

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/subsystems/vfs/wire"
)

// Mode bits (Unix-style); avoid syscall.S_IF* for Windows portability.
const (
	_modeDir = 0o40000
	_modeReg = 0o100000
	_modeLnk = 0o120000
)

func errToErrno(e error) uint32 {
	if e == nil {
		return 0
	}
	if errno, ok := e.(syscall.Errno); ok {
		return uint32(errno)
	}
	if pe, ok := e.(*os.PathError); ok {
		if errno, ok := pe.Err.(syscall.Errno); ok {
			return uint32(errno)
		}
	}
	return uint32(syscall.EIO)
}

// Backend serves file operations from local paths, resolving virtual paths using export paths.
type Backend struct {
	paths      []*wire.ExportPath // virtual_name -> local_path (we resolve by virtual_name)
	handles    map[uint64]handleEntry
	nextHandle uint64
	inoMap     map[string]uint64 // virtual path -> inode (stable for Lookup/Readdir)
	nextIno    uint64
	mu         sync.Mutex
}

type handleEntry struct {
	path string
	f    *os.File
}

// NewBackend creates a backend for the given export paths.
func NewBackend(paths []*wire.ExportPath) *Backend {
	return &Backend{
		paths:      paths,
		handles:    make(map[uint64]handleEntry),
		nextHandle: 1,
		inoMap:     make(map[string]uint64),
		nextIno:    2, // 1 is root
	}
}

// Resolve virtual path (e.g. "data/foo") to local path. Returns empty string and errno if invalid.
func (b *Backend) Resolve(virtualPath string) (localPath string, errno uint32) {
	virtualPath = filepath.Clean(virtualPath)
	if virtualPath == "." || virtualPath == "/" {
		// Root: list virtual names only; no single local path.
		return "", 0
	}
	parts := strings.Split(strings.TrimPrefix(virtualPath, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", 0
	}
	first := parts[0]
	for _, p := range b.paths {
		if p.VirtualName == first {
			localBase := filepath.Clean(p.LocalPath)
			if len(parts) == 1 {
				return localBase, 0
			}
			rest := filepath.Join(parts[1:]...)
			localFull := filepath.Join(localBase, rest)
			// Ensure we don't escape localBase
			absLocal, _ := filepath.Abs(localFull)
			absBase, _ := filepath.Abs(localBase)
			if !strings.HasPrefix(absLocal, absBase+string(filepath.Separator)) && absLocal != absBase {
				return "", uint32(syscall.EACCES)
			}
			return localFull, 0
		}
	}
	return "", uint32(syscall.ENOENT)
}

// inoForPath returns a stable inode number for the given virtual path (e.g. "" for root, "data", "data/hello.txt").
func (b *Backend) inoForPath(virtualPath string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if virtualPath == "" || virtualPath == "." || virtualPath == "/" {
		return 1
	}
	if ino, ok := b.inoMap[virtualPath]; ok {
		return ino
	}
	ino := b.nextIno
	b.nextIno++
	b.inoMap[virtualPath] = ino
	return ino
}

// RootNames returns the virtual names at root (for ReadDir of root).
func (b *Backend) RootNames() []string {
	names := make([]string, 0, len(b.paths))
	for _, p := range b.paths {
		names = append(names, p.VirtualName)
	}
	return names
}

// HandleRequest handles a single wire Request and returns the response.
func (b *Backend) HandleRequest(ctx context.Context, req *wire.Request) *wire.Response {
	resp := &wire.Response{ID: req.ID, Op: req.Op}
	switch req.Op {
	case wire.OpGetAttr:
		b.handleGetAttr(req, resp)
	case wire.OpLookup:
		b.handleLookup(req, resp)
	case wire.OpOpen:
		b.handleOpen(req, resp)
	case wire.OpRead:
		b.handleRead(req, resp)
	case wire.OpWrite:
		b.handleWrite(req, resp)
	case wire.OpRelease:
		b.handleRelease(req, resp)
	case wire.OpOpendir:
		b.handleOpendir(req, resp)
	case wire.OpReaddir:
		b.handleReaddir(req, resp)
	case wire.OpReleasedir:
		b.handleReleasedir(req, resp)
	case wire.OpCreate:
		b.handleCreate(req, resp)
	case wire.OpMkdir:
		b.handleMkdir(req, resp)
	case wire.OpUnlink:
		b.handleUnlink(req, resp)
	case wire.OpRmdir:
		b.handleRmdir(req, resp)
	case wire.OpRename:
		b.handleRename(req, resp)
	case wire.OpSymlink:
		b.handleSymlink(req, resp)
	case wire.OpReadlink:
		b.handleReadlink(req, resp)
	case wire.OpSetAttr:
		b.handleSetAttr(req, resp)
	case wire.OpStatfs:
		b.handleStatfs(req, resp)
	case wire.OpFlush:
		b.handleFlush(req, resp)
	case wire.OpFsync:
		b.handleFsync(req, resp)
	default:
		resp.Errno = uint32(syscall.ENOSYS)
	}
	return resp
}

func statToAttr(info os.FileInfo, ino uint64) *wire.Attr {
	mode := uint32(info.Mode())
	if info.IsDir() {
		mode = _modeDir | (mode & 07777)
	} else if info.Mode()&os.ModeSymlink != 0 {
		mode = _modeLnk | (mode & 07777)
	} else {
		mode = _modeReg | (mode & 07777)
	}
	attr := &wire.Attr{
		Ino:     ino,
		Size:    uint64(info.Size()),
		Mode:    mode,
		Blksize: 4096,
		Blocks:  (uint64(info.Size()) + 4095) / 4096,
	}
	mt := info.ModTime().Unix()
	attr.Atime = uint64(mt)
	attr.Mtime = uint64(mt)
	attr.Ctime = uint64(mt)
	attr.Uid, attr.Gid = uidGidFromFileInfo(info)
	return attr
}

func (b *Backend) handleGetAttr(req *wire.Request, resp *wire.Response) {
	local, errno := b.Resolve(req.Path)
	if errno != 0 {
		if req.Path == "" || req.Path == "/" || req.Path == "." {
			// Root: return synthetic dir attr
			resp.Attr = &wire.Attr{Mode: _modeDir | 0755, Ino: 1}
			return
		}
		resp.Errno = errno
		return
	}
	if local == "" {
		// Root: return synthetic dir attr (do not os.Stat(""))
		resp.Attr = &wire.Attr{Mode: _modeDir | 0755, Ino: 1}
		return
	}
	info, err := os.Lstat(local)
	if err != nil {
		resp.Errno = errToErrno(err)
		return
	}
	ino := b.inoForPath(req.Path)
	resp.Attr = statToAttr(info, ino)
}

func (b *Backend) handleLookup(req *wire.Request, resp *wire.Response) {
	parentPath := req.Path
	childVirtualPath := req.Name
	if parentPath != "" && parentPath != "/" {
		childVirtualPath = parentPath + "/" + req.Name
	}
	if parentPath == "" || parentPath == "/" {
		// Lookup at root: find virtual name
		for _, p := range b.paths {
			if p.VirtualName == req.Name {
				info, err := os.Lstat(p.LocalPath)
				if err != nil {
					resp.Errno = errToErrno(err)
					return
				}
				ino := b.inoForPath(childVirtualPath)
				resp.Attr = statToAttr(info, ino)
				return
			}
		}
		resp.Errno = uint32(syscall.ENOENT)
		return
	}
	local, errno := b.Resolve(parentPath + "/" + req.Name)
	if errno != 0 {
		resp.Errno = errno
		return
	}
	info, err := os.Lstat(local)
	if err != nil {
		resp.Errno = errToErrno(err)
		return
	}
	ino := b.inoForPath(childVirtualPath)
	resp.Attr = statToAttr(info, ino)
}

func (b *Backend) handleOpen(req *wire.Request, resp *wire.Response) {
	local, errno := b.Resolve(req.Path)
	if errno != 0 {
		resp.Errno = errno
		return
	}
	flags := int(req.Flags)
	f, err := os.OpenFile(local, flags, 0)
	if err != nil {
		resp.Errno = errToErrno(err)
		return
	}
	b.mu.Lock()
	id := b.nextHandle
	b.nextHandle++
	b.handles[id] = handleEntry{path: req.Path, f: f}
	b.mu.Unlock()
	resp.HandleID = id
}

func (b *Backend) handleRead(req *wire.Request, resp *wire.Response) {
	b.mu.Lock()
	ent, ok := b.handles[req.HandleID]
	b.mu.Unlock()
	if !ok {
		resp.Errno = uint32(syscall.EBADF)
		return
	}
	buf := make([]byte, req.Size)
	n, err := ent.f.ReadAt(buf, req.Offset)
	if err != nil && err != io.EOF {
		resp.Errno = errToErrno(err)
		return
	}
	resp.Data = buf[:n]
}

func (b *Backend) handleWrite(req *wire.Request, resp *wire.Response) {
	b.mu.Lock()
	ent, ok := b.handles[req.HandleID]
	b.mu.Unlock()
	if !ok {
		resp.Errno = uint32(syscall.EBADF)
		return
	}
	n, err := ent.f.WriteAt(req.Data, req.Offset)
	if err != nil {
		resp.Errno = errToErrno(err)
		return
	}
	resp.Written = uint32(n)
}

func (b *Backend) handleRelease(req *wire.Request, resp *wire.Response) {
	b.mu.Lock()
	ent, ok := b.handles[req.HandleID]
	if ok {
		delete(b.handles, req.HandleID)
	}
	b.mu.Unlock()
	if ok {
		_ = ent.f.Close()
	}
}

func (b *Backend) handleOpendir(req *wire.Request, resp *wire.Response) {
	var local string
	var errno uint32
	if req.Path == "" || req.Path == "/" {
		// Root: we don't open a real dir; use a synthetic handle that Readdir will use
		local = ""
	} else {
		local, errno = b.Resolve(req.Path)
		if errno != 0 {
			resp.Errno = errno
			return
		}
	}
	var f *os.File
	if local != "" {
		var err error
		f, err = os.Open(local)
		if err != nil {
			resp.Errno = errToErrno(err)
			return
		}
	}
	b.mu.Lock()
	id := b.nextHandle
	b.nextHandle++
	b.handles[id] = handleEntry{path: req.Path, f: f}
	b.mu.Unlock()
	resp.HandleID = id
}

func (b *Backend) handleReaddir(req *wire.Request, resp *wire.Response) {
	b.mu.Lock()
	ent, ok := b.handles[req.HandleID]
	b.mu.Unlock()
	if !ok {
		resp.Errno = uint32(syscall.EBADF)
		return
	}
	var entries []wire.DirEntry
	if ent.f == nil {
		// Root handle: virtual names are top-level entries (dirs or files)
		for _, p := range b.paths {
			info, err := os.Stat(p.LocalPath)
			if err != nil {
				continue
			}
			var mode uint32
			if info.IsDir() {
				mode = _modeDir | 0755
			} else {
				mode = _modeReg | (uint32(info.Mode()) & 07777)
			}
			ino := b.inoForPath(p.VirtualName)
			entries = append(entries, wire.DirEntry{Name: p.VirtualName, Mode: mode, Ino: ino})
		}
	} else {
		names, err := ent.f.Readdirnames(-1)
		if err != nil {
			resp.Errno = errToErrno(err)
			return
		}
		dirPath := ent.f.Name() // path of the directory we opened (e.g. /export/data)
		for _, name := range names {
			info, err := os.Lstat(filepath.Join(dirPath, name))
			if err != nil {
				continue
			}
			mode := uint32(info.Mode())
			if info.IsDir() {
				mode = _modeDir | (mode & 07777)
			} else if info.Mode()&os.ModeSymlink != 0 {
				mode = _modeLnk | (mode & 07777)
			} else {
				mode = _modeReg | (mode & 07777)
			}
			childPath := ent.path + "/" + name
			if ent.path == "" {
				childPath = name
			}
			ino := b.inoForPath(childPath)
			entries = append(entries, wire.DirEntry{Name: name, Mode: mode, Ino: ino})
		}
	}
	resp.Entries = entries
}

func (b *Backend) handleReleasedir(req *wire.Request, resp *wire.Response) {
	b.mu.Lock()
	ent, ok := b.handles[req.HandleID]
	if ok {
		delete(b.handles, req.HandleID)
	}
	b.mu.Unlock()
	if ok && ent.f != nil {
		_ = ent.f.Close()
	}
}

func (b *Backend) handleCreate(req *wire.Request, resp *wire.Response) {
	local, errno := b.Resolve(req.Path + "/" + req.Name)
	if errno != 0 {
		resp.Errno = errno
		return
	}
	flags := int(req.Flags)
	mode := os.FileMode(req.Mode) & 07777
	f, err := os.OpenFile(local, flags|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		resp.Errno = errToErrno(err)
		return
	}
	info, _ := f.Stat()
	b.mu.Lock()
	id := b.nextHandle
	b.nextHandle++
	b.handles[id] = handleEntry{path: req.Path + "/" + req.Name, f: f}
	b.mu.Unlock()
	childPath := req.Path + "/" + req.Name
	if req.Path == "" {
		childPath = req.Name
	}
	ino := b.inoForPath(childPath)
	resp.HandleID = id
	resp.Attr = statToAttr(info, ino)
}

func (b *Backend) handleMkdir(req *wire.Request, resp *wire.Response) {
	local, errno := b.Resolve(req.Path + "/" + req.Name)
	if errno != 0 {
		resp.Errno = errno
		return
	}
	if err := os.Mkdir(local, os.FileMode(req.Mode)&07777); err != nil {
		resp.Errno = errToErrno(err)
		return
	}
	info, _ := os.Stat(local)
	childPath := req.Path + "/" + req.Name
	if req.Path == "" {
		childPath = req.Name
	}
	ino := b.inoForPath(childPath)
	resp.Attr = statToAttr(info, ino)
}

func (b *Backend) handleUnlink(req *wire.Request, resp *wire.Response) {
	local, errno := b.Resolve(req.Path + "/" + req.Name)
	if errno != 0 {
		resp.Errno = errno
		return
	}
	if err := os.Remove(local); err != nil {
		resp.Errno = errToErrno(err)
		return
	}
}

func (b *Backend) handleRmdir(req *wire.Request, resp *wire.Response) {
	local, errno := b.Resolve(req.Path + "/" + req.Name)
	if errno != 0 {
		resp.Errno = errno
		return
	}
	if err := os.Remove(local); err != nil {
		resp.Errno = errToErrno(err)
		return
	}
}

func (b *Backend) handleRename(req *wire.Request, resp *wire.Response) {
	oldLocal, errno := b.Resolve(req.Path + "/" + req.Name)
	if errno != 0 {
		resp.Errno = errno
		return
	}
	newLocal, errno := b.Resolve(req.NewPath + "/" + req.NewName)
	if errno != 0 {
		resp.Errno = errno
		return
	}
	if err := os.Rename(oldLocal, newLocal); err != nil {
		resp.Errno = errToErrno(err)
		return
	}
}

func (b *Backend) handleSymlink(req *wire.Request, resp *wire.Response) {
	local, errno := b.Resolve(req.Path + "/" + req.Name)
	if errno != 0 {
		resp.Errno = errno
		return
	}
	if err := os.Symlink(req.Target, local); err != nil {
		resp.Errno = errToErrno(err)
		return
	}
	info, _ := os.Lstat(local)
	childPath := req.Path + "/" + req.Name
	if req.Path == "" {
		childPath = req.Name
	}
	ino := b.inoForPath(childPath)
	resp.Attr = statToAttr(info, ino)
}

func (b *Backend) handleReadlink(req *wire.Request, resp *wire.Response) {
	local, errno := b.Resolve(req.Path)
	if errno != 0 {
		resp.Errno = errno
		return
	}
	target, err := os.Readlink(local)
	if err != nil {
		resp.Errno = errToErrno(err)
		return
	}
	resp.Target = target
}

func (b *Backend) handleSetAttr(req *wire.Request, resp *wire.Response) {
	local, errno := b.Resolve(req.Path)
	if errno != 0 {
		resp.Errno = errno
		return
	}
	if req.SetAttrValid&wire.SetAttrSize != 0 {
		if err := os.Truncate(local, int64(req.SetSize)); err != nil {
			resp.Errno = errToErrno(err)
			return
		}
	}
	if req.SetAttrValid&wire.SetAttrMtime != 0 {
		// best-effort
		_ = os.Chtimes(local, time.Unix(int64(req.SetMtime), 0), time.Unix(int64(req.SetMtime), 0))
	}
	info, err := os.Stat(local)
	if err != nil {
		resp.Errno = errToErrno(err)
		return
	}
	ino := b.inoForPath(req.Path)
	resp.Attr = statToAttr(info, ino)
}

func (b *Backend) handleStatfs(req *wire.Request, resp *wire.Response) {
	local, errno := b.Resolve(req.Path)
	if errno != 0 {
		if req.Path == "" || req.Path == "/" {
			local = b.paths[0].LocalPath
		} else {
			resp.Errno = errno
			return
		}
	}
	blocks, bfree, bavail, files, ffree, bsize, err := getStatfs(local)
	if err != nil {
		resp.Errno = errToErrno(err)
		return
	}
	resp.Blocks = blocks
	resp.Bfree = bfree
	resp.Bavail = bavail
	resp.Files = files
	resp.Ffree = ffree
	resp.Bsize = uint64(bsize)
	resp.Namelen = 255
}

func (b *Backend) handleFlush(req *wire.Request, resp *wire.Response) {
	b.mu.Lock()
	ent, ok := b.handles[req.HandleID]
	b.mu.Unlock()
	if ok && ent.f != nil {
		_ = ent.f.Sync()
	}
}

func (b *Backend) handleFsync(req *wire.Request, resp *wire.Response) {
	b.mu.Lock()
	ent, ok := b.handles[req.HandleID]
	b.mu.Unlock()
	if !ok {
		resp.Errno = uint32(syscall.EBADF)
		return
	}
	if ent.f != nil {
		if err := ent.f.Sync(); err != nil {
			resp.Errno = errToErrno(err)
			return
		}
	}
}
