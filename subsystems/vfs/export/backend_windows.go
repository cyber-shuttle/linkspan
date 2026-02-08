//go:build windows

package export

import "os"

func uidGidFromFileInfo(info os.FileInfo) (uid, gid uint32) {
	return 0, 0
}

func getStatfs(path string) (blocks, bfree, bavail, files, ffree uint64, bsize uint32, err error) {
	// Windows has no direct Statfs equivalent; return synthetic values.
	return 0, 0, 0, 0, 0, 4096, nil
}
