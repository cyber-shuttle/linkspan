// Tests for the mode framing. Each case expects the tasks' kinds, or an error naming the offending flag.
//
//	TestParse
package tunnel

import (
	"strings"
	"testing"

	"github.com/cyber-shuttle/linkspan/internal/tunnel/devtunnel"
	"github.com/cyber-shuttle/linkspan/internal/tunnel/websocket"
)

func TestParse(t *testing.T) {
	t.Setenv(websocket.Env, "link-token")
	t.Setenv(devtunnel.Env, "host-token")
	ws, dt := "--url wss://plane.example/link", "--id t --cluster usw2"
	for _, c := range []struct {
		enable bool
		list   string
		args   map[string]string
		want   string
	}{
		{false, "", nil, ""},
		{true, "websocket", map[string]string{"websocket": ws}, "websocket"},
		{true, "devtunnel", map[string]string{"devtunnel": dt}, "devtunnel"},
		{true, "websocket,devtunnel", map[string]string{"websocket": ws, "devtunnel": dt}, "devtunnel,websocket"},
		{true, "", nil, "--tunnel-mode"},
		{false, "websocket", map[string]string{"websocket": ws}, "--tunnel-mode"},
		{true, "ssh", nil, "--tunnel-mode"},
		{true, "websocket,websocket", map[string]string{"websocket": ws}, "--tunnel-mode"},
		{true, "websocket", nil, "--tunnel-websocket-args"},
		{true, "websocket", map[string]string{"websocket": ws, "devtunnel": dt}, "--tunnel-devtunnel-args"},
		{true, "websocket", map[string]string{"websocket": "--url https://plane.example"}, "--tunnel-websocket-args"},
		{true, "websocket", map[string]string{"websocket": ws + " --token x"}, "--tunnel-websocket-args"},
		{true, "devtunnel", map[string]string{"devtunnel": "--id t"}, "--tunnel-devtunnel-args"},
	} {
		all, err := Parse(c.enable, c.list, c.args)
		kinds := make([]string, 0, len(all))
		for _, task := range all {
			kinds = append(kinds, string(task.Kind))
		}
		got := strings.Join(kinds, ",")
		if err != nil {
			got = strings.TrimSuffix(strings.Fields(err.Error())[0], ":")
		}
		if got != c.want {
			t.Errorf("Parse(%v, %q, %q) gave %q (%v), want %q", c.enable, c.list, c.args, got, err, c.want)
		}
	}
}
