package fuse

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// encodePath writes path as a null-terminated byte sequence into buf,
// returning the total bytes written (len(path)+1).
func encodePath(path string) []byte {
	b := make([]byte, len(path)+1)
	copy(b, path)
	// b[len(path)] is already 0 (null terminator)
	return b
}

// readNullTerminated reads a null-terminated string from buf starting at
// offset pos. It returns the string and the position immediately after the
// null terminator. Returns an error if no null byte is found.
func readNullTerminated(buf []byte, pos int) (string, int, error) {
	end := bytes.IndexByte(buf[pos:], 0)
	if end < 0 {
		return "", pos, fmt.Errorf("fuse: missing null terminator in payload at offset %d", pos)
	}
	s := string(buf[pos : pos+end])
	return s, pos + end + 1, nil
}

// ---------------------------------------------------------------------------
// Path-only payloads (GetAttr, Unlink, Rmdir, OpenDir, ReadDir, Open)
// ---------------------------------------------------------------------------

// EncodePathPayload encodes a single null-terminated path as a payload.
func EncodePathPayload(path string) []byte {
	return encodePath(path)
}

// DecodePathPayload decodes a single null-terminated path from payload.
func DecodePathPayload(payload []byte) (string, error) {
	path, _, err := readNullTerminated(payload, 0)
	return path, err
}

// ---------------------------------------------------------------------------
// ReadReq — path\0 + offset(8B) + size(4B)
// ---------------------------------------------------------------------------

// ReadReq holds the parameters of a read request.
type ReadReq struct {
	Path   string
	Offset int64
	Size   uint32
}

// EncodeReadReq encodes a ReadReq into a payload byte slice.
func EncodeReadReq(r ReadReq) []byte {
	pathBytes := encodePath(r.Path)
	buf := make([]byte, len(pathBytes)+8+4)
	copy(buf, pathBytes)
	pos := len(pathBytes)
	binary.BigEndian.PutUint64(buf[pos:pos+8], uint64(r.Offset))
	binary.BigEndian.PutUint32(buf[pos+8:pos+12], r.Size)
	return buf
}

// DecodeReadReq decodes a ReadReq from payload.
func DecodeReadReq(payload []byte) (ReadReq, error) {
	path, pos, err := readNullTerminated(payload, 0)
	if err != nil {
		return ReadReq{}, err
	}
	if len(payload)-pos < 12 {
		return ReadReq{}, ErrShortPayload
	}
	return ReadReq{
		Path:   path,
		Offset: int64(binary.BigEndian.Uint64(payload[pos : pos+8])),
		Size:   binary.BigEndian.Uint32(payload[pos+8 : pos+12]),
	}, nil
}

// ---------------------------------------------------------------------------
// WriteReq — path\0 + offset(8B) + size(4B) + raw bytes
// ---------------------------------------------------------------------------

// WriteReq holds the parameters of a write request.
type WriteReq struct {
	Path   string
	Offset int64
	Data   []byte
}

// EncodeWriteReq encodes a WriteReq into a payload byte slice.
func EncodeWriteReq(w WriteReq) []byte {
	pathBytes := encodePath(w.Path)
	dataLen := len(w.Data)
	buf := make([]byte, len(pathBytes)+8+4+dataLen)
	copy(buf, pathBytes)
	pos := len(pathBytes)
	binary.BigEndian.PutUint64(buf[pos:pos+8], uint64(w.Offset))
	binary.BigEndian.PutUint32(buf[pos+8:pos+12], uint32(dataLen))
	copy(buf[pos+12:], w.Data)
	return buf
}

// DecodeWriteReq decodes a WriteReq from payload.
func DecodeWriteReq(payload []byte) (WriteReq, error) {
	path, pos, err := readNullTerminated(payload, 0)
	if err != nil {
		return WriteReq{}, err
	}
	if len(payload)-pos < 12 {
		return WriteReq{}, ErrShortPayload
	}
	offset := int64(binary.BigEndian.Uint64(payload[pos : pos+8]))
	size := int(binary.BigEndian.Uint32(payload[pos+8 : pos+12]))
	pos += 12
	if len(payload)-pos < size {
		return WriteReq{}, ErrShortPayload
	}
	data := make([]byte, size)
	copy(data, payload[pos:pos+size])
	return WriteReq{
		Path:   path,
		Offset: offset,
		Data:   data,
	}, nil
}

