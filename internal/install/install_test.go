// Tests that nothing half-written is published.
//
//	TestFetchIsAtomic
package install

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name      string
		existing  string
		handler   http.HandlerFunc
		published string
	}{
		{"whole body", "", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("binary")) }, "binary"},
		{"already present", "old", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusNotFound) }, "old"},
		{"non-200", "", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusNotFound) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".cybershuttle", "bin")
			if tc.existing != "" {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "tool"), []byte(tc.existing), 0o700); err != nil {
					t.Fatal(err)
				}
			}

			dst := filepath.Join(dir, "tool")
			err := Fetch(context.Background(), dst, srv.URL+"/tool")
			if (err == nil) != (tc.published != "") {
				t.Fatalf("download returned %v, want published=%v", err, tc.published != "")
			}
			if entries, _ := os.ReadDir(dir); (len(entries) == 1) != (tc.published != "") {
				t.Fatalf("directory holds %d entries, want one exactly when something is published", len(entries))
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
