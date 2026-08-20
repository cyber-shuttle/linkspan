package tunnel

import (
	"slices"
	"testing"
)

// TestRedactedArgs guards the CLI log path: a host/connect token must never be
// written to the log, and every other argument must survive unchanged.
func TestRedactedArgs(t *testing.T) {
	args := []string{"host", "ls-48.use2", "--access-token", "s3cret"}
	got := redactedArgs(args)
	want := []string{"host", "ls-48.use2", "--access-token", "[redacted]"}
	if !slices.Equal(got, want) {
		t.Fatalf("redactedArgs = %v, want %v", got, want)
	}
	if args[3] != "s3cret" {
		t.Fatal("redactedArgs mutated its input")
	}
}
