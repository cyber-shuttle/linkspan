//go:build linux

package fuse

import (
	"context"
	"fmt"
	"os"
	"path"
	"syscall"
	"time"

	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fs"
)

// Mount bridges a local FUSE mount point to a remote FUSE TCP server.
// Kernel filesystem operations on the local mount point are translated to
// binary protocol messages and forwarded to the server via a Client.
type Mount struct {
	mountPoint string
	serverAddr string
	client     *Client
	server     *gofuse.Server
}

// NewMount creates a Mount that will serve the given mount point by
// connecting to the FUSE TCP server at serverAddr.
// Start must be called to create the mount and begin serving requests.
func NewMount(mountPoint, serverAddr string) *Mount {
	return &Mount{
		mountPoint: mountPoint,
		serverAddr: serverAddr,
	}
}

// MountPoint returns the local directory used as the FUSE mount point.
func (m *Mount) MountPoint() string {
	return m.mountPoint
}

// Start connects to the remote FUSE server, creates the local mount point,
// mounts the FUSE filesystem, and begins serving kernel requests in a
// background goroutine.
func (m *Mount) Start() error {
	if err := os.MkdirAll(m.mountPoint, 0o755); err != nil {
		return fmt.Errorf("fuse mount: create mount point %q: %w", m.mountPoint, err)
	}

	client, err := NewClient(m.serverAddr)
	if err != nil {
		return fmt.Errorf("fuse mount: connect to %s: %w", m.serverAddr, err)
	}
	m.client = client

	root := &bridgeRoot{client: client}

	attrTimeout := time.Second
	entryTimeout := time.Second
	opts := &fs.Options{
		AttrTimeout:  &attrTimeout,
		EntryTimeout: &entryTimeout,
		MountOptions: gofuse.MountOptions{
			FsName:        "linkspan",
			Name:          "linkspan",
			DisableXAttrs: true,
			Debug:         false,
		},
	}

	server, err := fs.Mount(m.mountPoint, root, opts)
	if err != nil {
		m.client.Close() //nolint:errcheck
		m.client = nil
		return fmt.Errorf("fuse mount: mount %q: %w", m.mountPoint, err)
	}
	m.server = server

	// fs.Mount already starts the serve loop internally in go-fuse v2.9+,
	// so we must NOT call server.Serve() again (it panics on double-call).

	return nil
}

// Stop unmounts the FUSE filesystem and closes the client connection to the
// remote server. It is safe to call Stop even if Start was not called or
// returned an error.
func (m *Mount) Stop() {
	if m.server != nil {
		m.server.Unmount() //nolint:errcheck
		m.server = nil
	}
	if m.client != nil {
		m.client.Close() //nolint:errcheck
		m.client = nil
	}
}

// ---------------------------------------------------------------------------
// bridgeRoot — root directory inode
// ---------------------------------------------------------------------------

// bridgeRoot is the root inode of the bridged filesystem. It implements the
// directory-related node interfaces from the go-fuse/v2/fs package and
// delegates all operations to the remote server via the Client.
type bridgeRoot struct {
	fs.Inode
	client *Client
	// relPath is the path relative to the server root ("" for the root itself).
	relPath string
}

// Ensure bridgeRoot implements the required interfaces.
var _ fs.NodeLookuper = (*bridgeRoot)(nil)
var _ fs.NodeReaddirer = (*bridgeRoot)(nil)
var _ fs.NodeGetattrer = (*bridgeRoot)(nil)
var _ fs.NodeCreater = (*bridgeRoot)(nil)
var _ fs.NodeMkdirer = (*bridgeRoot)(nil)
var _ fs.NodeUnlinker = (*bridgeRoot)(nil)
var _ fs.NodeRmdirer = (*bridgeRoot)(nil)
var _ fs.NodeRenamer = (*bridgeRoot)(nil)

