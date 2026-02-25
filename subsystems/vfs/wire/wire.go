// Package wire implements a binary protocol for remote filesystem operations.
//
// Frame format: [4-byte big-endian payload length][payload bytes]
// Request payload:  [4B request_id][1B op][op-specific fields...]
// Response payload: [4B request_id][1B op][4B errno][result fields if errno==0...]
//
// Strings are encoded as [2B length][bytes].
// Byte slices (file data) are encoded as [4B length][bytes].
// Attr is a fixed 68-byte struct.
package wire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// Op codes for file system operations.
const (
	OpGetAttr    byte = 0x01
	OpLookup     byte = 0x02
	OpOpen       byte = 0x03
	OpRead       byte = 0x04
	OpWrite      byte = 0x05
	OpRelease    byte = 0x06
	OpOpendir    byte = 0x07
	OpReaddir    byte = 0x08
	OpReleasedir byte = 0x09
	OpCreate     byte = 0x0A
	OpMkdir      byte = 0x0B
	OpUnlink     byte = 0x0C
	OpRmdir      byte = 0x0D
	OpRename     byte = 0x0E
	OpSymlink    byte = 0x0F
	OpReadlink   byte = 0x10
	OpSetAttr    byte = 0x11
	OpStatfs     byte = 0x12
	OpFlush      byte = 0x13
	OpFsync      byte = 0x14
)

// SetAttr field presence bitmask.
const (
	SetAttrSize  uint32 = 1 << 0
	SetAttrMtime uint32 = 1 << 1
	SetAttrAtime uint32 = 1 << 2
	SetAttrMode  uint32 = 1 << 3
	SetAttrUid   uint32 = 1 << 4
	SetAttrGid   uint32 = 1 << 5
)

// ErrClosed is returned when the client connection has been closed.
var ErrClosed = errors.New("connection closed")

// Attr represents file attributes (68 bytes on the wire).
type Attr struct {
	Ino     uint64
	Size    uint64
	Mode    uint32
	Uid     uint32
	Gid     uint32
	Atime   uint64
	Mtime   uint64
	Ctime   uint64
	Blksize uint64
	Blocks  uint64
}

// DirEntry represents a directory entry.
type DirEntry struct {
	Name string
	Mode uint32
	Ino  uint64
}

// ExportPath maps a virtual name to a local path.
type ExportPath struct {
	LocalPath   string
	VirtualName string
}

// Request carries a file operation request.
type Request struct {
	ID       uint32
	Op       byte
	Path     string
	Name     string // Lookup, Create, Mkdir, Unlink, Rmdir, Symlink; also OldName for Rename
	HandleID uint64
	Flags    uint32
	Mode     uint32
	Offset   int64
	Size     uint32
	Data     []byte
	NewPath  string // Rename
	NewName  string // Rename
	Target   string // Symlink target

	SetAttrValid uint32
	SetSize      uint64
	SetMtime     uint64
	SetAtime     uint64
	SetMode      uint32
	SetUid       uint32
	SetGid       uint32
}

// Response carries a file operation response.
type Response struct {
	ID       uint32
	Op       byte
	Errno    uint32
	Attr     *Attr
	HandleID uint64
	Data     []byte
	Written  uint32
	Entries  []DirEntry
	Target   string
	Blocks   uint64
	Bfree    uint64
	Bavail   uint64
	Files    uint64
	Ffree    uint64
	Bsize    uint64
	Namelen  uint64
}

// ---------- encoder ----------

type encoder struct{ b []byte }

func (e *encoder) putByte(v byte)     { e.b = append(e.b, v) }
func (e *encoder) putUint16(v uint16) { e.b = binary.BigEndian.AppendUint16(e.b, v) }
func (e *encoder) putUint32(v uint32) { e.b = binary.BigEndian.AppendUint32(e.b, v) }
func (e *encoder) putUint64(v uint64) { e.b = binary.BigEndian.AppendUint64(e.b, v) }
func (e *encoder) putInt64(v int64)   { e.putUint64(uint64(v)) }

func (e *encoder) putStr(s string) {
	e.putUint16(uint16(len(s)))
	e.b = append(e.b, s...)
}

func (e *encoder) putBytes(data []byte) {
	e.putUint32(uint32(len(data)))
	e.b = append(e.b, data...)
}

