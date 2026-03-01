package fuse

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Server is a TCP FUSE server that translates binary FUSE requests into real
// filesystem operations rooted at a given directory.
type Server struct {
	root string
	mu   sync.Mutex
	ln   net.Listener
}

// NewServer creates a new Server rooted at the given directory.
// All paths received from clients are resolved relative to root.
func NewServer(root string) *Server {
	abs, err := filepath.Abs(root)
	if err != nil {
		// Fallback: use root as-is; resolvePath will enforce containment.
		abs = root
	}
	return &Server{root: abs}
}

// Serve accepts connections from ln and handles each in its own goroutine.
// It blocks until the listener is closed or an accept error occurs.
// The caller is responsible for closing ln or calling Stop().
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

// Stop closes the listener, causing Serve to return.
func (s *Server) Stop() {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
}

// handleConn runs the read → dispatch → write loop for one connection.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	for {
		req, err := ReadRequest(conn)
		if err != nil {
			// io.EOF and io.ErrUnexpectedEOF both indicate a clean client disconnect;
			// only log genuinely unexpected errors.
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				log.Printf("fuse server: read request: %v", err)
			}
			return
		}

		resp := s.dispatch(req)
		if err := WriteResponse(conn, resp); err != nil {
			log.Printf("fuse server: write response: %v", err)
			return
		}
	}
}

// dispatch routes a request to the appropriate handler and returns a response.
func (s *Server) dispatch(req *Request) *Response {
	switch req.Op {
	case OpGetAttr:
		return s.handleGetAttr(req)
	case OpReadDir, OpOpenDir:
		return s.handleReadDir(req)
	case OpRead:
		return s.handleRead(req)
	case OpWrite:
		return s.handleWrite(req)
	case OpCreate:
		return s.handleCreate(req)
	case OpMkdir:
		return s.handleMkdir(req)
	case OpUnlink:
		return s.handleUnlink(req)
	case OpRmdir:
		return s.handleRmdir(req)
	case OpRename:
		return s.handleRename(req)
	case OpStatFS:
		return s.handleStatFS(req)
	default:
		return errorResp(req.ReqID, EINVAL)
	}
}

// resolvePath cleans relPath and returns the absolute path within the server
// root, rejecting any attempts to escape via ".." components.
func (s *Server) resolvePath(relPath string) (string, error) {
	// filepath.Join cleans the path (resolves "..", "."), giving us an absolute
	// path that is either inside root or outside it.
	abs := filepath.Join(s.root, filepath.FromSlash(relPath))
	abs = filepath.Clean(abs)

	// Ensure the resulting path is within root. We add a trailing separator to
	// root so that a path equal to root itself is also accepted.
	rootWithSep := s.root
	if !strings.HasSuffix(rootWithSep, string(filepath.Separator)) {
		rootWithSep += string(filepath.Separator)
	}

	if abs != s.root && !strings.HasPrefix(abs, rootWithSep) {
		return "", fmt.Errorf("fuse: path %q escapes server root", relPath)
	}
	return abs, nil
}

// mapOSError converts common OS errors to FUSE Status codes.
func mapOSError(err error) Status {
	if err == nil {
		return StatusOK
	}
	if os.IsNotExist(err) {
		return ENOENT
	}
	if os.IsPermission(err) {
		return EACCES
	}
	return EIO
}

// errorResp constructs a Response containing only a non-OK status.
func errorResp(reqID uint32, status Status) *Response {
	return &Response{ReqID: reqID, Status: status}
}

// ---------------------------------------------------------------------------
// Operation handlers
// ---------------------------------------------------------------------------

func (s *Server) handleGetAttr(req *Request) *Response {
	relPath, err := DecodePathPayload(req.Payload)
	if err != nil {
		return errorResp(req.ReqID, EINVAL)
	}
	abs, err := s.resolvePath(relPath)
	if err != nil {
		return errorResp(req.ReqID, EACCES)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return errorResp(req.ReqID, mapOSError(err))
	}
	return &Response{
		ReqID:   req.ReqID,
		Status:  StatusOK,
		Payload: EncodeAttrInfo(AttrInfoFromFileInfo(fi)),
	}
}

func (s *Server) handleReadDir(req *Request) *Response {
	relPath, err := DecodePathPayload(req.Payload)
	if err != nil {
		return errorResp(req.ReqID, EINVAL)
	}
	abs, err := s.resolvePath(relPath)
	if err != nil {
		return errorResp(req.ReqID, EACCES)
	}
	osEntries, err := os.ReadDir(abs)
	if err != nil {
		return errorResp(req.ReqID, mapOSError(err))
	}
	entries := make([]DirEntry, 0, len(osEntries))
	for _, e := range osEntries {
		info, infoErr := e.Info()
		if infoErr != nil {
			continue
		}
		entries = append(entries, DirEntry{
			Name: e.Name(),
			Mode: GoModeToPOSIX(info.Mode()),
			Size: info.Size(),
		})
	}
	return &Response{
		ReqID:   req.ReqID,
		Status:  StatusOK,
		Payload: EncodeDirEntries(entries),
	}
}