// ---------------------------------------------------------------------------
// MkdirReq — path\0 + mode(4B)
// ---------------------------------------------------------------------------

// MkdirReq holds the parameters of a mkdir request.
type MkdirReq struct {
	Path string
	Mode uint32
}

// EncodeMkdirReq encodes a MkdirReq into a payload byte slice.
func EncodeMkdirReq(m MkdirReq) []byte {
	pathBytes := encodePath(m.Path)
	buf := make([]byte, len(pathBytes)+4)
	copy(buf, pathBytes)
	binary.BigEndian.PutUint32(buf[len(pathBytes):], m.Mode)
	return buf
}

// DecodeMkdirReq decodes a MkdirReq from payload.
func DecodeMkdirReq(payload []byte) (MkdirReq, error) {
	path, pos, err := readNullTerminated(payload, 0)
	if err != nil {
		return MkdirReq{}, err
	}
	if len(payload)-pos < 4 {
		return MkdirReq{}, ErrShortPayload
	}
	return MkdirReq{
		Path: path,
		Mode: binary.BigEndian.Uint32(payload[pos : pos+4]),
	}, nil
}

// ---------------------------------------------------------------------------
// RenameReq — old_path\0 + new_path\0
// ---------------------------------------------------------------------------

// RenameReq holds the parameters of a rename request.
type RenameReq struct {
	OldPath string
	NewPath string
}

// EncodeRenameReq encodes a RenameReq into a payload byte slice.
func EncodeRenameReq(r RenameReq) []byte {
	old := encodePath(r.OldPath)
	new := encodePath(r.NewPath)
	buf := make([]byte, len(old)+len(new))
	copy(buf, old)
	copy(buf[len(old):], new)
	return buf
}

// DecodeRenameReq decodes a RenameReq from payload.
func DecodeRenameReq(payload []byte) (RenameReq, error) {
	oldPath, pos, err := readNullTerminated(payload, 0)
	if err != nil {
		return RenameReq{}, fmt.Errorf("fuse: decode RenameReq old_path: %w", err)
	}
	newPath, _, err := readNullTerminated(payload, pos)
	if err != nil {
		return RenameReq{}, fmt.Errorf("fuse: decode RenameReq new_path: %w", err)
	}
	return RenameReq{OldPath: oldPath, NewPath: newPath}, nil
}

// ---------------------------------------------------------------------------
// CreateReq — path\0 + mode(4B) + flags(4B)
// ---------------------------------------------------------------------------

// CreateReq holds the parameters of a create request.
type CreateReq struct {
	Path  string
	Mode  uint32
	Flags uint32
}

// EncodeCreateReq encodes a CreateReq into a payload byte slice.
func EncodeCreateReq(c CreateReq) []byte {
	pathBytes := encodePath(c.Path)
	buf := make([]byte, len(pathBytes)+4+4)
	copy(buf, pathBytes)
	pos := len(pathBytes)
	binary.BigEndian.PutUint32(buf[pos:pos+4], c.Mode)
	binary.BigEndian.PutUint32(buf[pos+4:pos+8], c.Flags)
	return buf
}

// DecodeCreateReq decodes a CreateReq from payload.
func DecodeCreateReq(payload []byte) (CreateReq, error) {
	path, pos, err := readNullTerminated(payload, 0)
	if err != nil {
		return CreateReq{}, err
	}
	if len(payload)-pos < 8 {
		return CreateReq{}, ErrShortPayload
	}
	return CreateReq{
		Path:  path,
		Mode:  binary.BigEndian.Uint32(payload[pos : pos+4]),
		Flags: binary.BigEndian.Uint32(payload[pos+4 : pos+8]),
	}, nil
}

// ---------------------------------------------------------------------------
// StatFSInfo — blocks(8B) + bfree(8B) + bavail(8B) + bsize(4B) = 28 bytes
// ---------------------------------------------------------------------------