// Lookup looks up a child name in this directory and returns the corresponding
// Inode. It calls GetAttr on the remote server to verify existence and obtain
// attributes.
func (d *bridgeRoot) Lookup(ctx context.Context, name string, out *gofuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childPath := joinPath(d.relPath, name)
	attr, err := d.client.GetAttr(childPath)
	if err != nil {
		return nil, remoteErrToErrno(err)
	}

	fillAttr(&out.Attr, attr)

	mode := attr.Mode & ^uint32(0o777) // keep only type bits for StableAttr
	var child fs.InodeEmbedder
	if isDir(attr.Mode) {
		child = &bridgeRoot{client: d.client, relPath: childPath}
	} else {
		child = &bridgeFile{client: d.client, relPath: childPath}
	}

	return d.NewInode(ctx, child, fs.StableAttr{Mode: mode}), 0
}

// Readdir returns all entries in this directory by calling ReadDir on the
// remote server.
func (d *bridgeRoot) Readdir(_ context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := d.client.ReadDir(d.relPath)
	if err != nil {
		return nil, remoteErrToErrno(err)
	}

	fuseDirEntries := make([]gofuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		fuseDirEntries = append(fuseDirEntries, gofuse.DirEntry{
			Name: e.Name,
			Mode: e.Mode,
		})
	}
	return fs.NewListDirStream(fuseDirEntries), 0
}

// Getattr returns the file attributes for this directory inode.
func (d *bridgeRoot) Getattr(_ context.Context, _ fs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	attr, err := d.client.GetAttr(d.relPath)
	if err != nil {
		return remoteErrToErrno(err)
	}
	fillAttr(&out.Attr, attr)
	return 0
}

// Create creates a new regular file with the given name in this directory.
func (d *bridgeRoot) Create(ctx context.Context, name string, flags uint32, mode uint32, out *gofuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	childPath := joinPath(d.relPath, name)
	if err := d.client.Create(childPath, os.FileMode(mode)); err != nil {
		return nil, nil, 0, remoteErrToErrno(err)
	}

	attr, err := d.client.GetAttr(childPath)
	if err != nil {
		return nil, nil, 0, remoteErrToErrno(err)
	}
	fillAttr(&out.Attr, attr)

	child := &bridgeFile{client: d.client, relPath: childPath}
	inode := d.NewInode(ctx, child, fs.StableAttr{Mode: gofuse.S_IFREG})
	fh := &bridgeFileHandle{client: d.client, relPath: childPath}
	return inode, fh, 0, 0
}

// Mkdir creates a new subdirectory with the given name in this directory.
func (d *bridgeRoot) Mkdir(ctx context.Context, name string, mode uint32, out *gofuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childPath := joinPath(d.relPath, name)
	if err := d.client.Mkdir(childPath, os.FileMode(mode)); err != nil {
		return nil, remoteErrToErrno(err)
	}

	attr, err := d.client.GetAttr(childPath)
	if err != nil {
		return nil, remoteErrToErrno(err)
	}
	fillAttr(&out.Attr, attr)

	child := &bridgeRoot{client: d.client, relPath: childPath}
	return d.NewInode(ctx, child, fs.StableAttr{Mode: gofuse.S_IFDIR}), 0
}

// Unlink removes a file entry from this directory.
func (d *bridgeRoot) Unlink(_ context.Context, name string) syscall.Errno {
	childPath := joinPath(d.relPath, name)
	if err := d.client.Unlink(childPath); err != nil {
		return remoteErrToErrno(err)
	}
	return 0
}

// Rmdir removes a subdirectory from this directory.
func (d *bridgeRoot) Rmdir(_ context.Context, name string) syscall.Errno {
	childPath := joinPath(d.relPath, name)
	if err := d.client.Rmdir(childPath); err != nil {
		return remoteErrToErrno(err)
	}
	return 0
}

