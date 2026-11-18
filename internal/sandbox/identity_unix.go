//go:build darwin || linux

package sandbox

import (
	"io/fs"
	"os"
	"syscall"
)

func placeholderIdentity(_ *os.File, info fs.FileInfo) (device, inode uint64, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), stat.Ino, true
}

func placeholderEntryIdentity(_ *os.Root, _ string, info fs.FileInfo) (device, inode uint64, ok bool) {
	return placeholderIdentity(nil, info)
}
