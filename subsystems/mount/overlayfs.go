package mount

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// OverlayFS is a userspace overlay FUSE filesystem.
// Reads check upper (local cache) first, then fall back to lower (SFTP).
// Writes always go to upper.
type OverlayFS struct {
	server   *fuse.Server
	sshConn  *ssh.Client
	sftpConn *sftp.Client
	mountDir string
	mu       sync.Mutex
}

// MountOverlayFS creates a single FUSE mount at mountDir that overlays
// upperDir (local cache) on top of the remote SFTP workspace (lower).
func MountOverlayFS(sshAddr, remoteRoot, upperDir, mountDir string) (*OverlayFS, error) {
	sshConfig := &ssh.ClientConfig{
		User:            "linkspan",
		Auth:            []ssh.AuthMethod{},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	sshConn, err := ssh.Dial("tcp", sshAddr, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", sshAddr, err)
	}

	sftpConn, err := sftp.NewClient(sshConn)
	if err != nil {
		sshConn.Close()
		return nil, fmt.Errorf("sftp client: %w", err)
	}

	root := &overlayNode{
		sftp:       sftpConn,
		remotePath: remoteRoot,
		upperPath:  upperDir,
	}

	server, err := fs.Mount(mountDir, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			AllowOther: false,
			FsName:     "linkspan-overlay",
			Name:       "overlayfs",
		},
	})
	if err != nil {
		sftpConn.Close()
		sshConn.Close()
		return nil, fmt.Errorf("fuse mount %s: %w", mountDir, err)
	}

	log.Printf("[overlayfs] mounted at %s (upper=%s, lower=%s:%s)", mountDir, upperDir, sshAddr, remoteRoot)

	return &OverlayFS{
		server:   server,
		sshConn:  sshConn,
		sftpConn: sftpConn,
		mountDir: mountDir,
	}, nil
}

// Unmount stops the FUSE server and closes connections.
func (o *OverlayFS) Unmount() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.server != nil {
		if err := o.server.Unmount(); err != nil {
			log.Printf("[overlayfs] unmount warning: %v", err)
		}
		o.server = nil
	}
	if o.sftpConn != nil {
		o.sftpConn.Close()
		o.sftpConn = nil
	}
	if o.sshConn != nil {
		o.sshConn.Close()
		o.sshConn = nil
	}
}

// overlayNode implements a FUSE node that overlays a local upper dir on an SFTP lower dir.
type overlayNode struct {
	fs.Inode
	sftp       *sftp.Client
	remotePath string // path on the SFTP server (lower)
	upperPath  string // path on the local filesystem (upper/cache)
}

var (
	_ fs.InodeEmbedder  = (*overlayNode)(nil)
	_ fs.NodeLookuper   = (*overlayNode)(nil)
	_ fs.NodeReaddirer  = (*overlayNode)(nil)
	_ fs.NodeGetattrer  = (*overlayNode)(nil)
	_ fs.NodeOpener     = (*overlayNode)(nil)
	_ fs.NodeCreater    = (*overlayNode)(nil)
	_ fs.NodeMkdirer    = (*overlayNode)(nil)
	_ fs.NodeUnlinker   = (*overlayNode)(nil)
	_ fs.NodeRmdirer    = (*overlayNode)(nil)
	_ fs.NodeRenamer    = (*overlayNode)(nil)
	_ fs.NodeSetattrer  = (*overlayNode)(nil)
	_ fs.NodeReadlinker = (*overlayNode)(nil)
	_ fs.NodeSymlinker  = (*overlayNode)(nil)
)

// source describes where a file/dir lives.
type source int

const (
	srcNone  source = iota
	srcUpper        // exists in local cache
	srcLower        // exists on SFTP only
	srcBoth         // exists in both (upper wins)
)

func (n *overlayNode) childUpperPath(name string) string {
	return filepath.Join(n.upperPath, name)
}

