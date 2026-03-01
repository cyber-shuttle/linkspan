//go:build !linux && !darwin

package fuse

import "fmt"

// ActionMountRemote is a no-op stub on unsupported platforms.
func ActionMountRemote(params map[string]any) (map[string]any, error) {
	return nil, fmt.Errorf("fuse.mount_remote: FUSE mounting is only supported on Linux and macOS")
}

// isMountActive always returns false on non-Linux platforms.
func isMountActive() bool {
	return false
}

// mountPoint always returns "" on non-Linux platforms.
func mountPoint() string {
	return ""
}

// cleanupMount is a no-op on non-Linux platforms.
func cleanupMount() {}
