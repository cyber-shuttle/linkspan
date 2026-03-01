package fuse

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
)

// FuseError is returned by the client when the server responds with a non-OK
// status. Errno holds the raw Status value from the server.
type FuseError struct {
	Errno uint32
}

// Error implements the error interface.
func (e *FuseError) Error() string {
	return fmt.Sprintf("fuse: remote errno %d", e.Errno)
}

// Client is a TCP FUSE client. It maintains a single TCP connection to a
// remote FUSE server and serialises all request/response pairs using an
// internal mutex.
type Client struct {
	conn  net.Conn
	mu    sync.Mutex
	reqID atomic.Uint32
}

// NewClient dials addr and returns a connected Client. The caller must call
// Close when done to release the underlying TCP connection.
func NewClient(addr string) (*Client, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("fuse: connect to %s: %w", addr, err)
	}
	return &Client{conn: conn}, nil
}

// Close closes the underlying TCP connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// do sends a single request and returns the decoded response. The mutex
// ensures that concurrent callers do not interleave their frames.
func (c *Client) do(op Opcode, payload []byte) (*Response, error) {
	id := c.reqID.Add(1)

	req := &Request{
		ReqID:   id,
		Op:      op,
		Payload: payload,
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := WriteRequest(c.conn, req); err != nil {
		return nil, fmt.Errorf("fuse: write request (op=%d): %w", op, err)
	}

	resp, err := ReadResponse(c.conn)
	if err != nil {
		return nil, fmt.Errorf("fuse: read response (op=%d): %w", op, err)
	}

	if resp.ReqID != id {
		return nil, fmt.Errorf("fuse: response req_id mismatch: got %d, want %d", resp.ReqID, id)
	}

	return resp, nil
}

// checkStatus returns nil when resp carries StatusOK, otherwise it wraps the
// status code in a FuseError.
func checkStatus(resp *Response) error {
	if resp.Status == StatusOK {
		return nil
	}
	return &FuseError{Errno: uint32(resp.Status)}
}

// GetAttr retrieves file attributes for path.
func (c *Client) GetAttr(path string) (*AttrInfo, error) {
	resp, err := c.do(OpGetAttr, EncodePathPayload(path))
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	attr, err := DecodeAttrInfo(resp.Payload)
	if err != nil {
		return nil, fmt.Errorf("fuse: decode AttrInfo: %w", err)
	}
	return &attr, nil
}

// ReadDir returns the directory entries for path.
func (c *Client) ReadDir(path string) ([]DirEntry, error) {
	resp, err := c.do(OpReadDir, EncodePathPayload(path))
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	entries, err := DecodeDirEntries(resp.Payload)
	if err != nil {
		return nil, fmt.Errorf("fuse: decode DirEntries: %w", err)
	}
	return entries, nil
}

// Read reads size bytes from path starting at offset.
func (c *Client) Read(path string, offset uint64, size uint32) ([]byte, error) {
	payload := EncodeReadReq(ReadReq{
		Path:   path,
		Offset: int64(offset),
		Size:   size,
	})
	resp, err := c.do(OpRead, payload)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	return resp.Payload, nil
}

// Write writes data to path at offset and returns the number of bytes written.
func (c *Client) Write(path string, offset uint64, data []byte) (int, error) {
	payload := EncodeWriteReq(WriteReq{
		Path:   path,
		Offset: int64(offset),
		Data:   data,
	})
	resp, err := c.do(OpWrite, payload)
	if err != nil {
		return 0, err
	}
	if err := checkStatus(resp); err != nil {
		return 0, err
	}
	if len(resp.Payload) < 4 {
		return 0, fmt.Errorf("fuse: write response payload too short (%d bytes)", len(resp.Payload))
	}
	n := int(binary.BigEndian.Uint32(resp.Payload[0:4]))
	return n, nil
}

// Create creates a new file at path with the given mode.
func (c *Client) Create(path string, mode os.FileMode) error {
	payload := EncodeCreateReq(CreateReq{
		Path:  path,
		Mode:  uint32(mode),
		Flags: 0,
	})
	resp, err := c.do(OpCreate, payload)
	if err != nil {
		return err
	}
	return checkStatus(resp)
}

// Mkdir creates a new directory at path with the given mode.
func (c *Client) Mkdir(path string, mode os.FileMode) error {
	payload := EncodeMkdirReq(MkdirReq{
		Path: path,
		Mode: uint32(mode),
	})
	resp, err := c.do(OpMkdir, payload)
	if err != nil {
		return err
	}
	return checkStatus(resp)
}

// Unlink removes the file at path.
func (c *Client) Unlink(path string) error {
	resp, err := c.do(OpUnlink, EncodePathPayload(path))
	if err != nil {
		return err
	}
	return checkStatus(resp)
}

// Rmdir removes the directory at path.
func (c *Client) Rmdir(path string) error {
	resp, err := c.do(OpRmdir, EncodePathPayload(path))
	if err != nil {
		return err
	}
	return checkStatus(resp)
}

// Rename renames oldPath to newPath.
func (c *Client) Rename(oldPath, newPath string) error {
	payload := EncodeRenameReq(RenameReq{
		OldPath: oldPath,
		NewPath: newPath,
	})
	resp, err := c.do(OpRename, payload)
	if err != nil {
		return err
	}
	return checkStatus(resp)
}

// StatFS returns filesystem statistics from the server.
func (c *Client) StatFS() (*StatFSInfo, error) {
	resp, err := c.do(OpStatFS, nil)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	info, err := DecodeStatFSInfo(resp.Payload)
	if err != nil {
		return nil, fmt.Errorf("fuse: decode StatFSInfo: %w", err)
	}
	return &info, nil
}
