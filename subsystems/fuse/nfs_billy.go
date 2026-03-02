package fuse

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	billy "github.com/go-git/go-billy/v5"
)

// remoteBillyFS adapts a FUSE TCP *Client into a billy.Filesystem so that it
// can be served by go-nfs. It also implements billy.Change so that the NFS
// server can expose (no-op) ownership/permission helpers.
type remoteBillyFS struct {
	client *Client
	root   string // prefix applied to all paths (for Chroot)
}

// Compile-time interface checks.
var _ billy.Filesystem = (*remoteBillyFS)(nil)
var _ billy.Change = (*remoteBillyFS)(nil)

func newRemoteBillyFS(client *Client) *remoteBillyFS {
	return &remoteBillyFS{client: client, root: "/"}
}

// resolvePath prepends the chroot root to name and cleans the result.
func (fs *remoteBillyFS) resolvePath(name string) string {
	p := path.Join(fs.root, name)
	// The FUSE TCP server uses paths without leading "/", but root itself is
	// represented as empty string internally.  Strip leading slash so that
	// "foo/bar" is sent to the server rather than "/foo/bar".
	p = strings.TrimPrefix(p, "/")
	return p
}

// ---------------------------------------------------------------------------
// billy.Basic
// ---------------------------------------------------------------------------

func (fs *remoteBillyFS) Create(filename string) (billy.File, error) {
	p := fs.resolvePath(filename)
	if err := fs.client.Create(p, 0o644); err != nil {
		return nil, err
	}
	return newRemoteBillyFile(fs.client, p, filename), nil
}

func (fs *remoteBillyFS) Open(filename string) (billy.File, error) {
	p := fs.resolvePath(filename)
	// Verify the file exists.
	if _, err := fs.client.GetAttr(p); err != nil {
		return nil, err
	}
	return newRemoteBillyFile(fs.client, p, filename), nil
}

