// Package fileproto provides the wire protocol client for request/response file operations.
package fileproto

import (
	"context"
	"sync"

	"github.com/cyber-shuttle/linkspan/subsystems/vfs/wire"
)

// Client wraps a wire.Conn and provides request/response matching by request ID.
type Client struct {
	conn     *wire.Conn
	mu       sync.Mutex
	pending  map[uint32]chan *wire.Response
	nextID   uint32
	closed   bool
	recvDone chan struct{}
}

// NewClient creates a client that uses the given wire.Conn. Call Run() in a goroutine to dispatch responses.
func NewClient(conn *wire.Conn) *Client {
	return &Client{
		conn:     conn,
		pending:  make(map[uint32]chan *wire.Response),
		recvDone: make(chan struct{}),
	}
}

// Run receives responses from the connection and dispatches them to waiters. Call once in a goroutine.
// Returns when RecvResponse fails or the connection is closed.
func (c *Client) Run() {
	for {
		resp, err := c.conn.RecvResponse()
		if err != nil {
			break
		}
		c.mu.Lock()
		ch := c.pending[resp.ID]
		delete(c.pending, resp.ID)
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- resp:
			default:
				// waiter gave up
			}
		}
	}
	c.mu.Lock()
	c.closed = true
	for _, ch := range c.pending {
		close(ch)
	}
	c.pending = make(map[uint32]chan *wire.Response)
	c.mu.Unlock()
	close(c.recvDone)
}

// Do sends the request and blocks until the response with the matching ID is received.
func (c *Client) Do(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, wire.ErrClosed
	}
	c.nextID++
	req.ID = c.nextID
	reqID := req.ID
	ch := make(chan *wire.Response, 1)
	c.pending[reqID] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
	}()

	if err := c.conn.SendRequest(req); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp == nil {
			return nil, wire.ErrClosed
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.recvDone:
		return nil, wire.ErrClosed
	}
}

// Close marks the client closed and unblocks all waiters.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	for _, ch := range c.pending {
		close(ch)
	}
	c.pending = make(map[uint32]chan *wire.Response)
	c.mu.Unlock()
}
