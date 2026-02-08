package vfs

import "errors"

var (
	ErrMountNotFound     = errors.New("mount not found")
	ErrPublishNotFound   = errors.New("publish not found")
	ErrUnsupported       = errors.New("FUSE mount not supported on this platform (Linux only)")
	errMountTokenInvalid = errors.New("mount token must be id:secret")
)
