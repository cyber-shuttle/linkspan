package mount

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// reconnectingSftpClient wraps an SFTP client with automatic reconnection.
// All overlay nodes share a single instance so reconnection is transparent.
type reconnectingSftpClient struct {
	mu        sync.RWMutex
	client    *sftp.Client
	sshClient *ssh.Client
	sshAddr   string
	sshConfig *ssh.ClientConfig
	closed    bool
}

func newReconnectingSftpClient(sshAddr string, sshConfig *ssh.ClientConfig) (*reconnectingSftpClient, error) {
	r := &reconnectingSftpClient{
		sshAddr:   sshAddr,
		sshConfig: sshConfig,
	}
	if err := r.connect(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *reconnectingSftpClient) connect() error {
	sshConn, err := ssh.Dial("tcp", r.sshAddr, r.sshConfig)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", r.sshAddr, err)
	}
	sftpConn, err := sftp.NewClient(sshConn)
	if err != nil {
		sshConn.Close()
		return fmt.Errorf("sftp client: %w", err)
	}
	r.sshClient = sshConn
	r.client = sftpConn
	return nil
}

// reconnect attempts to re-establish the SFTP connection with backoff.
// Caller must hold r.mu write lock.
func (r *reconnectingSftpClient) reconnect() error {
	if r.closed {
		return fmt.Errorf("sftp client closed")
	}
	// Close old connections
	if r.client != nil {
		r.client.Close()
		r.client = nil
	}
	if r.sshClient != nil {
		r.sshClient.Close()
		r.sshClient = nil
	}

	delays := []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, delay := range delays {
		if err := r.connect(); err == nil {
			log.Printf("[overlayfs] SFTP reconnected to %s (attempt %d)", r.sshAddr, i+1)
			return nil
		}
		log.Printf("[overlayfs] SFTP reconnect attempt %d/%d to %s failed, retrying in %s", i+1, len(delays), r.sshAddr, delay)
		time.Sleep(delay)
	}
	return fmt.Errorf("sftp reconnect failed after %d attempts", len(delays))
}

// withClient runs fn with a live SFTP client. If the client has disconnected,
// it attempts to reconnect transparently.
func (r *reconnectingSftpClient) withClient(fn func(*sftp.Client) error) error {
	// Fast path: snapshot client under read lock, then call fn without holding the lock.
	r.mu.RLock()
	client := r.client
	r.mu.RUnlock()

	if client != nil {
		if err := fn(client); err == nil || !isConnectionError(err) {
			return err
		}
	}

	// Slow path: reconnect under write lock.
	r.mu.Lock()
	// Double-check: another goroutine may have already reconnected.
	if r.client != nil && r.client != client {
		client = r.client
		r.mu.Unlock()
		return fn(client)
	}
	if err := r.reconnect(); err != nil {
		r.mu.Unlock()
		return err
	}
	client = r.client
	r.mu.Unlock()
	return fn(client)
}

// Close shuts down the SFTP and SSH connections permanently.
func (r *reconnectingSftpClient) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.client != nil {
		r.client.Close()
		r.client = nil
	}
	if r.sshClient != nil {
		r.sshClient.Close()
		r.sshClient = nil
	}
}

// isConnectionError returns true if the error indicates the SSH/SFTP
// connection is dead and a reconnect should be attempted.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	// Check for common connection-related errors
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return true
	}
	if _, ok := err.(*net.OpError); ok {
		return true
	}
	// ssh package errors when the connection is closed
	msg := err.Error()
	for _, substr := range []string{
		"connection reset",
		"broken pipe",
		"use of closed network connection",
		"connection refused",
		"ssh: disconnect",
		"session not started",
	} {
		if len(msg) >= len(substr) {
			for i := 0; i <= len(msg)-len(substr); i++ {
				if msg[i:i+len(substr)] == substr {
					return true
				}
			}
		}
	}
	return false
}

