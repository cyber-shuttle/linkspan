// Package forward carries a TCP connection to a server Linkspan runs over a WebSocket on Linkspan's own HTTP listener.
// A client that can reach the control port, over a Dev Tunnel or through cs-plane, reaches every server behind it
// through that one port. Binary frames carry the bytes each way, and closing either side closes both.
//
//	Dial    nil unless a running task serves that loopback port, so a stream reaches Linkspan's own servers and
//	        nothing else on the node.
//	Pipe
//	Stream  /api/v1/forward/{port}: 404 unless Dial reaches the port. Any request but a WebSocket upgrade of that
//	        exact path is HTTP, sent on to the port at the path after it, for clients that cannot open a stream.
package forward

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/cyber-shuttle/linkspan/internal/router"
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
	rawPort, path, isHTTP := strings.Cut(r.PathValue("port"), "/")
	port, _ := strconv.Atoi(rawPort)
	server := Dial(port)
	if server == nil {
		router.Error(w, http.StatusNotFound, "no running server on that port")
		return
	}
	if isHTTP || !websocket.IsWebSocketUpgrade(r) {
		(&httputil.ReverseProxy{
			Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(context.Context, string, string) (net.Conn, error) { return server, nil }},
			Rewrite: func(p *httputil.ProxyRequest) {
				p.Out.URL = &url.URL{Scheme: "http", Host: server.RemoteAddr().String(), Path: "/" + path, RawQuery: r.URL.RawQuery}
				p.Out.Host = ""
			},
		}).ServeHTTP(w, r)
		return
	}
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		_ = server.Close()
		return
	}
	Pipe(client, server)
}
