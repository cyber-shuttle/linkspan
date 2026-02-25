//go:build !linux

package vfs

// doMount is a no-op on non-Linux; FUSE mount is Linux-only.
func doMount(cfg MountConfig) (unmount func() error, err error) {
	return nil, ErrUnsupported
}