func (e *encoder) putAttr(a *Attr) {
	if a == nil {
		a = &Attr{}
	}
	e.putUint64(a.Ino)
	e.putUint64(a.Size)
	e.putUint32(a.Mode)
	e.putUint32(a.Uid)
	e.putUint32(a.Gid)
	e.putUint64(a.Atime)
	e.putUint64(a.Mtime)
	e.putUint64(a.Ctime)
	e.putUint64(a.Blksize)
	e.putUint64(a.Blocks)
}

func (e *encoder) putDirEntry(de *DirEntry) {
	e.putStr(de.Name)
	e.putUint32(de.Mode)
	e.putUint64(de.Ino)
}

// ---------- decoder ----------

type decoder struct {
	b   []byte
	pos int
	err error
}

func (d *decoder) getByte() byte {
	if d.err != nil || d.pos+1 > len(d.b) {
		d.err = io.ErrUnexpectedEOF
		return 0
	}
	v := d.b[d.pos]
	d.pos++
	return v
}

func (d *decoder) getUint16() uint16 {
	if d.err != nil || d.pos+2 > len(d.b) {
		d.err = io.ErrUnexpectedEOF
		return 0
	}
	v := binary.BigEndian.Uint16(d.b[d.pos:])
	d.pos += 2
	return v
}

func (d *decoder) getUint32() uint32 {
	if d.err != nil || d.pos+4 > len(d.b) {
		d.err = io.ErrUnexpectedEOF
		return 0
	}
	v := binary.BigEndian.Uint32(d.b[d.pos:])
	d.pos += 4
	return v
}

func (d *decoder) getUint64() uint64 {
	if d.err != nil || d.pos+8 > len(d.b) {
		d.err = io.ErrUnexpectedEOF
		return 0
	}
	v := binary.BigEndian.Uint64(d.b[d.pos:])
	d.pos += 8
	return v
}

func (d *decoder) getInt64() int64 { return int64(d.getUint64()) }

func (d *decoder) getStr() string {
	n := int(d.getUint16())
	if d.err != nil || d.pos+n > len(d.b) {
		d.err = io.ErrUnexpectedEOF
		return ""
	}
	s := string(d.b[d.pos : d.pos+n])
	d.pos += n
	return s
}

func (d *decoder) getBytes() []byte {
	n := int(d.getUint32())
	if d.err != nil || d.pos+n > len(d.b) {
		d.err = io.ErrUnexpectedEOF
		return nil
	}
	data := make([]byte, n)
	copy(data, d.b[d.pos:d.pos+n])
	d.pos += n
	return data
}

func (d *decoder) getAttr() *Attr {
	return &Attr{
		Ino:     d.getUint64(),
		Size:    d.getUint64(),
		Mode:    d.getUint32(),
		Uid:     d.getUint32(),
		Gid:     d.getUint32(),
		Atime:   d.getUint64(),
		Mtime:   d.getUint64(),
		Ctime:   d.getUint64(),
		Blksize: d.getUint64(),
		Blocks:  d.getUint64(),
	}
}

func (d *decoder) getDirEntry() DirEntry {
	return DirEntry{
		Name: d.getStr(),
		Mode: d.getUint32(),
		Ino:  d.getUint64(),
	}
}

// ---------- request encoding ----------