func (n *overlayNode) childRemotePath(name string) string {
	return filepath.Join(n.remotePath, name)
}

func (n *overlayNode) child(name string) *overlayNode {
	return &overlayNode{
		sftp:       n.sftp,
		remotePath: n.childRemotePath(name),
		upperPath:  n.childUpperPath(name),
	}
}

// locate determines where a path exists (upper, lower, both, or none).
func (n *overlayNode) locate() source {
	_, upperErr := os.Lstat(n.upperPath)
	_, lowerErr := n.sftp.Lstat(n.remotePath)
	upperOK := upperErr == nil
	lowerOK := lowerErr == nil
	switch {
	case upperOK && lowerOK:
		return srcBoth
	case upperOK:
		return srcUpper
	case lowerOK:
		return srcLower
	default:
		return srcNone
	}
}

// locateChild determines where a named child exists.
func (n *overlayNode) locateChild(name string) source {
	_, upperErr := os.Lstat(n.childUpperPath(name))
	_, lowerErr := n.sftp.Lstat(n.childRemotePath(name))
	upperOK := upperErr == nil
	lowerOK := lowerErr == nil
	switch {
	case upperOK && lowerOK:
		return srcBoth
	case upperOK:
		return srcUpper
	case lowerOK:
		return srcLower
	default:
		return srcNone
	}
}

func localAttrToFuse(info os.FileInfo, out *fuse.Attr) {
	out.Mode = uint32(info.Mode())
	out.Size = uint64(info.Size())
	out.Mtime = uint64(info.ModTime().Unix())
	fillStatFields(info, out)
}

func sftpAttrToFuse(info os.FileInfo, out *fuse.Attr) {
	s := info.Sys()
	if stat, ok := s.(*sftp.FileStat); ok {
		out.Mode = uint32(info.Mode())
		out.Size = uint64(info.Size())
		out.Mtime = uint64(stat.Mtime)
		out.Atime = uint64(stat.Atime)
		out.Uid = stat.UID
		out.Gid = stat.GID
	} else {
		out.Mode = uint32(info.Mode())
		out.Size = uint64(info.Size())
		out.Mtime = uint64(info.ModTime().Unix())
		out.Atime = out.Mtime
	}
}

func (n *overlayNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	// Upper wins
	if info, err := os.Lstat(n.upperPath); err == nil {
		localAttrToFuse(info, &out.Attr)
		return fs.OK
	}
	if info, err := n.sftp.Lstat(n.remotePath); err == nil {
		sftpAttrToFuse(info, &out.Attr)
		return fs.OK
	}
	return syscall.ENOENT
}

func (n *overlayNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	child := n.child(name)
	up := n.childUpperPath(name)
	rp := n.childRemotePath(name)

	// Check upper first
	if info, err := os.Lstat(up); err == nil {
		localAttrToFuse(info, &out.Attr)
		stable := fs.StableAttr{Mode: uint32(info.Mode())}
		return n.NewInode(ctx, child, stable), fs.OK
	}
	// Fall back to lower
	if info, err := n.sftp.Lstat(rp); err == nil {
		sftpAttrToFuse(info, &out.Attr)
		stable := fs.StableAttr{Mode: uint32(info.Mode())}
		return n.NewInode(ctx, child, stable), fs.OK
	}
	return nil, syscall.ENOENT
}

func (n *overlayNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	seen := make(map[string]struct{})
	var result []fuse.DirEntry

	// Upper entries first (they win on conflicts)
	if entries, err := os.ReadDir(n.upperPath); err == nil {
		for _, e := range entries {
			info, _ := e.Info()
			if info == nil {
				continue
			}
			seen[e.Name()] = struct{}{}
			result = append(result, fuse.DirEntry{
				Name: e.Name(),
				Mode: uint32(info.Mode()),
			})
		}
	}

	// Lower entries (skip duplicates)
	if entries, err := n.sftp.ReadDir(n.remotePath); err == nil {
		for _, e := range entries {
			if _, dup := seen[e.Name()]; dup {
				continue
			}
			result = append(result, fuse.DirEntry{
				Name: e.Name(),
				Mode: uint32(e.Mode()),
			})
		}
	}

	if result == nil {
		return nil, syscall.ENOENT
	}
	return fs.NewListDirStream(result), fs.OK
}