// OverlayFS is a userspace overlay FUSE filesystem.
// Reads check upper (local cache) first, then fall back to lower (SFTP).
// Writes always go to upper.
type OverlayFS struct {
	server   *fuse.Server
	rsftp    *reconnectingSftpClient
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

	rsftp, err := newReconnectingSftpClient(sshAddr, sshConfig)
	if err != nil {
		return nil, err
	}

	root := &overlayNode{
		rsftp:      rsftp,
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
		rsftp.Close()
		return nil, fmt.Errorf("fuse mount %s: %w", mountDir, err)
	}

	log.Printf("[overlayfs] mounted at %s (upper=%s, lower=%s:%s)", mountDir, upperDir, sshAddr, remoteRoot)

	return &OverlayFS{
		server:   server,
		rsftp:    rsftp,
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
	if o.rsftp != nil {
		o.rsftp.Close()
		o.rsftp = nil
	}
}

// overlayNode implements a FUSE node that overlays a local upper dir on an SFTP lower dir.
type overlayNode struct {
	fs.Inode
	rsftp      *reconnectingSftpClient
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
		rsftp:      n.rsftp,
		remotePath: n.childRemotePath(name),
		upperPath:  n.childUpperPath(name),
	}
}

// sftpLstat wraps SFTP Lstat with automatic reconnection.
func (n *overlayNode) sftpLstat(path string) (os.FileInfo, error) {
	var info os.FileInfo
	err := n.rsftp.withClient(func(c *sftp.Client) error {
		var e error
		info, e = c.Lstat(path)
		return e
	})
	return info, err
}

// locate determines where a path exists (upper, lower, both, or none).
func (n *overlayNode) locate() source {
	_, upperErr := os.Lstat(n.upperPath)
	_, lowerErr := n.sftpLstat(n.remotePath)
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
	_, lowerErr := n.sftpLstat(n.childRemotePath(name))
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
	if info, err := n.sftpLstat(n.remotePath); err == nil {
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
	if info, err := n.sftpLstat(rp); err == nil {
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

	// Lower entries (skip duplicates). On SFTP error we log and continue
	// with upper-only entries rather than blocking the entire readdir.
	if err := n.rsftp.withClient(func(c *sftp.Client) error {
		entries, err := c.ReadDir(n.remotePath)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if _, dup := seen[e.Name()]; dup {
				continue
			}
			result = append(result, fuse.DirEntry{
				Name: e.Name(),
				Mode: uint32(e.Mode()),
			})
		}
		return nil
	}); err != nil {
		log.Printf("[overlayfs] readdir %s: SFTP error (returning upper-only entries): %v", n.remotePath, err)
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
	var sftpFile *sftp.File
	err := n.rsftp.withClient(func(c *sftp.Client) error {
		var e error
		sftpFile, e = c.Open(n.remotePath)
		return e
	})
	if err == nil {
		return &sftpFileHandle{f: sftpFile}, fuse.FOPEN_DIRECT_IO, fs.OK
	}
	return nil, 0, syscall.ENOENT
}

// copyUp ensures the file exists in the upper layer. If it only exists in
// lower (SFTP), it is copied to upper. Parent dirs are created as needed.
// The copy is atomic: content is written to a temp file and renamed into
// place on success; the temp file is removed on any failure.
func (n *overlayNode) copyUp() error {
	if _, err := os.Lstat(n.upperPath); err == nil {
		return nil // already in upper
	}

	info, err := n.sftpLstat(n.remotePath)
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

	// Copy file content from SFTP to local atomically via a temp file.
	return n.rsftp.withClient(func(c *sftp.Client) error {
		src, err := c.Open(n.remotePath)
		if err != nil {
			return err
		}
		defer src.Close()

		// Write to a sibling temp file so a partial copy never leaves a
		// corrupt file at n.upperPath.
		tmp, err := os.CreateTemp(filepath.Dir(n.upperPath), ".copyup-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()

		// Ensure the temp file is cleaned up on any failure path.
		success := false
		defer func() {
			if !success {
				tmp.Close()
				os.Remove(tmpName)
			}
		}()

		if err := tmp.Chmod(info.Mode()); err != nil {
			return err
		}
		if _, err := io.Copy(tmp, src); err != nil {
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}

		if err := os.Rename(tmpName, n.upperPath); err != nil {
			return err
		}
		success = true
		return nil
	})
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
	var target string
	err := n.rsftp.withClient(func(c *sftp.Client) error {
		var e error
		target, e = c.ReadLink(n.remotePath)
		return e
	})
	if err == nil {
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
