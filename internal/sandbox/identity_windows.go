package sandbox

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

func placeholderIdentity(f *os.File, _ fs.FileInfo) (device, inode uint64, ok bool) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, 0, false
	}
	return uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), true
}

func placeholderEntryIdentity(parent *os.Root, name string, info fs.FileInfo) (device, inode uint64, ok bool) {
	if info.Mode()&fs.ModeSymlink != 0 {
		return 0, 0, false
	}
	f, err := parent.Open(name)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return 0, 0, false
	}
	return placeholderIdentity(f, opened)
}