func (n *overlayNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	goFlags := int(flags) & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_TRUNC)
	writing := goFlags&(os.O_WRONLY|os.O_RDWR) != 0

	// If writing, do copy-up if needed then open from upper
	if writing {
		if err := n.copyUp(); err != nil {
			return nil, 0, syscall.EIO
		}
		f, err := os.OpenFile(n.upperPath, goFlags, 0644)
		if err != nil {
			return nil, 0, syscall.EIO
		}
		return &localFileHandle{f: f}, fuse.FOPEN_DIRECT_IO, fs.OK
	}

	// Read-only: prefer upper, fall back to lower
	if f, err := os.Open(n.upperPath); err == nil {
		return &localFileHandle{f: f}, fuse.FOPEN_DIRECT_IO, fs.OK
	}
	if f, err := n.sftp.Open(n.remotePath); err == nil {
		return &sftpFileHandle{f: f}, fuse.FOPEN_DIRECT_IO, fs.OK
	}
	return nil, 0, syscall.ENOENT
}

// copyUp ensures the file exists in the upper layer. If it only exists in
// lower (SFTP), it is copied to upper. Parent dirs are created as needed.
func (n *overlayNode) copyUp() error {
	if _, err := os.Lstat(n.upperPath); err == nil {
		return nil // already in upper
	}

	info, err := n.sftp.Lstat(n.remotePath)
	if err != nil {
		// Doesn't exist in lower either — that's fine, caller will create
		return nil
	}

	// Ensure parent dir exists in upper
	if err := os.MkdirAll(filepath.Dir(n.upperPath), 0755); err != nil {
		return err
	}

	if info.IsDir() {
		return os.MkdirAll(n.upperPath, info.Mode())
	}

	// Copy file content
	src, err := n.sftp.Open(n.remotePath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(n.upperPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		os.Remove(n.upperPath)
		return err
	}
	return nil
}

func (n *overlayNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	up := n.childUpperPath(name)
	if err := os.MkdirAll(n.upperPath, 0755); err != nil {
		return nil, nil, 0, syscall.EIO
	}

	f, err := os.OpenFile(up, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode))
	if err != nil {
		return nil, nil, 0, syscall.EIO
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, 0, syscall.EIO
	}
	localAttrToFuse(info, &out.Attr)

	child := n.child(name)
	stable := fs.StableAttr{Mode: uint32(info.Mode())}
	inode := n.NewInode(ctx, child, stable)
	return inode, &localFileHandle{f: f}, fuse.FOPEN_DIRECT_IO, fs.OK
}

func (n *overlayNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	up := n.childUpperPath(name)
	if err := os.MkdirAll(up, os.FileMode(mode)); err != nil {
		return nil, syscall.EIO
	}

	info, err := os.Lstat(up)
	if err != nil {
		return nil, syscall.EIO
	}
	localAttrToFuse(info, &out.Attr)

	child := n.child(name)
	stable := fs.StableAttr{Mode: uint32(info.Mode())}
	return n.NewInode(ctx, child, stable), fs.OK
}

func (n *overlayNode) Unlink(ctx context.Context, name string) syscall.Errno {
	up := n.childUpperPath(name)
	if err := os.Remove(up); err != nil && !os.IsNotExist(err) {
		return syscall.EIO
	}
	// Note: if the file only existed in lower, this is a no-op.
	// A full overlay would create a whiteout; for our use case (cache layer)
	// leaving the lower file visible is acceptable.
	return fs.OK
}

