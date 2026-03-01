// Package fuse implements the binary FUSE-over-TCP wire protocol used to proxy
// filesystem operations between a linkspan client (running in-kernel or in
// userspace via go-fuse) and a linkspan agent running on a remote host.
//
// Wire format (big-endian):
//
//	Request:  [msg_len:4B][req_id:4B][opcode:4B][payload:var]
//	Response: [msg_len:4B][req_id:4B][status:4B][payload:var]
//
// msg_len is the total byte length of the frame including the header fields.
package fuse

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrShortPayload is returned when a payload is too short to decode.
var ErrShortPayload = errors.New("fuse: short payload")

// maxPayloadSize is a sanity limit to prevent runaway allocations.
const maxPayloadSize = 16 * 1024 * 1024 // 16 MiB

// headerSize is the fixed byte count of the non-payload header fields.
// msg_len(4) + req_id(4) + opcode/status(4) = 12 bytes.
const headerSize = 12

// Opcode identifies the filesystem operation being requested.
type Opcode uint32

// Linux FUSE opcode subset used over the TCP wire.
const (
	OpGetAttr  Opcode = 3
	OpMkdir    Opcode = 9
	OpUnlink   Opcode = 10
	OpRmdir    Opcode = 11
	OpRename   Opcode = 12
	OpOpen     Opcode = 14
	OpRead     Opcode = 15
	OpWrite    Opcode = 16
	OpStatFS   Opcode = 17
	OpOpenDir  Opcode = 27
	OpReadDir  Opcode = 28
	OpCreate   Opcode = 35
)

// Status values mirror POSIX errno constants so that the agent can return
// meaningful error codes to the kernel.
type Status uint32

const (
	StatusOK     Status = 0
	ENOENT       Status = 2
	EIO          Status = 5
	EACCES       Status = 13
	EEXIST       Status = 17
	ENOTDIR      Status = 20
	EISDIR       Status = 21
	EINVAL       Status = 22
	ENOSPC       Status = 28
)

// Request is a deserialized FUSE request frame.
type Request struct {
	// ReqID uniquely identifies the in-flight request.
	ReqID uint32
	// Op is the filesystem operation being requested.
	Op Opcode
	// Payload holds the operation-specific encoded bytes.
	Payload []byte
}

// Response is a deserialized FUSE response frame.
type Response struct {
	// ReqID matches the request this response is answering.
	ReqID uint32
	// Status is StatusOK on success or an errno constant on failure.
	Status Status
	// Payload holds the operation-specific encoded bytes (may be nil on error).
	Payload []byte
}

// WriteRequest encodes req into w using the binary wire format.
func WriteRequest(w io.Writer, req *Request) error {
	payloadLen := len(req.Payload)
	msgLen := uint32(headerSize + payloadLen)

	buf := make([]byte, msgLen)
	binary.BigEndian.PutUint32(buf[0:4], msgLen)
	binary.BigEndian.PutUint32(buf[4:8], req.ReqID)
	binary.BigEndian.PutUint32(buf[8:12], uint32(req.Op))
	if payloadLen > 0 {
		copy(buf[12:], req.Payload)
	}

	_, err := w.Write(buf)
	return err
}

// ReadRequest reads and decodes one Request frame from r.
func ReadRequest(r io.Reader) (*Request, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("fuse: read request header: %w", err)
	}

	msgLen := binary.BigEndian.Uint32(header[0:4])
	if msgLen < headerSize {
		return nil, fmt.Errorf("fuse: msg_len %d is smaller than header size %d", msgLen, headerSize)
	}

	payloadLen := int(msgLen) - headerSize
	if payloadLen > maxPayloadSize {
		return nil, fmt.Errorf("fuse: payload size %d exceeds limit %d", payloadLen, maxPayloadSize)
	}

	req := &Request{
		ReqID:   binary.BigEndian.Uint32(header[4:8]),
		Op:      Opcode(binary.BigEndian.Uint32(header[8:12])),
		Payload: nil,
	}

	if payloadLen > 0 {
		req.Payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(r, req.Payload); err != nil {
			return nil, fmt.Errorf("fuse: read request payload: %w", err)
		}
	}

	return req, nil
}

// WriteResponse encodes resp into w using the binary wire format.
func WriteResponse(w io.Writer, resp *Response) error {
	payloadLen := len(resp.Payload)
	msgLen := uint32(headerSize + payloadLen)

	buf := make([]byte, msgLen)
	binary.BigEndian.PutUint32(buf[0:4], msgLen)
	binary.BigEndian.PutUint32(buf[4:8], resp.ReqID)
	binary.BigEndian.PutUint32(buf[8:12], uint32(resp.Status))
	if payloadLen > 0 {
		copy(buf[12:], resp.Payload)
	}

	_, err := w.Write(buf)
	return err
}

