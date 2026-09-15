package filesystem

import (
	"context"
	"net/http"
	"testing"
)

func TestDeclared(t *testing.T) {
	for name, params := range map[string][]string{"mount": {"source", "target"}, "unmount": {"target"}, "copy": {"source", "target"}, "sync": {"source", "target"}} {
		if status, _, msg := Commands[name](context.Background(), nil); status != http.StatusBadRequest {
			t.Errorf("%s without params answered %d %q, want 400", name, status, msg)
		}
		given := map[string]any{}
		for _, p := range params {
			given[p] = "/x"
		}
		if status, _, msg := Commands[name](context.Background(), given); status != http.StatusNotImplemented {
			t.Errorf("%s with %v answered %d %q, want 501", name, params, status, msg)
		}
	}
}
