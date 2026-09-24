// Package link carries Linkspan's servers to cs-plane with no inbound port and no Dev Tunnel. Linkspan dials one
// WebSocket out to --link-url and holds it, redialing with backoff, and yamux multiplexes every stream over it with its
// own keepalive. For each stream cs-plane opens, Linkspan reads a two-byte port, answers one byte, 1 carried or 0
// refused, and joins the stream to that port through forward.Dial, so only a port a running task serves is reached;
// closing either end closes both. The socket offers cybershuttle.v1 and link.<token>, the token read from
// LINKSPAN_LINK_TOKEN so it never shows on a command line.
//
//	Link
//	carry
//	serve  One socket as yamux's transport, accepting streams until it ends; true once it connected.
//	New    Validates the URL and token, so main refuses them before binding.
//	Run    The task main starts; returns once its context is done.
package link

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"net/url"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/forward"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

const (
	Env      = "LINKSPAN_LINK_TOKEN"
	protocol = "cybershuttle.v1"
)

type Link struct {
	url    string
	dialer websocket.Dialer
}

func carry(stream net.Conn) {
	defer func() { _ = stream.Close() }()
	var port [2]byte
	if _, err := io.ReadFull(stream, port[:]); err != nil {
		return
	}
	server := forward.Dial(int(binary.BigEndian.Uint16(port[:])))
	if server == nil {
		_, _ = stream.Write([]byte{0})
		return
	}
	_, _ = stream.Write([]byte{1})
	go func() { _, _ = io.Copy(server, stream); _ = server.Close() }()
	_, _ = io.Copy(stream, server)
}

func (l *Link) serve(ctx context.Context) bool {
	ws, response, err := l.dialer.DialContext(ctx, l.url, nil)
	if response != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		log.Printf("link: %v", err)
		return false
	}
	local, remote := net.Pipe()
	go forward.Pipe(ws, remote)
	session, _ := yamux.Server(local, nil)
	defer func() { _ = session.Close() }()
	for {
		stream, err := session.AcceptStreamWithContext(ctx)
		if err != nil {
			log.Printf("link: %v", err)
			return true
		}
		go carry(stream)
	}
}

func New(rawURL, token string) (*Link, error) {
	if u, err := url.Parse(rawURL); err != nil || u.Host == "" || u.Scheme != "ws" && u.Scheme != "wss" || token == "" {
		return nil, errors.New("--link-url needs a ws or wss URL and " + Env)
	}
	return &Link{url: rawURL, dialer: websocket.Dialer{HandshakeTimeout: 30 * time.Second, Subprotocols: []string{protocol, "link." + token}}}, nil
}

func (l *Link) Run(ctx context.Context) error {
	for backoff := time.Second; ctx.Err() == nil; backoff = min(2*backoff, time.Minute) {
		if l.serve(ctx) {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
	}
	return nil
}
