package logstream

import (
	"io"
	"log"
	"net"
	"sync"
)

// Broadcaster captures log output and fans it out to connected TCP clients.
// It implements io.Writer so it can be installed via log.SetOutput.
type Broadcaster struct {
	mu      sync.RWMutex
	clients map[net.Conn]struct{}
	dest    io.Writer // original destination (os.Stderr)
}

// New creates a Broadcaster that tees to dest and any connected TCP clients.
func New(dest io.Writer) *Broadcaster {
	return &Broadcaster{
		clients: make(map[net.Conn]struct{}),
		dest:    dest,
	}
}

// Write implements io.Writer. Every log line is written to the original
// destination AND broadcast to all connected clients.
func (b *Broadcaster) Write(p []byte) (int, error) {
	n, err := b.dest.Write(p)

	b.mu.RLock()
	for c := range b.clients {
		_, writeErr := c.Write(p)
		if writeErr != nil {
			// Mark for removal but don't modify map during iteration
			go b.remove(c)
		}
	}
	b.mu.RUnlock()

	return n, err
}

func (b *Broadcaster) remove(c net.Conn) {
	b.mu.Lock()
	delete(b.clients, c)
	b.mu.Unlock()
	c.Close()
}

// ListenAndServe starts a TCP listener on addr and accepts clients that
// receive the log stream. Returns the listener so the caller can retrieve
// the bound port.
func (b *Broadcaster) ListenAndServe(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			b.mu.Lock()
			b.clients[conn] = struct{}{}
			b.mu.Unlock()
			log.Printf("[logstream] client connected from %s", conn.RemoteAddr())

			// Drain reads so we detect client disconnect
			go func(c net.Conn) {
				buf := make([]byte, 256)
				for {
					if _, err := c.Read(buf); err != nil {
						b.remove(c)
						return
					}
				}
			}(conn)
		}
	}()

	return ln, nil
}

// Install redirects the standard logger to this broadcaster.
func (b *Broadcaster) Install() {
	log.SetOutput(b)
}

// Close disconnects all clients.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	for c := range b.clients {
		c.Close()
		delete(b.clients, c)
	}
	b.mu.Unlock()
}
