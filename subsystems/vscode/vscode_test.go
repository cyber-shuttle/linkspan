// Tests for the SSH session surface cs-plane drives: the handlers are called directly and their bodies marshalled.
//
//	authorizedKey
//	marshal
//	TestCreateSessionServesOnReturn, TestCreateSessionRejectsBadKey
//	TestARefReusesItsServer
//	TestSelectShape  An empty list marshals as [].
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

func TestCreateSessionServesOnReturn(t *testing.T) {
	t.Cleanup(func() { tasks.StopAll() })
	status, body, errMsg := startSession(context.Background(), map[string]any{"authorized_key": authorizedKey})
	var out struct {
		BindPort int `json:"bind_port"`
	}
	if status != http.StatusCreated || json.Unmarshal(marshal(t, body), &out) != nil || out.BindPort == 0 {
		t.Fatalf("create = %d %s: %s", status, marshal(t, body), errMsg)
	}
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(out.BindPort))
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

func TestARefReusesItsServer(t *testing.T) {
	t.Cleanup(func() { tasks.StopAll() })
	start := func() (int, any) {
		status, body, _ := startSession(context.Background(), map[string]any{"authorized_key": authorizedKey, "ref": "ssh-laptop"})
		return status, body.(map[string]any)["bind_port"]
	}
	firstStatus, firstPort := start()
	againStatus, againPort := start()
	if firstStatus != http.StatusCreated || againStatus != http.StatusOK || firstPort != againPort {
		t.Fatalf("a repeated ref answered %d/%v then %d/%v, want 201 then 200 on the same port", firstStatus, firstPort, againStatus, againPort)
	}
}

func TestSelectShape(t *testing.T) {
	_, body, _ := Commands["sessions.select"](context.Background(), nil)
	if got := string(marshal(t, body)); got != "[]" {
		t.Fatalf("an empty sessions list must marshal as [], got %s", got)
	}
}