// StatFSInfo holds filesystem statistics returned by StatFS.
type StatFSInfo struct {
	Blocks uint64
	Bfree  uint64
	Bavail uint64
	Bsize  uint32
}

// statFSInfoSize is the fixed encoded size of StatFSInfo.
const statFSInfoSize = 8 + 8 + 8 + 4 // 28 bytes

// EncodeStatFSInfo serialises s into a 28-byte slice.
func EncodeStatFSInfo(s StatFSInfo) []byte {
	buf := make([]byte, statFSInfoSize)
	binary.BigEndian.PutUint64(buf[0:8], s.Blocks)
	binary.BigEndian.PutUint64(buf[8:16], s.Bfree)
	binary.BigEndian.PutUint64(buf[16:24], s.Bavail)
	binary.BigEndian.PutUint32(buf[24:28], s.Bsize)
	return buf
}

// DecodeStatFSInfo deserialises a StatFSInfo from buf.
func DecodeStatFSInfo(buf []byte) (StatFSInfo, error) {
	if len(buf) < statFSInfoSize {
		return StatFSInfo{}, ErrShortPayload
	}
	return StatFSInfo{
		Blocks: binary.BigEndian.Uint64(buf[0:8]),
		Bfree:  binary.BigEndian.Uint64(buf[8:16]),
		Bavail: binary.BigEndian.Uint64(buf[16:24]),
		Bsize:  binary.BigEndian.Uint32(buf[24:28]),
	}, nil
}

// ---------------------------------------------------------------------------
// DirEntry — encoded as an array prefixed by entry count
//
// Each entry on the wire: name_len(2B) + name + mode(4B) + size(8B)
// Array frame:            count(4B) + [entry …]
// ---------------------------------------------------------------------------

// DirEntry represents a single directory entry.
type DirEntry struct {
	Name string
	Mode uint32
	Size int64
}

// EncodeDirEntries encodes a slice of DirEntry values as a contiguous payload.
// Wire layout: count(4B) followed by each entry as name_len(2B)+name+mode(4B)+size(8B).
func EncodeDirEntries(entries []DirEntry) []byte {
	// Calculate total size first to do a single allocation.
	total := 4 // count field
	for _, e := range entries {
		total += 2 + len(e.Name) + 4 + 8
	}

	buf := make([]byte, total)
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(entries)))
	pos := 4
	for _, e := range entries {
		nameLen := uint16(len(e.Name))
		binary.BigEndian.PutUint16(buf[pos:pos+2], nameLen)
		pos += 2
		copy(buf[pos:], e.Name)
		pos += len(e.Name)
		binary.BigEndian.PutUint32(buf[pos:pos+4], e.Mode)
		pos += 4
		binary.BigEndian.PutUint64(buf[pos:pos+8], uint64(e.Size))
		pos += 8
	}
	return buf
}

// DecodeDirEntries decodes a slice of DirEntry values from payload.
func DecodeDirEntries(payload []byte) ([]DirEntry, error) {
	if len(payload) < 4 {
		return nil, ErrShortPayload
	}
	count := int(binary.BigEndian.Uint32(payload[0:4]))
	entries := make([]DirEntry, 0, count)
	pos := 4
	for i := range count {
		if len(payload)-pos < 2 {
			return nil, fmt.Errorf("fuse: decode DirEntry[%d]: %w", i, ErrShortPayload)
		}
		nameLen := int(binary.BigEndian.Uint16(payload[pos : pos+2]))
		pos += 2
		if len(payload)-pos < nameLen+4+8 {
			return nil, fmt.Errorf("fuse: decode DirEntry[%d] body: %w", i, ErrShortPayload)
		}
		name := string(payload[pos : pos+nameLen])
		pos += nameLen
		mode := binary.BigEndian.Uint32(payload[pos : pos+4])
		pos += 4
		size := int64(binary.BigEndian.Uint64(payload[pos : pos+8]))
		pos += 8
		entries = append(entries, DirEntry{Name: name, Mode: mode, Size: size})
	}
	return entries, nil
}
