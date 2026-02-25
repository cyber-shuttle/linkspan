//go:build unix

package export

import (
	"os"
	"syscall"
)

func uidGidFromFileInfo(info os.FileInfo) (uid, gid uint32) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint32(stat.Uid), uint32(stat.Gid)
	}
	return 0, 0
}

func getStatfs(path string) (blocks, bfree, bavail, files, ffree uint64, bsize uint32, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}
	return stat.Blocks, stat.Bfree, stat.Bavail, stat.Files, stat.Ffree, uint32(stat.Bsize), nil
}
