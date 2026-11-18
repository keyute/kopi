//go:build !unix

package sandbox

import "io/fs"

func placeholderIdentity(fs.FileInfo) (device, inode uint64, ok bool) { return 0, 0, false }
