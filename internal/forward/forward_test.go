// Tests for the stream: a task's server is reached and answers, and a port no task binds is refused.
//
//	TestStreamReachesTaskPorts  An HTTP exchange with a task's server through the stream; 404 for an unbound port.
//	TestHTTPReachesTaskPorts    A request reaches a task's server at the path and query after the port; 404 for an unbound port.
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
	served, err := (&tasks.Task{Kind: "test", Server: &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}}).Start()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /f/{port}", Stream)
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
}

func TestHTTPReachesTaskPorts(t *testing.T) {
	t.Cleanup(tasks.StopAll)
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Saw", r.Host+r.URL.RequestURI()) })
	served, err := (&tasks.Task{Kind: "test", Server: &http.Server{Handler: echo, ReadHeaderTimeout: time.Second}}).Start()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/f/{port}/{path...}", HTTP)
	server := httptest.NewServer(mux)
	defer server.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode, resp.Header.Get("Saw")
	}

	if status, _ := get("/f/1/api"); status != http.StatusNotFound {
		t.Fatalf("an unbound port answered %d, want 404", status)
	}
	port := strconv.Itoa(served.Port())
	if status, saw := get("/f/" + port + "/api/kernels?token=t"); status != http.StatusOK || saw != "127.0.0.1:"+port+"/api/kernels?token=t" {
		t.Fatalf("the server saw %d %q, want its own host and the path after the port", status, saw)
	}
}
