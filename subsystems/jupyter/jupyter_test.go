// Tests for what reaches the server and the wire without building an environment.
//
//	TestToken     43 URL-safe characters, and two differ.
//	TestCommands  The list must carry only the Jupyter kind, and stop must answer 404 for an unknown id and 200 once
//	              for a listed one.
package jupyter

import (
	"context"
	"net/http"
	"regexp"
	"testing"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

func TestToken(t *testing.T) {
	a, b := newToken(), newToken()
	if a == b || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(a) {
		t.Fatalf("tokens %q %q", a, b)
	}
}

func TestCommands(t *testing.T) {
	ctx := context.Background()
	idle := func(ctx context.Context) error { <-ctx.Done(); return nil }
	_, _ = (&tasks.Task{ID: "t-1", Kind: "terminal", Run: idle}).Start()
	_, _ = (&tasks.Task{ID: "j-2", Kind: kind, Run: idle}).Start()
	t.Cleanup(tasks.StopAll)
	if _, body, _ := Commands["sessions.select"](ctx, nil); len(body.([]tasks.Task)) != 1 || body.([]tasks.Task)[0].ID != "j-2" {
		t.Fatalf("list = %+v, want the Jupyter kind alone", body)
	}
	stop := func(id string) int {
		status, _, _ := Commands["sessions.stop"](ctx, map[string]any{"id": id})
		return status
	}
	if stop("nope") != http.StatusNotFound || stop("j-2") != http.StatusOK || stop("j-2") != http.StatusNotFound {
		t.Fatal("stop must answer 404 for an unknown id and 200 once for a listed one")
	}
}
