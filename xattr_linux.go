package ffuse

import (
	"syscall"

	"bazil.org/fuse"
)

const (
	xattrCreate  = 1 // XATTR_CREATE for setxattr(2)
	xattrReplace = 2 // XATTR_REPLACE for setxattr(2)

	xattrErrnoNotFound = fuse.Errno(syscall.ENODATA)
)
