// Tests for the CLI download and the relay's lifecycle under StopAll. The
// heartbeat fake appends to a file named by its tunnel id and never reports
// ready, so a relay left running is visible.
//
//	fakeDevtunnel                                 It installs a fake CLI under a
//	                                              temporary HOME; $2 is the
//	                                              qualified id.
//	installHeartbeat, beatCount, assertRelayDead  They install the heartbeat
//	                                              fake, count its beats, and
//	                                              assert the count has stopped
//	                                              growing.
//	TestDownloadDevtunnelBinaryIsAtomic           A refused transfer must publish
//	                                              nothing, and a present binary
//	                                              must be kept.
//	TestReadyOnMarker                             The ready channel must close
//	                                              once the marker is complete,
//	                                              however it is split, and even
//	                                              past the output cap.
//	TestReadyTimeoutKills                         A relay that never reports
//	                                              ready must be killed at the
//	                                              timeout.
//	TestStopAllKillsTheRelay                      A hosted relay must be dead
//	                                              after StopAll.
//	TestRelayExitEndsTheTask                      A relay that exits must end the
//	                                              task with an error.
package tunnel

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/procmgr"
)

func fakeDevtunnel(t *testing.T, script string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, ".linkspan", "bin", "devtunnel")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func installHeartbeat(t *testing.T) (*Tunnel, string) {
	t.Helper()
	fakeDevtunnel(t, "#!/bin/sh\nwhile :; do echo . >> \"$2\"; sleep 0.02; done\n")
	id := filepath.Join(t.TempDir(), "beat")
	tn, err := New(id, "c", "token")
	if err != nil {
		t.Fatal(err)
	}
	return tn, id + ".c"
}

func beatCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(b)
}

func assertRelayDead(t *testing.T, beat, after string) {
	t.Helper()
	before := beatCount(t, beat)
	time.Sleep(200 * time.Millisecond)
	if now := beatCount(t, beat); now != before {
		t.Fatalf("relay still running after %s: heartbeat grew %d -> %d", after, before, now)
	}
}

func TestDownloadDevtunnelBinaryIsAtomic(t *testing.T) {
	serve := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
	}
	for _, tc := range []struct {
		name      string
		existing  string
		handler   http.HandlerFunc
		published string
	}{
		{"whole body", "", serve("binary"), "binary"},
		{"already present", "old", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusNotFound) }, "old"},
		{"non-200", "", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusNotFound) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			old := cliBase
			cliBase = srv.URL + "/"
			t.Cleanup(func() { cliBase = old })
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".linkspan", "bin")
			if tc.existing != "" {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "devtunnel"), []byte(tc.existing), 0o700); err != nil {
					t.Fatal(err)
				}
			}

			dst, err := downloadDevtunnelBinary(context.Background())
			if (err == nil) != (tc.published != "") {
				t.Fatalf("download returned %v, want published=%v", err, tc.published != "")
			}
			want := 0
			if tc.published != "" {
				want = 1
			}
			if entries, _ := os.ReadDir(dir); len(entries) != want {
				t.Fatalf("directory holds %d entries, want %d", len(entries), want)
			}
			if tc.published == "" {
				return
			}
			if b, err := os.ReadFile(dst); err != nil || string(b) != tc.published {
				t.Fatalf("published %q (%v), want %q", b, err, tc.published)
			}
			if info, err := os.Stat(dst); err != nil || info.Mode().Perm()&0o111 == 0 {
				t.Fatalf("published binary is not executable (%v)", err)
			}
		})
	}
}

func TestReadyOnMarker(t *testing.T) {
	o := &output{ready: make(chan struct{})}
	_, _ = o.Write([]byte("Ready to acc"))
	select {
	case <-o.ready:
		t.Fatal("ready closed before the marker was complete")
	default:
	}
	_, _ = o.Write([]byte("ept connections\nmore\n"))
	_, _ = o.Write([]byte("Ready to accept connections\n"))
	select {
	case <-o.ready:
	default:
		t.Fatal("ready not closed once the marker was complete")
	}

	o = &output{ready: make(chan struct{})}
	_, _ = o.Write(bytes.Repeat([]byte("x"), 70<<10))
	_, _ = o.Write([]byte("Ready to accept connections\n"))
	select {
	case <-o.ready:
	default:
		t.Fatal("ready not closed when the marker arrived past the output cap")
	}
	if s := o.String(); len(s) != 64<<10 || !strings.HasSuffix(s, "connections\n") {
		t.Fatalf("captured %d bytes ending %q; want the last 64KB", len(s), s[len(s)-12:])
	}
}

func TestReadyTimeoutKills(t *testing.T) {
	old := hostReadyTimeout
	hostReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { hostReadyTimeout = old })
	tn, beat := installHeartbeat(t)

	err := tn.host(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no ready signal within") {
		t.Fatalf("err = %v, want a timeout naming the deadline", err)
	}
	assertRelayDead(t, beat, "the timeout")
}

func TestStopAllKillsTheRelay(t *testing.T) {
	tn, beat := installHeartbeat(t)
	tn.Start()

	for deadline := time.Now().Add(5 * time.Second); beatCount(t, beat) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the fake relay never started")
		}
	}

	procmgr.StopAll()
	assertRelayDead(t, beat, "StopAll")
}

func TestRelayExitEndsTheTask(t *testing.T) {
	fakeDevtunnel(t, "#!/bin/sh\necho 'Ready to accept connections'\n")
	tn, err := New("t", "c", "token")
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- tn.host(context.Background()) }()
	select {
	case err := <-exited:
		if err == nil || !strings.Contains(err.Error(), "relay exited") {
			t.Fatalf("want a relay-exited error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the task never returned after the ready relay died")
	}
}
