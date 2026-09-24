// Package forward carries a TCP connection to a server Linkspan runs over a WebSocket on Linkspan's own HTTP listener.
// A client that can reach the control port, over the tunnel or a relay such as cs-plane, reaches every server behind
// it through that one port. Binary frames carry the bytes each way, and closing either side closes both.
//
//	Dial    nil unless a running task serves that loopback port, so a stream reaches Linkspan's own servers and
//	        nothing else on the node.
//	Pipe
//	Stream  GET /api/v1/forward/{port}: 404 unless Dial reaches the port.
package forward

import (
	"net"
	"net/http"
	"strconv"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/gorilla/websocket"
)

var upgrader websocket.Upgrader

func Dial(port int) net.Conn {
	if !tasks.IsServing(port) {
		return nil
	}
	conn, _ := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	return conn
}

func Pipe(client *websocket.Conn, server net.Conn) {
	defer func() { _ = server.Close() }()
	go func() {
		defer func() { _ = client.Close() }()
		buf := make([]byte, 32<<10)
		for {
			n, err := server.Read(buf)
			if n > 0 && client.WriteMessage(websocket.BinaryMessage, buf[:n]) != nil || err != nil {
				return
			}
		}
	}()
	for {
		_, data, err := client.ReadMessage()
		if err == nil {
			_, err = server.Write(data)
		}
		if err != nil {
			return
		}
	}
}

func Stream(w http.ResponseWriter, r *http.Request) {
	port, _ := strconv.Atoi(r.PathValue("port"))
	server := Dial(port)
	if server == nil {
		http.Error(w, `{"error":"no running server on that port"}`, http.StatusNotFound)
		return
	}
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		_ = server.Close()
		return
	}
	Pipe(client, server)
}
