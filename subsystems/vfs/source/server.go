// Package source provides a plain-TCP server that serves a single export backend.
package source

import (
	"context"
	"io"
	"log"
	"net"

	"github.com/cyber-shuttle/linkspan/subsystems/vfs/export"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/wire"
)

// Server serves file operations from a single export backend over the wire protocol.
type Server struct {
	backend *export.Backend
}

// NewServer creates a server that will serve file operations from backend.
func NewServer(backend *export.Backend) *Server {
	return &Server{backend: backend}
}

// HandleConn handles a single client connection, running the request/response loop
// until the connection is closed or an error occurs.
func (s *Server) HandleConn(c net.Conn) {
	wc := wire.NewConn(c)
	defer wc.Close()
	ctx := context.Background()
	for {
		req, err := wc.RecvRequest()
		if err != nil {
			if err != io.EOF {
				log.Printf("source: recv error: %v", err)
			}
			return
		}
		resp := s.backend.HandleRequest(ctx, req)
		resp.ID = req.ID
		resp.Op = req.Op
		if err := wc.SendResponse(resp); err != nil {
			log.Printf("source: send error: %v", err)
			return
		}
	}
}

// Run accepts connections from lis and handles each in its own goroutine. It
// returns when the listener is closed or another accept error occurs.
func Run(ctx context.Context, lis net.Listener, srv *Server) error {
	for {
		c, err := lis.Accept()
		if err != nil {
			return err
		}
		go srv.HandleConn(c)
	}
}

// RunContext accepts connections from lis until stopCh is closed, then closes
// the listener and returns. Each accepted connection is handled in its own goroutine.
func RunContext(stopCh <-chan struct{}, lis net.Listener, srv *Server) error {
	go func() {
		<-stopCh
		lis.Close()
	}()
	return Run(context.Background(), lis, srv)
}
