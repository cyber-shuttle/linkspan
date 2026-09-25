// Package websocket carries Linkspan's servers to cs-plane with no inbound port and no Dev Tunnel. Linkspan dials one
// WebSocket out to its --url and holds it until it ends, and yamux multiplexes every stream over it with its
// own keepalive. For each stream cs-plane opens, Linkspan reads a two-byte port, answers one byte, 1 carried or 0
// refused, and joins the stream to that port through forward.Dial, so only a port a running task serves is reached;
// closing either end closes both. The socket offers cybershuttle.v1 and link.<token>, the token read from
// LINKSPAN_LINK_TOKEN so it never shows on a command line.
//
//	Link
//	carry
//	Run    One socket as yamux's transport, accepting streams until it or its context ends.
//	New    Parses the args value and validates the URL and token, so main refuses them before binding.
package websocket

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/forward"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

const (
	Env      = "LINKSPAN_LINK_TOKEN"
	Usage    = "websocket mode `args`: \"--url <ws|wss URL>\" of cs-plane; token in " + Env
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

func (l *Link) Run(ctx context.Context) error {
	ws, response, err := l.dialer.DialContext(ctx, l.url, nil)
	if response != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return err
	}
	local, remote := net.Pipe()
	go forward.Pipe(ws, remote)
	session, _ := yamux.Server(local, nil)
	defer func() { _ = session.Close() }()
	for {
		stream, err := session.AcceptStreamWithContext(ctx)
		if err != nil {
			return err
		}
		go carry(stream)
	}
}

func New(args string) (func(context.Context) error, error) {
	fs := flag.NewFlagSet("websocket", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rawURL, token := fs.String("url", "", ""), os.Getenv(Env)
	perr := fs.Parse(strings.Fields(args))
	u, err := url.Parse(*rawURL)
	if perr != nil || fs.NArg() > 0 || err != nil || u.Host == "" || u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, errors.New("--tunnel-websocket-args needs only --url, a ws or wss URL")
	}
	if token == "" {
		return nil, errors.New(Env + " is required with the websocket mode")
	}
	return (&Link{url: *rawURL, dialer: websocket.Dialer{HandshakeTimeout: 30 * time.Second, Subprotocols: []string{protocol, "link." + token}}}).Run, nil
}