func encodeRequest(req *Request) []byte {
	e := encoder{b: make([]byte, 0, 128)}
	e.putUint32(req.ID)
	e.putByte(req.Op)

	switch req.Op {
	case OpGetAttr, OpOpendir, OpReadlink, OpStatfs:
		e.putStr(req.Path)
	case OpLookup, OpUnlink, OpRmdir:
		e.putStr(req.Path)
		e.putStr(req.Name)
	case OpOpen:
		e.putStr(req.Path)
		e.putUint32(req.Flags)
	case OpRead:
		e.putStr(req.Path)
		e.putUint64(req.HandleID)
		e.putInt64(req.Offset)
		e.putUint32(req.Size)
	case OpWrite:
		e.putStr(req.Path)
		e.putUint64(req.HandleID)
		e.putInt64(req.Offset)
		e.putBytes(req.Data)
	case OpRelease, OpReaddir, OpReleasedir, OpFlush:
		e.putStr(req.Path)
		e.putUint64(req.HandleID)
	case OpCreate:
		e.putStr(req.Path)
		e.putStr(req.Name)
		e.putUint32(req.Flags)
		e.putUint32(req.Mode)
	case OpMkdir:
		e.putStr(req.Path)
		e.putStr(req.Name)
		e.putUint32(req.Mode)
	case OpRename:
		e.putStr(req.Path)
		e.putStr(req.Name)
		e.putStr(req.NewPath)
		e.putStr(req.NewName)
	case OpSymlink:
		e.putStr(req.Path)
		e.putStr(req.Name)
		e.putStr(req.Target)
	case OpSetAttr:
		e.putStr(req.Path)
		e.putUint32(req.SetAttrValid)
		if req.SetAttrValid&SetAttrSize != 0 {
			e.putUint64(req.SetSize)
		}
		if req.SetAttrValid&SetAttrMtime != 0 {
			e.putUint64(req.SetMtime)
		}
		if req.SetAttrValid&SetAttrAtime != 0 {
			e.putUint64(req.SetAtime)
		}
		if req.SetAttrValid&SetAttrMode != 0 {
			e.putUint32(req.SetMode)
		}
		if req.SetAttrValid&SetAttrUid != 0 {
			e.putUint32(req.SetUid)
		}
		if req.SetAttrValid&SetAttrGid != 0 {
			e.putUint32(req.SetGid)
		}
	case OpFsync:
		e.putStr(req.Path)
		e.putUint64(req.HandleID)
		e.putUint32(req.Flags)
	}

	return e.b
}

func decodeRequest(data []byte) (*Request, error) {
	d := decoder{b: data}
	req := &Request{
		ID: d.getUint32(),
		Op: d.getByte(),
	}

	switch req.Op {
	case OpGetAttr, OpOpendir, OpReadlink, OpStatfs:
		req.Path = d.getStr()
	case OpLookup, OpUnlink, OpRmdir:
		req.Path = d.getStr()
		req.Name = d.getStr()
	case OpOpen:
		req.Path = d.getStr()
		req.Flags = d.getUint32()
	case OpRead:
		req.Path = d.getStr()
		req.HandleID = d.getUint64()
		req.Offset = d.getInt64()
		req.Size = d.getUint32()
	case OpWrite:
		req.Path = d.getStr()
		req.HandleID = d.getUint64()
		req.Offset = d.getInt64()
		req.Data = d.getBytes()
	case OpRelease, OpReaddir, OpReleasedir, OpFlush:
		req.Path = d.getStr()
		req.HandleID = d.getUint64()
	case OpCreate:
		req.Path = d.getStr()
		req.Name = d.getStr()
		req.Flags = d.getUint32()
		req.Mode = d.getUint32()
	case OpMkdir:
		req.Path = d.getStr()
		req.Name = d.getStr()
		req.Mode = d.getUint32()
	case OpRename:
		req.Path = d.getStr()
		req.Name = d.getStr()
		req.NewPath = d.getStr()
		req.NewName = d.getStr()
	case OpSymlink:
		req.Path = d.getStr()
		req.Name = d.getStr()
		req.Target = d.getStr()
	case OpSetAttr:
		req.Path = d.getStr()
		req.SetAttrValid = d.getUint32()
		if req.SetAttrValid&SetAttrSize != 0 {
			req.SetSize = d.getUint64()
		}
		if req.SetAttrValid&SetAttrMtime != 0 {
			req.SetMtime = d.getUint64()
		}
		if req.SetAttrValid&SetAttrAtime != 0 {
			req.SetAtime = d.getUint64()
		}
		if req.SetAttrValid&SetAttrMode != 0 {
			req.SetMode = d.getUint32()
		}
		if req.SetAttrValid&SetAttrUid != 0 {
			req.SetUid = d.getUint32()
		}
		if req.SetAttrValid&SetAttrGid != 0 {
			req.SetGid = d.getUint32()
		}
	case OpFsync:
		req.Path = d.getStr()
		req.HandleID = d.getUint64()
		req.Flags = d.getUint32()
	}

	if d.err != nil {
		return nil, d.err
	}
	return req, nil
}

// ---------- response encoding ----------

