// fuse-test-server starts a FUSE TCP server on a temp directory with test
// files, prints the port, and waits for a signal to shut down.
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cyber-shuttle/linkspan/subsystems/fuse"
)

func main() {
	// Create temp directory with test data
	tmpDir, err := os.MkdirTemp("", "fuse-test-*")
	if err != nil {
		log.Fatalf("mktemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test files
	os.WriteFile(filepath.Join(tmpDir, "hello.txt"), []byte("Hello from the Go FUSE server!\n"), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "data.bin"), []byte{0xDE, 0xAD, 0xBE, 0xEF}, 0o644)
	os.Mkdir(filepath.Join(tmpDir, "subdir"), 0o755)
	os.WriteFile(filepath.Join(tmpDir, "subdir", "nested.txt"), []byte("I am nested.\n"), 0o644)

	// Start server
	srv := fuse.NewServer(tmpDir)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("FUSE_SERVER_PORT=%d\n", port)
	fmt.Printf("FUSE_SERVER_ROOT=%s\n", tmpDir)
	fmt.Println("READY")

	go srv.Serve(ln)

	// Wait for signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	ln.Close()
	fmt.Println("Server stopped.")
}
