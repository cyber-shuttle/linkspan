// Tests for the writer every route answers through, for what a command sees of a request, and for the paths a nested
// tree holds.
//
//	TestErrorShape
//	TestParams          The body is the params, the path id joins them, no body is no params, and bad or oversize
//	                    bodies are refused before the command runs.
//	TestNestedPrefixes  A leaf-first chain of many levels, each with siblings, and one subtree mounted twice.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	respond(func(context.Context, map[string]any) (int, any, string) {
		return http.StatusTeapot, nil, "short and stout"
	})(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != http.StatusTeapot || err != nil || body.Error != "short and stout" {
		t.Fatalf("got %d %s, want 418 with the error object", rec.Code, rec.Body)
	}
}

func TestParams(t *testing.T) {
	var seen []string
	echo := func(_ context.Context, params map[string]any) (int, any, string) {
		seen = append(seen, fmt.Sprint(params))
		return http.StatusOK, nil, ""
	}
	h := New(Router{Prefix: "/x", Select: echo, Create: echo, Stop: echo}).Handler()
	call := func(method, path, body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec.Code
	}
	if call(http.MethodGet, "/x", "") != 200 || call(http.MethodPost, "/x", `{"a": 1}`) != 200 || call(http.MethodDelete, "/x/j-9", "") != 200 {
		t.Fatalf("codes; seen %q", seen)
	}
	if want := []string{"map[]", "map[a:1]", "map[id:j-9]"}; fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("commands saw %q, want %q", seen, want)
	}
	if code := call(http.MethodPost, "/x", "{"); code != http.StatusBadRequest {
		t.Fatalf("bad JSON answered %d", code)
	}
	if code := call(http.MethodPost, "/x", `{"a": "`+strings.Repeat("x", 65<<10)+`"}`); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversize body answered %d", code)
	}
	if len(seen) != 3 {
		t.Fatal("a refused body must not reach the command")
	}
}

func TestNestedPrefixes(t *testing.T) {
	ok := func(context.Context, map[string]any) (int, any, string) { return http.StatusOK, nil, "" }
	const depth = 64
	shared := New(Router{Prefix: "/shared", Select: ok, Routes: map[string]Command{"GET /extra": ok}})
	want := map[string]bool{}
	var chain *Router
	for level := depth; level >= 1; level-- {
		prefix := "/l" + strconv.Itoa(level)
		node := New(Router{Prefix: prefix, Create: ok, Stop: ok}).Mount(shared).Mount(New(Router{Prefix: "/sib", Stop: ok}))
		if chain != nil {
			node.Mount(chain)
		}
		chain = node
	}
	root := New(Router{Prefix: "/root", Routes: map[string]Command{"GET /health": ok}}).Mount(chain).Mount(shared)
	want["GET /root/health"], want["GET /root/shared"], want["GET /root/shared/extra"] = true, true, true
	path := "/root"
	for level := 1; level <= depth; level++ {
		path += "/l" + strconv.Itoa(level)
		for _, route := range []string{"POST " + path, "DELETE " + path + "/{id}", "DELETE " + path + "/sib/{id}", "GET " + path + "/shared", "GET " + path + "/shared/extra"} {
			want[route] = true
		}
	}
	for route := range want {
		if root.Routes[route] == nil {
			t.Errorf("missing %s", route)
		}
	}
	for route := range root.Routes {
		if !want[route] {
			t.Errorf("unexpected %s", route)
		}
	}
	if len(root.Routes) != len(want) {
		t.Fatalf("got %d routes, want %d", len(root.Routes), len(want))
	}
}
