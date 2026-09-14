// Package install owns ~/.cybershuttle, where everything Linkspan fetches or builds lives, beside the binary the
// clients install there. Nothing is verified beyond the transport.
//
//	Asset  The entry of a GOOS/GOARCH-keyed table for this platform, or an error naming what has no build for it.
//	Dir    ~/.cybershuttle.
//	Fetch  Does nothing when dst is present; otherwise downloads src through a sibling file renamed into place, so a
//	       failed transfer publishes nothing.
package install

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

func Asset(table map[string]string, name string) (string, error) {
	asset, ok := table[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("no %s binary for %s/%s", name, runtime.GOOS, runtime.GOARCH)
	}
	return asset, nil
}

func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cybershuttle")
}

func Fetch(ctx context.Context, dst, src string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	log.Printf("install: downloading %s -> %s", src, dst)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: unexpected status %s", src, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("GET %s: %w", src, err)
	}
	part := dst + ".part"
	defer func() { _ = os.Remove(part) }()
	if err := os.WriteFile(part, data, 0o700); err != nil {
		return err
	}
	return os.Rename(part, dst)
}
