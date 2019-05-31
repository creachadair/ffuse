package ffuse

import (
	"syscall"

	"bazil.org/fuse"
)

const (
	xattrCreate  = 2 // XATTR_CREATE for setxattr(2)
	xattrReplace = 4 // XATTR_REPLACE for setxattr(2)

	xattrErrnoNotFound = fuse.Errno(syscall.ENOATTR)
)
