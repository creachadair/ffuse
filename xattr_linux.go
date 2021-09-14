// Copyright (C) 2019 Michael J. Fromberger. All Rights Reserved.

package ffuse

import (
	"syscall"

	"github.com/seaweedfs/fuse"
)

// Symbolic constants for extended attributes, Linux version.
// These are not exposed in the syscall package.
const (
	xattrCreate  = 1 // XATTR_CREATE for setxattr(2)
	xattrReplace = 2 // XATTR_REPLACE for setxattr(2)

	// The errno returned for "xattr not found".
	xattrErrnoNotFound = fuse.Errno(syscall.ENODATA)
)