func encodeResponse(resp *Response) []byte {
	e := encoder{b: make([]byte, 0, 128)}
	e.putUint32(resp.ID)
	e.putByte(resp.Op)
	e.putUint32(resp.Errno)

	if resp.Errno == 0 {
		switch resp.Op {
		case OpGetAttr, OpLookup, OpMkdir, OpSymlink, OpSetAttr:
			e.putAttr(resp.Attr)
		case OpOpen, OpOpendir:
			e.putUint64(resp.HandleID)
		case OpRead:
			e.putBytes(resp.Data)
		case OpWrite:
			e.putUint32(resp.Written)
		case OpReaddir:
			e.putUint16(uint16(len(resp.Entries)))
			for i := range resp.Entries {
				e.putDirEntry(&resp.Entries[i])
			}
		case OpCreate:
			e.putUint64(resp.HandleID)
			e.putAttr(resp.Attr)
		case OpReadlink:
			e.putStr(resp.Target)
		case OpStatfs:
			e.putUint64(resp.Blocks)
			e.putUint64(resp.Bfree)
			e.putUint64(resp.Bavail)
			e.putUint64(resp.Files)
			e.putUint64(resp.Ffree)
			e.putUint64(resp.Bsize)
			e.putUint64(resp.Namelen)
		}
	}

	return e.b
}

func decodeResponse(data []byte) (*Response, error) {
	d := decoder{b: data}
	resp := &Response{
		ID:    d.getUint32(),
		Op:    d.getByte(),
		Errno: d.getUint32(),
	}

	if resp.Errno == 0 {
		switch resp.Op {
		case OpGetAttr, OpLookup, OpMkdir, OpSymlink, OpSetAttr:
			resp.Attr = d.getAttr()
		case OpOpen, OpOpendir:
			resp.HandleID = d.getUint64()
		case OpRead:
			resp.Data = d.getBytes()
		case OpWrite:
			resp.Written = d.getUint32()
		case OpReaddir:
			count := int(d.getUint16())
			resp.Entries = make([]DirEntry, count)
			for i := 0; i < count; i++ {
				resp.Entries[i] = d.getDirEntry()
			}
		case OpCreate:
			resp.HandleID = d.getUint64()
			resp.Attr = d.getAttr()
		case OpReadlink:
			resp.Target = d.getStr()
		case OpStatfs:
			resp.Blocks = d.getUint64()
			resp.Bfree = d.getUint64()
			resp.Bavail = d.getUint64()
			resp.Files = d.getUint64()
			resp.Ffree = d.getUint64()
			resp.Bsize = d.getUint64()
			resp.Namelen = d.getUint64()
		}
	}

	if d.err != nil {
		return nil, d.err
	}
	return resp, nil
}

// ---------- Conn ----------

// Conn wraps a connection for sending and receiving wire protocol messages.
type Conn struct {
	rwc     io.ReadWriteCloser
	reader  *bufio.Reader
	writeMu sync.Mutex
}

// NewConn wraps the given connection for wire protocol communication.
func NewConn(rwc io.ReadWriteCloser) *Conn {
	return &Conn{
		rwc:    rwc,
		reader: bufio.NewReaderSize(rwc, 256*1024),
	}
}

// Close closes the underlying connection.
func (c *Conn) Close() error {
	return c.rwc.Close()
}

func (c *Conn) writeFrame(data []byte) error {
	frame := make([]byte, 4, 4+len(data))
	binary.BigEndian.PutUint32(frame, uint32(len(data)))
	frame = append(frame, data...)

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.rwc.Write(frame)
	return err
}

func (c *Conn) readFrame() ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.reader, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(hdr[:])
	if length > 64*1024*1024 { // 64MB sanity limit
		return nil, errors.New("wire: frame too large")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(c.reader, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// SendRequest sends a request message.
func (c *Conn) SendRequest(req *Request) error {
	return c.writeFrame(encodeRequest(req))
}

// RecvRequest reads and decodes a request message.
func (c *Conn) RecvRequest() (*Request, error) {
	data, err := c.readFrame()
	if err != nil {
		return nil, err
	}
	return decodeRequest(data)
}

// SendResponse sends a response message.
func (c *Conn) SendResponse(resp *Response) error {
	return c.writeFrame(encodeResponse(resp))
}

// RecvResponse reads and decodes a response message.
func (c *Conn) RecvResponse() (*Response, error) {
	data, err := c.readFrame()
	if err != nil {
		return nil, err
	}
	return decodeResponse(data)
}
