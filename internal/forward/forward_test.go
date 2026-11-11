// Tests for the stream: a task's server is reached and answers, and a port no task binds is refused.
//
//	TestStreamReachesTaskPorts  An HTTP exchange with a task's server through the stream; 404 for an unbound port;
//	                            a request with a path reaches the server as HTTP at that path.
package forward

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/gorilla/websocket"
)

func TestStreamReachesTaskPorts(t *testing.T) {
	t.Cleanup(tasks.StopAll)
	served, err := (&tasks.Task{Kind: "test", Server: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Saw", r.Host+r.URL.RequestURI())
		http.NotFound(w, r)
	}), ReadHeaderTimeout: time.Second}}).Start()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/f/{port...}", Stream)
	server := httptest.NewServer(mux)
	defer server.Close()
	base := "ws" + strings.TrimPrefix(server.URL, "http") + "/f/"

	_, refused, err := websocket.DefaultDialer.Dial(base+"1", nil)
	if err == nil || refused.StatusCode != http.StatusNotFound {
		t.Fatalf("an unbound port was not refused with 404: %v", err)
	}
	_ = refused.Body.Close()
	conn, upgraded, err := websocket.DefaultDialer.Dial(base+strconv.Itoa(served.Port()), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = upgraded.Body.Close()
	defer func() { _ = conn.Close() }()
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if kind, data, err := conn.ReadMessage(); err != nil || kind != websocket.BinaryMessage || !strings.HasPrefix(string(data), "HTTP/1.0 404") {
		t.Fatalf("read %d %q %v, want the server's answer in a binary frame", kind, data, err)
	}
	port := strconv.Itoa(served.Port())
	resp, err := http.Get(server.URL + "/f/" + port + "/api/kernels?token=t")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if saw := resp.Header.Get("Saw"); saw != "127.0.0.1:"+port+"/api/kernels?token=t" {
		t.Fatalf("the server saw %q, want its own host and the path after the port", saw)
	}
}