// ReadResponse reads and decodes one Response frame from r.
func ReadResponse(r io.Reader) (*Response, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("fuse: read response header: %w", err)
	}

	msgLen := binary.BigEndian.Uint32(header[0:4])
	if msgLen < headerSize {
		return nil, fmt.Errorf("fuse: msg_len %d is smaller than header size %d", msgLen, headerSize)
	}

	payloadLen := int(msgLen) - headerSize
	if payloadLen > maxPayloadSize {
		return nil, fmt.Errorf("fuse: payload size %d exceeds limit %d", payloadLen, maxPayloadSize)
	}

	resp := &Response{
		ReqID:   binary.BigEndian.Uint32(header[4:8]),
		Status:  Status(binary.BigEndian.Uint32(header[8:12])),
		Payload: nil,
	}

	if payloadLen > 0 {
		resp.Payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(r, resp.Payload); err != nil {
			return nil, fmt.Errorf("fuse: read response payload: %w", err)
		}
	}

	return resp, nil
}

// AttrInfo holds the file attributes returned by GetAttr.
// Wire layout: mode(4B) + size(8B) + atime(8B) + mtime(8B) + ctime(8B) = 36 bytes.
type AttrInfo struct {
	Mode  uint32
	Size  int64
	Atime int64
	Mtime int64
	Ctime int64
}

// attrInfoSize is the fixed encoded size of AttrInfo.
const attrInfoSize = 4 + 8 + 8 + 8 + 8 // 36 bytes

// EncodeAttrInfo serialises a into a 36-byte slice.
func EncodeAttrInfo(a AttrInfo) []byte {
	buf := make([]byte, attrInfoSize)
	binary.BigEndian.PutUint32(buf[0:4], a.Mode)
	binary.BigEndian.PutUint64(buf[4:12], uint64(a.Size))
	binary.BigEndian.PutUint64(buf[12:20], uint64(a.Atime))
	binary.BigEndian.PutUint64(buf[20:28], uint64(a.Mtime))
	binary.BigEndian.PutUint64(buf[28:36], uint64(a.Ctime))
	return buf
}

// DecodeAttrInfo deserialises an AttrInfo from buf.
func DecodeAttrInfo(buf []byte) (AttrInfo, error) {
	if len(buf) < attrInfoSize {
		return AttrInfo{}, ErrShortPayload
	}
	return AttrInfo{
		Mode:  binary.BigEndian.Uint32(buf[0:4]),
		Size:  int64(binary.BigEndian.Uint64(buf[4:12])),
		Atime: int64(binary.BigEndian.Uint64(buf[12:20])),
		Mtime: int64(binary.BigEndian.Uint64(buf[20:28])),
		Ctime: int64(binary.BigEndian.Uint64(buf[28:36])),
	}, nil
}

// GoModeToPOSIX converts a Go os.FileMode value to a POSIX-style mode_t.
// Go uses its own bit layout (e.g. ModeDir = 1<<31) which differs from the
// POSIX convention (S_IFDIR = 0o040000). The wire protocol uses POSIX mode
// so that clients in any language can interpret file-type bits consistently.
func GoModeToPOSIX(m os.FileMode) uint32 {
	// Permission bits (lower 9) are the same in both representations.
	perm := uint32(m.Perm())

	// Map Go type bits to POSIX S_IF* constants.
	var typeBits uint32
	switch {
	case m.IsDir():
		typeBits = 0o040000 // S_IFDIR
	case m&os.ModeSymlink != 0:
		typeBits = 0o120000 // S_IFLNK
	case m&os.ModeNamedPipe != 0:
		typeBits = 0o010000 // S_IFIFO
	case m&os.ModeSocket != 0:
		typeBits = 0o140000 // S_IFSOCK
	case m&os.ModeDevice != 0 && m&os.ModeCharDevice != 0:
		typeBits = 0o020000 // S_IFCHR
	case m&os.ModeDevice != 0:
		typeBits = 0o060000 // S_IFBLK
	default:
		typeBits = 0o100000 // S_IFREG
	}

	// Setuid/setgid/sticky.
	if m&os.ModeSetuid != 0 {
		perm |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		perm |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		perm |= 0o1000
	}

	return typeBits | perm
}

// AttrInfoFromFileInfo creates an AttrInfo from a standard os.FileInfo value.
// Timestamps are expressed as Unix seconds (nanoseconds are truncated).
// The Mode field is encoded in POSIX format (see GoModeToPOSIX).
func AttrInfoFromFileInfo(fi os.FileInfo) AttrInfo {
	return AttrInfo{
		Mode:  GoModeToPOSIX(fi.Mode()),
		Size:  fi.Size(),
		Atime: fi.ModTime().Unix(), // os.FileInfo has no atime; use mtime as approximation
		Mtime: fi.ModTime().Unix(),
		Ctime: fi.ModTime().Unix(),
	}
}