func (n *overlayNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	up := n.childUpperPath(name)
	if err := os.RemoveAll(up); err != nil && !os.IsNotExist(err) {
		return syscall.EIO
	}
	return fs.OK
}

func (n *overlayNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	newNode, ok := newParent.(*overlayNode)
	if !ok {
		return syscall.ENOSYS
	}

	oldChild := n.child(name)
	if err := oldChild.copyUp(); err != nil {
		return syscall.EIO
	}

	oldUp := n.childUpperPath(name)
	newUp := newNode.childUpperPath(newName)
	if err := os.MkdirAll(filepath.Dir(newUp), 0755); err != nil {
		return syscall.EIO
	}
	if err := os.Rename(oldUp, newUp); err != nil {
		return syscall.EIO
	}
	return fs.OK
}

func (n *overlayNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	// Copy-up before modifying attributes
	if err := n.copyUp(); err != nil {
		return syscall.EIO
	}

	if m, ok := in.GetMode(); ok {
		if err := os.Chmod(n.upperPath, os.FileMode(m)); err != nil {
			return syscall.EIO
		}
	}
	if size, ok := in.GetSize(); ok {
		if err := os.Truncate(n.upperPath, int64(size)); err != nil {
			return syscall.EIO
		}
	}

	info, err := os.Lstat(n.upperPath)
	if err != nil {
		return syscall.EIO
	}
	localAttrToFuse(info, &out.Attr)
	return fs.OK
}

func (n *overlayNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	// Upper first
	if target, err := os.Readlink(n.upperPath); err == nil {
		return []byte(target), fs.OK
	}
	if target, err := n.sftp.ReadLink(n.remotePath); err == nil {
		return []byte(target), fs.OK
	}
	return nil, syscall.ENOENT
}

func (n *overlayNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	up := n.childUpperPath(name)
	if err := os.MkdirAll(n.upperPath, 0755); err != nil {
		return nil, syscall.EIO
	}
	if err := os.Symlink(target, up); err != nil {
		return nil, syscall.EIO
	}

	info, err := os.Lstat(up)
	if err != nil {
		return nil, syscall.EIO
	}
	localAttrToFuse(info, &out.Attr)

	child := n.child(name)
	stable := fs.StableAttr{Mode: uint32(info.Mode())}
	return n.NewInode(ctx, child, stable), fs.OK
}

// --- File handles ---

// localFileHandle wraps an os.File for upper-layer operations.
type localFileHandle struct {
	f  *os.File
	mu sync.Mutex
}

var (
	_ fs.FileHandle  = (*localFileHandle)(nil)
	_ fs.FileReader  = (*localFileHandle)(nil)
	_ fs.FileWriter  = (*localFileHandle)(nil)
	_ fs.FileFlusher = (*localFileHandle)(nil)
)

func (h *localFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, err := h.f.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), fs.OK
}

func (h *localFileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, err := h.f.WriteAt(data, off)
	if err != nil {
		return 0, syscall.EIO
	}
	return uint32(n), fs.OK
}

func (h *localFileHandle) Flush(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.f.Sync(); err != nil {
		return syscall.EIO
	}
	return fs.OK
}

func (h *localFileHandle) Release(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f != nil {
		h.f.Close()
		h.f = nil
	}
	return fs.OK
}

// sftpFileHandle wraps an sftp.File for lower-layer read operations.
type sftpFileHandle struct {
	f  *sftp.File
	mu sync.Mutex
}

var (
	_ fs.FileHandle = (*sftpFileHandle)(nil)
	_ fs.FileReader = (*sftpFileHandle)(nil)
)

func (h *sftpFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.f.Seek(off, io.SeekStart); err != nil {
		return nil, syscall.EIO
	}
	n, err := h.f.Read(dest)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), fs.OK
}

func (h *sftpFileHandle) Release(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f != nil {
		h.f.Close()
		h.f = nil
	}
	return fs.OK
}