// Rename moves a child entry from this directory to newParent under newName.
func (d *bridgeRoot) Rename(_ context.Context, name string, newParent fs.InodeEmbedder, newName string, _ uint32) syscall.Errno {
	oldPath := joinPath(d.relPath, name)

	// Determine the destination directory's relPath by type-asserting the
	// InodeEmbedder to our bridge directory type.
	var newDirPath string
	switch p := newParent.(type) {
	case *bridgeRoot:
		newDirPath = p.relPath
	default:
		return syscall.EINVAL
	}

	newPath := joinPath(newDirPath, newName)
	if err := d.client.Rename(oldPath, newPath); err != nil {
		return remoteErrToErrno(err)
	}
	return 0
}

// ---------------------------------------------------------------------------
// bridgeFile — regular file inode
// ---------------------------------------------------------------------------

// bridgeFile is a regular-file inode. It implements Getattr and Open; the
// actual I/O is performed by bridgeFileHandle which is returned from Open.
type bridgeFile struct {
	fs.Inode
	client  *Client
	relPath string
}

// Ensure bridgeFile implements the required interfaces.
var _ fs.NodeGetattrer = (*bridgeFile)(nil)
var _ fs.NodeOpener = (*bridgeFile)(nil)

// Getattr returns the file attributes for this regular-file inode.
func (f *bridgeFile) Getattr(_ context.Context, _ fs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	attr, err := f.client.GetAttr(f.relPath)
	if err != nil {
		return remoteErrToErrno(err)
	}
	fillAttr(&out.Attr, attr)
	return 0
}

// Open opens the file and returns a bridgeFileHandle for subsequent reads and
// writes.
func (f *bridgeFile) Open(_ context.Context, _ uint32) (fs.FileHandle, uint32, syscall.Errno) {
	fh := &bridgeFileHandle{client: f.client, relPath: f.relPath}
	return fh, gofuse.FOPEN_KEEP_CACHE, 0
}

// ---------------------------------------------------------------------------
// bridgeFileHandle — open file handle
// ---------------------------------------------------------------------------

// bridgeFileHandle implements the FileReader and FileWriter interfaces for an
// open file. It delegates all I/O to the Client.
type bridgeFileHandle struct {
	client  *Client
	relPath string
}

// Ensure bridgeFileHandle implements the required interfaces.
var _ fs.FileReader = (*bridgeFileHandle)(nil)
var _ fs.FileWriter = (*bridgeFileHandle)(nil)

// Read reads len(dest) bytes from the remote file starting at off.
func (fh *bridgeFileHandle) Read(_ context.Context, dest []byte, off int64) (gofuse.ReadResult, syscall.Errno) {
	data, err := fh.client.Read(fh.relPath, uint64(off), uint32(len(dest)))
	if err != nil {
		return nil, remoteErrToErrno(err)
	}
	return gofuse.ReadResultData(data), 0
}

// Write writes data to the remote file starting at off.
func (fh *bridgeFileHandle) Write(_ context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	n, err := fh.client.Write(fh.relPath, uint64(off), data)
	if err != nil {
		return 0, remoteErrToErrno(err)
	}
	return uint32(n), 0
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// joinPath joins a directory's relative path with a child name.
// If dir is empty (root directory) the child name is returned as-is.
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return path.Join(dir, name)
}

// isDir returns true when the mode indicates a directory.
func isDir(mode uint32) bool {
	return (mode & syscall.S_IFMT) == syscall.S_IFDIR
}

// fillAttr populates a gofuse.Attr from an AttrInfo.
func fillAttr(out *gofuse.Attr, a *AttrInfo) {
	out.Mode = a.Mode
	out.Size = uint64(a.Size)
	out.Atime = uint64(a.Atime)
	out.Mtime = uint64(a.Mtime)
	out.Ctime = uint64(a.Ctime)
	out.Nlink = 1
}

// remoteErrToErrno maps a Client error to a syscall.Errno.
// FuseError values carry raw POSIX errno numbers from the server; other
// errors are mapped to EIO.
func remoteErrToErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	if fe, ok := err.(*FuseError); ok {
		return syscall.Errno(fe.Errno)
	}
	return syscall.EIO
}
