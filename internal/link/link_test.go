// Tests for the link against a fake cs-plane that multiplexes streams over the one socket Linkspan holds.
//
//	TestLinkMultiplexesStreamsOverOneSocket  The token rides the subprotocol; a port no task serves is refused, a
//	                                         served one carries an HTTP exchange, closing the stream closes the
//	                                         server's connection, and every stream shares the one socket.
package link

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/forward"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

func TestLinkMultiplexesStreamsOverOneSocket(t *testing.T) {
	t.Cleanup(tasks.StopAll)
	closed := make(chan struct{}, 1)
	server := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second, ConnState: func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}}
	served, err := (&tasks.Task{Kind: "test", Server: server}).Start()
	if err != nil {
		t.Fatal(err)
	}
	sessions := make(chan *yamux.Session, 1)
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Sec-Websocket-Protocol"); got != protocol+", link.secret" {
			t.Errorf("offered subprotocols %q", got)
		}
		ws, err := (&websocket.Upgrader{Subprotocols: []string{protocol}}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		local, remote := net.Pipe()
		go forward.Pipe(ws, remote)
		session, _ := yamux.Client(local, nil)
		sessions <- session
	}))
	defer plane.Close()

	ln, err := New("ws"+strings.TrimPrefix(plane.URL, "http"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ln.Run(ctx) }()
	session := <-sessions
	open := func(port int) (net.Conn, byte) {
		stream, err := session.Open()
		if err != nil {
			t.Fatal(err)
		}
		answer := binary.BigEndian.AppendUint16(nil, uint16(port&0xffff))
		if _, err := stream.Write(answer); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(stream, answer[:1]); err != nil {
			t.Fatal(err)
		}
		return stream, answer[0]
	}

	if _, answer := open(1); answer != 0 {
		t.Fatalf("an unserved port answered %d, want a refusal", answer)
	}
	stream, answer := open(served.Port())
	if answer != 1 {
		t.Fatalf("a served port answered %d", answer)
	}
	if _, err := stream.Write([]byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 12)
	if _, err := io.ReadFull(stream, reply); err != nil || !strings.HasPrefix(string(reply), "HTTP/1.0 404") {
		t.Fatalf("read %q %v, want the server's answer", reply, err)
	}
	_ = stream.Close()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("closing the stream left the connection to the server open")
	}
}
