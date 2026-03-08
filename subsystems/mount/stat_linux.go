package mount

import (
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func fillStatFields(info os.FileInfo, out *fuse.Attr) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	out.Ino = stat.Ino
	out.Nlink = uint32(stat.Nlink)
	out.Uid = stat.Uid
	out.Gid = stat.Gid
	out.Atime = uint64(stat.Atim.Sec)
	out.Atimensec = uint32(stat.Atim.Nsec)
	out.Mtime = uint64(stat.Mtim.Sec)
	out.Mtimensec = uint32(stat.Mtim.Nsec)
	out.Ctime = uint64(stat.Ctim.Sec)
	out.Ctimensec = uint32(stat.Ctim.Nsec)
	out.Blksize = uint32(stat.Blksize)
	out.Blocks = uint64(stat.Blocks)
}
