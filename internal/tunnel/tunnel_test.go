// Tests for the transport framing. Each case expects the tasks' kinds, or an error naming the offending flag.
//
//	TestParse
//	TestRedialRerunsAFailedTransport  A failing attempt is rerun, not fatal, and cancellation ends the loop cleanly.
package tunnel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/tunnel/devtunnel"
	"github.com/cyber-shuttle/linkspan/internal/tunnel/link"
)

func TestParse(t *testing.T) {
	t.Setenv(link.Env, "link-token")
	t.Setenv(devtunnel.Env, "host-token")
	lk, dt := "--url wss://plane.example/link", "--id t --cluster usw2"
	for _, c := range []struct {
		enable bool
		list   string
		args   map[string]string
		want   string
	}{
		{false, "", nil, ""},
		{true, "link", map[string]string{"link": lk}, "link"},
		{true, "devtunnel", map[string]string{"devtunnel": dt}, "devtunnel"},
		{true, "link,devtunnel", map[string]string{"link": lk, "devtunnel": dt}, "devtunnel,link"},
		{true, "", nil, "--tunnel-mode"},
		{false, "link", map[string]string{"link": lk}, "--tunnel-mode"},
		{true, "ssh", nil, "--tunnel-mode"},
		{true, "link,link", map[string]string{"link": lk}, "--tunnel-mode"},
		{true, "link", nil, "--tunnel-link-args"},
		{true, "link", map[string]string{"link": lk, "devtunnel": dt}, "--tunnel-devtunnel-args"},
		{true, "link", map[string]string{"link": "--url https://plane.example"}, "--tunnel-link-args"},
		{true, "link", map[string]string{"link": lk + " --token x"}, "--tunnel-link-args"},
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

func TestRedialRerunsAFailedTransport(t *testing.T) {
	attempts := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- redial("test", func(context.Context) error { attempts <- struct{}{}; return errors.New("exited") })(ctx)
	}()
	for range 2 {
		select {
		case <-attempts:
		case <-time.After(5 * time.Second):
			t.Fatal("a failed attempt was not rerun")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a cancelled redial returned %v", err)
	}
}
