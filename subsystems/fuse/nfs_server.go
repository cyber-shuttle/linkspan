package fuse

import (
	"fmt"
	"log"
	"net"

	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// NFSServer wraps a go-nfs server that translates NFSv3 operations into FUSE
// TCP client calls. macOS's built-in mount_nfs connects to this server to
// mount a remote directory locally.
type NFSServer struct {
	client   *Client
	listener net.Listener
	port     int
	done     chan struct{}
}

// NewNFSServer creates an NFSServer backed by the given FUSE TCP client.
// Call Start to begin listening for NFS connections.
func NewNFSServer(client *Client) *NFSServer {
	return &NFSServer{
		client: client,
		done:   make(chan struct{}),
	}
}

// Start binds to a random local port and begins serving NFSv3 requests.
// The server runs in a background goroutine; call Stop to shut it down.
func (s *NFSServer) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("nfs: listen: %w", err)
	}
	s.listener = ln
	s.port = ln.Addr().(*net.TCPAddr).Port

	billyFS := newRemoteBillyFS(s.client)
	handler := nfshelper.NewNullAuthHandler(billyFS)
	cacheHandler := nfshelper.NewCachingHandler(handler, 1024)

	go func() {
		defer close(s.done)
		if err := nfs.Serve(ln, cacheHandler); err != nil {
			log.Printf("nfs: server error: %v", err)
		}
	}()

	log.Printf("nfs: NFSv3 proxy started on port %d", s.port)
	return nil
}

// Port returns the TCP port the NFS server is listening on.
func (s *NFSServer) Port() int {
	return s.port
}

// Stop shuts down the NFS server by closing the listener and waits for the
// serve goroutine to exit.
func (s *NFSServer) Stop() {
	if s.listener != nil {
		s.listener.Close()
		<-s.done
		s.listener = nil
	}
}
