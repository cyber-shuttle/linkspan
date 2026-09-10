// Tests for the session surface cs-bridge parses: the handlers are called directly and their bodies marshalled.
//
//	authorizedKey
//	marshal, create
//	TestCreateSessionServesOnReturn, TestCreateSessionRejectsBadKey
//	TestSelectShape  Only the SSH kind may be listed, with the state literal cs-bridge compares.
package vscode

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"testing"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

const authorizedKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH66P8ofDO6v2AaMYZ7JN3lW/m/b32Ab75yYTf3n6NZg test"

func marshal(t *testing.T, body any) []byte {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func create(t *testing.T) (string, int) {
	t.Helper()
	t.Cleanup(func() { tasks.StopAll() })
	status, body, errMsg := startSession(context.Background(), map[string]any{"authorized_key": authorizedKey})
	if status != http.StatusCreated {
		t.Fatalf("create = %d: %s", status, errMsg)
	}
	var out struct {
		ID       string `json:"id"`
		BindPort int    `json:"bind_port"`
	}
	if err := json.Unmarshal(marshal(t, body), &out); err != nil {
		t.Fatalf("response is not the documented object: %v (%s)", err, marshal(t, body))
	}
	return out.ID, out.BindPort
}

func TestCreateSessionServesOnReturn(t *testing.T) {
	id, port := create(t)
	if port == 0 {
		t.Fatal("bind_port missing or zero")
	}
	if want := "s-" + strconv.Itoa(port); id != want {
		t.Fatalf("id = %q, want %q", id, want)
	}
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("nothing accepting on bind_port when the response was written: %v", err)
	}
	_ = c.Close()
}

func TestCreateSessionRejectsBadKey(t *testing.T) {
	for name, key := range map[string]string{
		"unparsable":   "not-a-key",
		"with options": `from="10.0.0.1" ` + authorizedKey,
	} {
		t.Run(name, func(t *testing.T) {
			status, _, errMsg := startSession(context.Background(), map[string]any{"authorized_key": key})
			if status != http.StatusBadRequest || errMsg == "" {
				t.Fatalf("status = %d message %q, want 400 with a message", status, errMsg)
			}
		})
	}
}

func TestSelectShape(t *testing.T) {
	_, body, _ := Commands["sessions.select"](context.Background(), nil)
	if got := string(marshal(t, body)); got != "[]" {
		t.Fatalf("an empty sessions list must marshal as [], got %s", got)
	}
	idle := func(ctx context.Context) error { <-ctx.Done(); return nil }
	_, _ = (&tasks.Task{ID: "not-a-session", Kind: "tunnel", Run: idle}).Start()
	id, _ := create(t)
	var listed []struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Addr  string `json:"addr"`
	}
	_, body, _ = Commands["sessions.select"](context.Background(), nil)
	if err := json.Unmarshal(marshal(t, body), &listed); err != nil {
		t.Fatalf("sessions body is not the documented array: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != id || listed[0].Addr == "" {
		t.Fatalf("want the one ssh session %s with its addr, got %+v", id, listed)
	}
	if listed[0].State != "running" {
		t.Fatalf("state = %q, want %q -- cs-bridge compares against this literal", listed[0].State, "running")
	}
}
