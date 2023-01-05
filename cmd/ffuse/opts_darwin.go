//go:build darwin

package main

import "github.com/seaweedfs/fuse"

var fuseMountOptions = []fuse.MountOption{
	fuse.FSName("ffs"),
	fuse.Subtype("ffs"),
	fuse.VolumeName("FFS"),
	fuse.NoAppleDouble(),
	fuse.MaxReadahead(1 << 16),
}
