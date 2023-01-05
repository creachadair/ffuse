//go:build linux

package main

import "github.com/seaweedfs/fuse"

var fuseMountOptions = []fuse.MountOption{
	fuse.FSName("ffs"),
	fuse.Subtype("ffs"),
	fuse.VolumeName("FFS"),
	fuse.MaxReadahead(1 << 16),
}
