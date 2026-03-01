package fuse

import (
	"bytes"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Request round-trip
// ---------------------------------------------------------------------------

func TestRequestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		req  Request
	}{
		{
			name: "no payload",
			req:  Request{ReqID: 1, Op: OpGetAttr, Payload: nil},
		},
		{
			name: "with payload",
			req:  Request{ReqID: 42, Op: OpRead, Payload: []byte("hello world")},
		},
		{
			name: "max req_id",
			req:  Request{ReqID: 0xFFFFFFFF, Op: OpCreate, Payload: []byte{0x00, 0x01, 0x02}},
		},
		{
			name: "empty payload slice",
			req:  Request{ReqID: 7, Op: OpMkdir, Payload: []byte{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteRequest(&buf, &tt.req); err != nil {
				t.Fatalf("WriteRequest: %v", err)
			}

			got, err := ReadRequest(&buf)
			if err != nil {
				t.Fatalf("ReadRequest: %v", err)
			}

			if got.ReqID != tt.req.ReqID {
				t.Errorf("ReqID: got %d, want %d", got.ReqID, tt.req.ReqID)
			}
			if got.Op != tt.req.Op {
				t.Errorf("Op: got %d, want %d", got.Op, tt.req.Op)
			}
			wantPayload := tt.req.Payload
			if len(wantPayload) == 0 {
				wantPayload = nil
			}
			if !bytes.Equal(got.Payload, wantPayload) {
				t.Errorf("Payload: got %v, want %v", got.Payload, wantPayload)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Response round-trip
// ---------------------------------------------------------------------------

func TestResponseRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		resp Response
	}{
		{
			name: "ok no payload",
			resp: Response{ReqID: 1, Status: StatusOK, Payload: nil},
		},
		{
			name: "ok with payload",
			resp: Response{ReqID: 5, Status: StatusOK, Payload: []byte{0xDE, 0xAD, 0xBE, 0xEF}},
		},
		{
			name: "error status",
			resp: Response{ReqID: 99, Status: ENOENT, Payload: nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteResponse(&buf, &tt.resp); err != nil {
				t.Fatalf("WriteResponse: %v", err)
			}

			got, err := ReadResponse(&buf)
			if err != nil {
				t.Fatalf("ReadResponse: %v", err)
			}

			if got.ReqID != tt.resp.ReqID {
				t.Errorf("ReqID: got %d, want %d", got.ReqID, tt.resp.ReqID)
			}
			if got.Status != tt.resp.Status {
				t.Errorf("Status: got %d, want %d", got.Status, tt.resp.Status)
			}
			if !bytes.Equal(got.Payload, tt.resp.Payload) {
				t.Errorf("Payload: got %v, want %v", got.Payload, tt.resp.Payload)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Error status round-trip (all errno constants)
// ---------------------------------------------------------------------------

func TestErrorStatusRoundTrip(t *testing.T) {
	statuses := []Status{StatusOK, ENOENT, EIO, EACCES, EEXIST, ENOTDIR, EISDIR, EINVAL, ENOSPC}

	for _, st := range statuses {
		resp := &Response{ReqID: 1, Status: st}
		var buf bytes.Buffer
		if err := WriteResponse(&buf, resp); err != nil {
			t.Fatalf("WriteResponse status %d: %v", st, err)
		}
		got, err := ReadResponse(&buf)
		if err != nil {
			t.Fatalf("ReadResponse status %d: %v", st, err)
		}
		if got.Status != st {
			t.Errorf("status round-trip: got %d, want %d", got.Status, st)
		}
	}
}

// ---------------------------------------------------------------------------
// AttrInfo round-trip
// ---------------------------------------------------------------------------

func TestAttrInfoRoundTrip(t *testing.T) {
	want := AttrInfo{
		Mode:  0o755,
		Size:  1234567890,
		Atime: 1700000001,
		Mtime: 1700000002,
		Ctime: 1700000003,
	}

	enc := EncodeAttrInfo(want)
	if len(enc) != attrInfoSize {
		t.Fatalf("encoded length: got %d, want %d", len(enc), attrInfoSize)
	}

	got, err := DecodeAttrInfo(enc)
	if err != nil {
		t.Fatalf("DecodeAttrInfo: %v", err)
	}
	if got != want {
		t.Errorf("AttrInfo round-trip: got %+v, want %+v", got, want)
	}
}

func TestDecodeAttrInfoShort(t *testing.T) {
	_, err := DecodeAttrInfo([]byte{0x01, 0x02})
	if !errors.Is(err, ErrShortPayload) {
		t.Errorf("expected ErrShortPayload, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// AttrInfoFromFileInfo helper
// ---------------------------------------------------------------------------

// fakeFileInfo implements os.FileInfo for testing.
type fakeFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
}

func (f fakeFileInfo) Name() string      { return f.name }
func (f fakeFileInfo) Size() int64       { return f.size }
func (f fakeFileInfo) Mode() os.FileMode { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return f.modTime }
func (f fakeFileInfo) IsDir() bool       { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any          { return nil }

func TestAttrInfoFromFileInfo(t *testing.T) {
	mt := time.Unix(1700000042, 0)
	fi := fakeFileInfo{name: "test.txt", size: 512, mode: 0o644, modTime: mt}

	a := AttrInfoFromFileInfo(fi)
	wantMode := GoModeToPOSIX(fi.Mode())
	if a.Mode != wantMode {
		t.Errorf("Mode: got %o, want %o", a.Mode, wantMode)
	}
	if a.Size != fi.Size() {
		t.Errorf("Size: got %d, want %d", a.Size, fi.Size())
	}
	if a.Mtime != mt.Unix() {
		t.Errorf("Mtime: got %d, want %d", a.Mtime, mt.Unix())
	}
}

// ---------------------------------------------------------------------------
// DirEntries round-trip
// ---------------------------------------------------------------------------

func TestDirEntriesRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		entries []DirEntry
	}{
		{
			name:    "empty",
			entries: []DirEntry{},
		},
		{
			name: "single entry",
			entries: []DirEntry{
				{Name: "file.txt", Mode: 0o644, Size: 1024},
			},
		},
		{
			name: "multiple entries",
			entries: []DirEntry{
				{Name: ".", Mode: 0o755, Size: 4096},
				{Name: "..", Mode: 0o755, Size: 4096},
				{Name: "README.md", Mode: 0o644, Size: 2048},
				{Name: "subdir", Mode: 0o755 | uint32(os.ModeDir), Size: 4096},
			},
		},
		{
			name: "entry with long name",
			entries: []DirEntry{
				{Name: "a-very-long-filename-that-tests-the-name_len-field.txt", Mode: 0o600, Size: 0},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc := EncodeDirEntries(tt.entries)
			got, err := DecodeDirEntries(enc)
			if err != nil {
				t.Fatalf("DecodeDirEntries: %v", err)
			}
			if !reflect.DeepEqual(got, tt.entries) {
				t.Errorf("entries round-trip:\ngot  %+v\nwant %+v", got, tt.entries)
			}
		})
	}
}

func TestDecodeDirEntriesShort(t *testing.T) {
	_, err := DecodeDirEntries([]byte{0x00})
	if !errors.Is(err, ErrShortPayload) {
		t.Errorf("expected ErrShortPayload, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// ReadReq round-trip
// ---------------------------------------------------------------------------

func TestReadReqRoundTrip(t *testing.T) {
	want := ReadReq{
		Path:   "/home/alice/data.bin",
		Offset: 65536,
		Size:   4096,
	}

	enc := EncodeReadReq(want)
	got, err := DecodeReadReq(enc)
	if err != nil {
		t.Fatalf("DecodeReadReq: %v", err)
	}
	if got != want {
		t.Errorf("ReadReq round-trip: got %+v, want %+v", got, want)
	}
}

func TestDecodeReadReqShort(t *testing.T) {
	// A path with null terminator but no offset/size bytes.
	payload := []byte("/file\x00")
	_, err := DecodeReadReq(payload)
	if !errors.Is(err, ErrShortPayload) {
		t.Errorf("expected ErrShortPayload, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// WriteReq round-trip (including data)
// ---------------------------------------------------------------------------

func TestWriteReqRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		req  WriteReq
	}{
		{
			name: "empty data",
			req:  WriteReq{Path: "/tmp/out.bin", Offset: 0, Data: []byte{}},
		},
		{
			name: "with data",
			req:  WriteReq{Path: "/var/log/app.log", Offset: 1024, Data: []byte("log line\n")},
		},
		{
			name: "binary data",
			req:  WriteReq{Path: "/dev/null", Offset: 0, Data: []byte{0x00, 0xFF, 0xAB, 0xCD}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc := EncodeWriteReq(tt.req)
			got, err := DecodeWriteReq(enc)
			if err != nil {
				t.Fatalf("DecodeWriteReq: %v", err)
			}
			if got.Path != tt.req.Path {
				t.Errorf("Path: got %q, want %q", got.Path, tt.req.Path)
			}
			if got.Offset != tt.req.Offset {
				t.Errorf("Offset: got %d, want %d", got.Offset, tt.req.Offset)
			}
			if !bytes.Equal(got.Data, tt.req.Data) {
				t.Errorf("Data: got %v, want %v", got.Data, tt.req.Data)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MkdirReq round-trip
// ---------------------------------------------------------------------------

func TestMkdirReqRoundTrip(t *testing.T) {
	want := MkdirReq{Path: "/home/alice/new-dir", Mode: 0o755}
	enc := EncodeMkdirReq(want)
	got, err := DecodeMkdirReq(enc)
	if err != nil {
		t.Fatalf("DecodeMkdirReq: %v", err)
	}
	if got != want {
		t.Errorf("MkdirReq round-trip: got %+v, want %+v", got, want)
	}
}

// ---------------------------------------------------------------------------
// RenameReq round-trip
// ---------------------------------------------------------------------------

func TestRenameReqRoundTrip(t *testing.T) {
	want := RenameReq{OldPath: "/tmp/old.txt", NewPath: "/tmp/new.txt"}
	enc := EncodeRenameReq(want)
	got, err := DecodeRenameReq(enc)
	if err != nil {
		t.Fatalf("DecodeRenameReq: %v", err)
	}
	if got != want {
		t.Errorf("RenameReq round-trip: got %+v, want %+v", got, want)
	}
}

// ---------------------------------------------------------------------------
// CreateReq round-trip
// ---------------------------------------------------------------------------

func TestCreateReqRoundTrip(t *testing.T) {
	want := CreateReq{Path: "/home/alice/newfile.txt", Mode: 0o644, Flags: 0o2}
	enc := EncodeCreateReq(want)
	got, err := DecodeCreateReq(enc)
	if err != nil {
		t.Fatalf("DecodeCreateReq: %v", err)
	}
	if got != want {
		t.Errorf("CreateReq round-trip: got %+v, want %+v", got, want)
	}
}

// ---------------------------------------------------------------------------
// StatFSInfo round-trip
// ---------------------------------------------------------------------------

func TestStatFSInfoRoundTrip(t *testing.T) {
	want := StatFSInfo{Blocks: 1000000, Bfree: 500000, Bavail: 450000, Bsize: 4096}
	enc := EncodeStatFSInfo(want)
	if len(enc) != statFSInfoSize {
		t.Fatalf("encoded length: got %d, want %d", len(enc), statFSInfoSize)
	}
	got, err := DecodeStatFSInfo(enc)
	if err != nil {
		t.Fatalf("DecodeStatFSInfo: %v", err)
	}
	if got != want {
		t.Errorf("StatFSInfo round-trip: got %+v, want %+v", got, want)
	}
}

func TestDecodeStatFSInfoShort(t *testing.T) {
	_, err := DecodeStatFSInfo([]byte{0x00, 0x01, 0x02})
	if !errors.Is(err, ErrShortPayload) {
		t.Errorf("expected ErrShortPayload, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Path-only payload round-trip
// ---------------------------------------------------------------------------

func TestPathPayloadRoundTrip(t *testing.T) {
	paths := []string{
		"/",
		"/home/alice",
		"/very/deeply/nested/path/to/a/file.txt",
	}
	for _, p := range paths {
		enc := EncodePathPayload(p)
		got, err := DecodePathPayload(enc)
		if err != nil {
			t.Errorf("DecodePathPayload(%q): %v", p, err)
			continue
		}
		if got != p {
			t.Errorf("path round-trip: got %q, want %q", got, p)
		}
	}
}

// ---------------------------------------------------------------------------
// 16 MiB payload sanity limit
// ---------------------------------------------------------------------------

func TestMaxPayloadLimit(t *testing.T) {
	// Build a frame that claims a payload larger than the limit.
	// msg_len field = headerSize + maxPayloadSize + 1
	tooBig := uint32(headerSize + maxPayloadSize + 1)
	var header [headerSize]byte
	// Write a big-endian msg_len
	header[0] = byte(tooBig >> 24)
	header[1] = byte(tooBig >> 16)
	header[2] = byte(tooBig >> 8)
	header[3] = byte(tooBig)
	// req_id = 1
	header[7] = 1
	// opcode = OpRead
	header[11] = byte(OpRead)

	r := bytes.NewReader(header[:])
	_, err := ReadRequest(r)
	if err == nil {
		t.Fatal("expected error for oversized payload, got nil")
	}
}

func TestMaxPayloadLimitResponse(t *testing.T) {
	tooBig := uint32(headerSize + maxPayloadSize + 1)
	var header [headerSize]byte
	header[0] = byte(tooBig >> 24)
	header[1] = byte(tooBig >> 16)
	header[2] = byte(tooBig >> 8)
	header[3] = byte(tooBig)
	header[7] = 1

	r := bytes.NewReader(header[:])
	_, err := ReadResponse(r)
	if err == nil {
		t.Fatal("expected error for oversized response payload, got nil")
	}
}

// ---------------------------------------------------------------------------
// Sequential requests on a single stream
// ---------------------------------------------------------------------------

func TestMultipleRequestsOnStream(t *testing.T) {
	reqs := []Request{
		{ReqID: 1, Op: OpGetAttr, Payload: EncodePathPayload("/home/alice")},
		{ReqID: 2, Op: OpRead, Payload: EncodeReadReq(ReadReq{Path: "/tmp/x", Offset: 0, Size: 512})},
		{ReqID: 3, Op: OpMkdir, Payload: EncodeMkdirReq(MkdirReq{Path: "/tmp/newdir", Mode: 0o700})},
	}

	var buf bytes.Buffer
	for _, req := range reqs {
		r := req
		if err := WriteRequest(&buf, &r); err != nil {
			t.Fatalf("WriteRequest %d: %v", req.ReqID, err)
		}
	}

	for i, want := range reqs {
		got, err := ReadRequest(&buf)
		if err != nil {
			t.Fatalf("ReadRequest[%d]: %v", i, err)
		}
		if got.ReqID != want.ReqID || got.Op != want.Op {
			t.Errorf("req[%d]: got {%d %d}, want {%d %d}", i, got.ReqID, got.Op, want.ReqID, want.Op)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("req[%d] payload mismatch", i)
		}
	}
}

// ---------------------------------------------------------------------------
// EOF behaviour
// ---------------------------------------------------------------------------

func TestReadRequestEOF(t *testing.T) {
	r := bytes.NewReader(nil)
	_, err := ReadRequest(r)
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected EOF/ErrUnexpectedEOF, got %v", err)
	}
}

func TestReadResponseEOF(t *testing.T) {
	r := bytes.NewReader(nil)
	_, err := ReadResponse(r)
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected EOF/ErrUnexpectedEOF, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// WriteReq large payload (up to the limit)
// ---------------------------------------------------------------------------

func TestWriteReqLargeData(t *testing.T) {
	data := make([]byte, 1*1024*1024) // 1 MiB
	for i := range data {
		data[i] = byte(i % 256)
	}
	want := WriteReq{Path: "/tmp/big.bin", Offset: 0, Data: data}
	enc := EncodeWriteReq(want)
	got, err := DecodeWriteReq(enc)
	if err != nil {
		t.Fatalf("DecodeWriteReq large: %v", err)
	}
	if !bytes.Equal(got.Data, want.Data) {
		t.Error("WriteReq large data mismatch")
	}
}
