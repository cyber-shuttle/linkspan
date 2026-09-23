// Tests for the stream: a task's server is reached and answers, and a port no task binds is refused.
//
//	TestStreamReachesTaskPorts  An HTTP exchange with a task's server through the stream; 404 for an unbound port.
package forward

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/gorilla/websocket"
)

func TestStreamReachesTaskPorts(t *testing.T) {
	t.Cleanup(tasks.StopAll)
	served, err := (&tasks.Task{Kind: "test", Server: &http.Server{Handler: http.NotFoundHandler()}}).Start()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /f/{port}", Stream)
	server := httptest.NewServer(mux)
	defer server.Close()
	base := "ws" + strings.TrimPrefix(server.URL, "http") + "/f/"

	if _, response, err := websocket.DefaultDialer.Dial(base+"1", nil); err == nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("an unbound port was not refused with 404: %v", err)
	}
	conn, _, err := websocket.DefaultDialer.Dial(base+strconv.Itoa(served.Port()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if kind, data, err := conn.ReadMessage(); err != nil || kind != websocket.BinaryMessage || !strings.HasPrefix(string(data), "HTTP/1.0 404") {
		t.Fatalf("read %d %q %v, want the server's answer in a binary frame", kind, data, err)
	}
}