func (s *Server) handleRead(req *Request) *Response {
	rr, err := DecodeReadReq(req.Payload)
	if err != nil {
		return errorResp(req.ReqID, EINVAL)
	}
	abs, err := s.resolvePath(rr.Path)
	if err != nil {
		return errorResp(req.ReqID, EACCES)
	}
	f, err := os.Open(abs)
	if err != nil {
		return errorResp(req.ReqID, mapOSError(err))
	}
	defer f.Close()

	buf := make([]byte, rr.Size)
	n, err := f.ReadAt(buf, rr.Offset)
	if err != nil && err != io.EOF {
		return errorResp(req.ReqID, mapOSError(err))
	}
	return &Response{
		ReqID:   req.ReqID,
		Status:  StatusOK,
		Payload: buf[:n],
	}
}

func (s *Server) handleWrite(req *Request) *Response {
	wr, err := DecodeWriteReq(req.Payload)
	if err != nil {
		return errorResp(req.ReqID, EINVAL)
	}
	abs, err := s.resolvePath(wr.Path)
	if err != nil {
		return errorResp(req.ReqID, EACCES)
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return errorResp(req.ReqID, mapOSError(err))
	}
	defer f.Close()

	n, err := f.WriteAt(wr.Data, wr.Offset)
	if err != nil {
		return errorResp(req.ReqID, mapOSError(err))
	}
	// Return number of bytes written as a 4-byte big-endian value.
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(n))
	return &Response{
		ReqID:   req.ReqID,
		Status:  StatusOK,
		Payload: payload,
	}
}

func (s *Server) handleCreate(req *Request) *Response {
	cr, err := DecodeCreateReq(req.Payload)
	if err != nil {
		return errorResp(req.ReqID, EINVAL)
	}
	abs, err := s.resolvePath(cr.Path)
	if err != nil {
		return errorResp(req.ReqID, EACCES)
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(cr.Mode))
	if err != nil {
		return errorResp(req.ReqID, mapOSError(err))
	}
	f.Close()
	return errorResp(req.ReqID, StatusOK)
}

func (s *Server) handleMkdir(req *Request) *Response {
	mr, err := DecodeMkdirReq(req.Payload)
	if err != nil {
		return errorResp(req.ReqID, EINVAL)
	}
	abs, err := s.resolvePath(mr.Path)
	if err != nil {
		return errorResp(req.ReqID, EACCES)
	}
	if err := os.Mkdir(abs, os.FileMode(mr.Mode)); err != nil {
		return errorResp(req.ReqID, mapOSError(err))
	}
	return errorResp(req.ReqID, StatusOK)
}

func (s *Server) handleUnlink(req *Request) *Response {
	relPath, err := DecodePathPayload(req.Payload)
	if err != nil {
		return errorResp(req.ReqID, EINVAL)
	}
	abs, err := s.resolvePath(relPath)
	if err != nil {
		return errorResp(req.ReqID, EACCES)
	}
	if err := os.Remove(abs); err != nil {
		return errorResp(req.ReqID, mapOSError(err))
	}
	return errorResp(req.ReqID, StatusOK)
}

func (s *Server) handleRmdir(req *Request) *Response {
	relPath, err := DecodePathPayload(req.Payload)
	if err != nil {
		return errorResp(req.ReqID, EINVAL)
	}
	abs, err := s.resolvePath(relPath)
	if err != nil {
		return errorResp(req.ReqID, EACCES)
	}
	if err := os.Remove(abs); err != nil {
		return errorResp(req.ReqID, mapOSError(err))
	}
	return errorResp(req.ReqID, StatusOK)
}

func (s *Server) handleRename(req *Request) *Response {
	rr, err := DecodeRenameReq(req.Payload)
	if err != nil {
		return errorResp(req.ReqID, EINVAL)
	}
	oldAbs, err := s.resolvePath(rr.OldPath)
	if err != nil {
		return errorResp(req.ReqID, EACCES)
	}
	newAbs, err := s.resolvePath(rr.NewPath)
	if err != nil {
		return errorResp(req.ReqID, EACCES)
	}
	if err := os.Rename(oldAbs, newAbs); err != nil {
		return errorResp(req.ReqID, mapOSError(err))
	}
	return errorResp(req.ReqID, StatusOK)
}

func (s *Server) handleStatFS(req *Request) *Response {
	info := StatFSInfo{
		Blocks: 1_000_000,
		Bfree:  500_000,
		Bavail: 450_000,
		Bsize:  4096,
	}
	return &Response{
		ReqID:   req.ReqID,
		Status:  StatusOK,
		Payload: EncodeStatFSInfo(info),
	}
}
