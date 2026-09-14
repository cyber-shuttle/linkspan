// Tests for the wire without fetching ttyd.
//
//	TestCreateOffLinux  Off Linux, create must answer 501 for lack of a build.
package terminal

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

func TestCreateOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("a Linux build exists, and create would fetch it")
	}
	status, _, msg := startSession(context.Background(), map[string]any{"cwd": "/work"})
	if status != http.StatusNotImplemented || !strings.Contains(msg, "no ttyd binary") {
		t.Fatalf("off Linux create answered %d %q, want 501", status, msg)
	}
}