func (fs *remoteBillyFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	p := fs.resolvePath(filename)
	if flag&os.O_CREATE != 0 {
		// Try to create; ignore EEXIST if not O_EXCL.
		if err := fs.client.Create(p, perm); err != nil {
			if flag&os.O_EXCL != 0 {
				return nil, err
			}
			// File may already exist — that's fine.
		}
	} else {
		if _, err := fs.client.GetAttr(p); err != nil {
			return nil, err
		}
	}
	f := newRemoteBillyFile(fs.client, p, filename)
	if flag&os.O_APPEND != 0 {
		// Seek to end for append mode.
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (fs *remoteBillyFS) Stat(filename string) (os.FileInfo, error) {
	p := fs.resolvePath(filename)
	attr, err := fs.client.GetAttr(p)
	if err != nil {
		return nil, err
	}
	return newRemoteBillyFileInfo(path.Base(filename), attr), nil
}

func (fs *remoteBillyFS) Rename(oldpath, newpath string) error {
	return fs.client.Rename(fs.resolvePath(oldpath), fs.resolvePath(newpath))
}

func (fs *remoteBillyFS) Remove(filename string) error {
	p := fs.resolvePath(filename)
	attr, err := fs.client.GetAttr(p)
	if err != nil {
		return err
	}
	if posixIsDir(attr.Mode) {
		return fs.client.Rmdir(p)
	}
	return fs.client.Unlink(p)
}

func (fs *remoteBillyFS) Join(elem ...string) string {
	return path.Join(elem...)
}

// ---------------------------------------------------------------------------
// billy.TempFile
// ---------------------------------------------------------------------------

var tempSeq atomic.Uint64

func (fs *remoteBillyFS) TempFile(dir, prefix string) (billy.File, error) {
	if dir == "" {
		dir = "tmp"
	}
	// Ensure the directory exists.
	_ = fs.MkdirAll(dir, 0o755)
	name := fmt.Sprintf("%s%d", prefix, tempSeq.Add(1))
	return fs.Create(path.Join(dir, name))
}

// ---------------------------------------------------------------------------
// billy.Dir
// ---------------------------------------------------------------------------

func (fs *remoteBillyFS) ReadDir(dirname string) ([]os.FileInfo, error) {
	p := fs.resolvePath(dirname)
	entries, err := fs.client.ReadDir(p)
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, len(entries))
	for i, e := range entries {
		infos[i] = newRemoteBillyFileInfoFromDirEntry(e)
	}
	return infos, nil
}

func (fs *remoteBillyFS) MkdirAll(filename string, perm os.FileMode) error {
	parts := strings.Split(path.Clean(filename), "/")
	current := ""
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if current == "" {
			current = part
		} else {
			current = current + "/" + part
		}
		p := fs.resolvePath(current)
		err := fs.client.Mkdir(p, perm)
		if err != nil {
			// Ignore "already exists" errors.
			if fe, ok := err.(*FuseError); ok && Status(fe.Errno) == EEXIST {
				continue
			}
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// billy.Symlink (unsupported — return errors)
// ---------------------------------------------------------------------------

func (fs *remoteBillyFS) Lstat(filename string) (os.FileInfo, error) {
	// No symlink support; behave like Stat.
	return fs.Stat(filename)
}

func (fs *remoteBillyFS) Symlink(target, link string) error {
	return os.ErrPermission
}

func (fs *remoteBillyFS) Readlink(link string) (string, error) {
	return "", os.ErrPermission
}

// ---------------------------------------------------------------------------
// billy.Chroot
// ---------------------------------------------------------------------------

func (fs *remoteBillyFS) Chroot(p string) (billy.Filesystem, error) {
	newRoot := path.Join(fs.root, p)
	return &remoteBillyFS{client: fs.client, root: newRoot}, nil
}

func (fs *remoteBillyFS) Root() string {
	return fs.root
}

// ---------------------------------------------------------------------------
// billy.Change (no-op implementations — FUSE protocol has no SetAttr)
// ---------------------------------------------------------------------------

func (fs *remoteBillyFS) Chmod(name string, mode os.FileMode) error { return nil }
func (fs *remoteBillyFS) Lchown(name string, uid, gid int) error   { return nil }
func (fs *remoteBillyFS) Chown(name string, uid, gid int) error    { return nil }

func (fs *remoteBillyFS) Chtimes(name string, atime time.Time, mtime time.Time) error {
	return nil
}

// =========================================================================
// remoteBillyFile implements billy.File
// =========================================================================

type remoteBillyFile struct {
	client *Client
	path   string // resolved (server-side) path
	name   string // user-facing name from Open/Create
	offset int64
}

var _ billy.File = (*remoteBillyFile)(nil)

func newRemoteBillyFile(client *Client, serverPath, name string) *remoteBillyFile {
	return &remoteBillyFile{client: client, path: serverPath, name: name}
}

func (f *remoteBillyFile) Name() string { return f.name }

func (f *remoteBillyFile) Read(p []byte) (int, error) {
	data, err := f.client.Read(f.path, uint64(f.offset), uint32(len(p)))
	if err != nil {
		return 0, err
	}
	n := copy(p, data)
	f.offset += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *remoteBillyFile) ReadAt(p []byte, off int64) (int, error) {
	data, err := f.client.Read(f.path, uint64(off), uint32(len(p)))
	if err != nil {
		return 0, err
	}
	n := copy(p, data)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *remoteBillyFile) Write(p []byte) (int, error) {
	n, err := f.client.Write(f.path, uint64(f.offset), p)
	if err != nil {
		return 0, err
	}
	f.offset += int64(n)
	return n, nil
}

func (f *remoteBillyFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.offset = offset
	case io.SeekCurrent:
		f.offset += offset
	case io.SeekEnd:
		attr, err := f.client.GetAttr(f.path)
		if err != nil {
			return 0, err
		}
		f.offset = attr.Size + offset
	default:
		return 0, fmt.Errorf("fuse: invalid seek whence %d", whence)
	}
	return f.offset, nil
}

func (f *remoteBillyFile) Close() error   { return nil }
func (f *remoteBillyFile) Lock() error    { return nil }
func (f *remoteBillyFile) Unlock() error  { return nil }

func (f *remoteBillyFile) Truncate(size int64) error {
	if size == 0 {
		// Re-create the file to truncate it.
		_ = f.client.Unlink(f.path)
		return f.client.Create(f.path, 0o644)
	}
	// Non-zero truncate is not supported by the FUSE protocol.
	return nil
}

// =========================================================================
// remoteBillyFileInfo implements os.FileInfo
// =========================================================================

type remoteBillyFileInfo struct {
	name string
	attr *AttrInfo
}

var _ os.FileInfo = (*remoteBillyFileInfo)(nil)

func newRemoteBillyFileInfo(name string, attr *AttrInfo) *remoteBillyFileInfo {
	return &remoteBillyFileInfo{name: name, attr: attr}
}

func newRemoteBillyFileInfoFromDirEntry(e DirEntry) *remoteBillyFileInfo {
	return &remoteBillyFileInfo{
		name: e.Name,
		attr: &AttrInfo{Mode: e.Mode, Size: e.Size},
	}
}

func (fi *remoteBillyFileInfo) Name() string      { return fi.name }
func (fi *remoteBillyFileInfo) Size() int64        { return fi.attr.Size }
func (fi *remoteBillyFileInfo) Mode() os.FileMode  { return posixToGoMode(fi.attr.Mode) }
func (fi *remoteBillyFileInfo) ModTime() time.Time { return time.Unix(fi.attr.Mtime, 0) }
func (fi *remoteBillyFileInfo) IsDir() bool        { return fi.Mode().IsDir() }

// Sys returns nil so that go-nfs generates unique Fileid values by hashing
// the file path. Returning a nfsfile.FileInfo with Fileid=0 would cause all
// entries to share the same inode, breaking macOS NFS readdir.
func (fi *remoteBillyFileInfo) Sys() any {
	return nil
}

// =========================================================================
// Helpers
// =========================================================================

// posixToGoMode converts a POSIX mode_t value back to Go's os.FileMode.
// This is the inverse of GoModeToPOSIX in protocol.go.
func posixToGoMode(m uint32) os.FileMode {
	mode := os.FileMode(m & 0o777) // permission bits

	// Map POSIX S_IF* type bits to Go FileMode constants.
	switch m & 0o170000 {
	case 0o040000: // S_IFDIR
		mode |= os.ModeDir
	case 0o120000: // S_IFLNK
		mode |= os.ModeSymlink
	case 0o010000: // S_IFIFO
		mode |= os.ModeNamedPipe
	case 0o140000: // S_IFSOCK
		mode |= os.ModeSocket
	case 0o020000: // S_IFCHR
		mode |= os.ModeDevice | os.ModeCharDevice
	case 0o060000: // S_IFBLK
		mode |= os.ModeDevice
	}

	// Setuid/setgid/sticky.
	if m&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if m&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if m&0o1000 != 0 {
		mode |= os.ModeSticky
	}

	return mode
}

// posixIsDir returns true when the POSIX mode indicates a directory.
func posixIsDir(mode uint32) bool {
	return (mode & 0o170000) == 0o040000
}
