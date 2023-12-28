//go:build linux

package main

import (
	"time"

	"github.com/creachadair/mds/value"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var fuseOptions = fs.Options{
	MountOptions: fuse.MountOptions{
		FsName: "ffs",
		Name:   "ffs",
	},
	EntryTimeout: value.Ptr(time.Second),
	AttrTimeout:  value.Ptr(time.Second),
}
