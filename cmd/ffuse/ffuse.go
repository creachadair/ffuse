// Copyright 2019 Michael J. Fromberger. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Program ffuse mounts an FFS filesystem via FUSE.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/creachadair/ffs/blob"
	"github.com/creachadair/ffstools/ffs/config"
	"github.com/creachadair/ffuse"

	"github.com/seaweedfs/fuse"
	"github.com/seaweedfs/fuse/fs"
)

var (
	storeAddr  = flag.String("store", os.Getenv("FFS_STORE"), "Blob storage address (required)")
	mountPoint = flag.String("mount", "", "Path of mount point (required)")
	doReadOnly = flag.Bool("read-only", false, "Mount the filesystem as read-only")
	rootKey    = flag.String("root", "", "Storage key of root pointer")
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %[1]s [-read-only] -store addr -mount path -root key[/path...]

Mount a FFS filesystem via FUSE at the specified -mount path, using the blob
store described by addr. The starting point for the mount may be the name of a
root pointer, or a path relative to a root pointer, or a specific storage key
prefixed by "@".

If -store is not set, the FFS_STORE environment variable is used as a default
if it is defined; otherwise the default from the FFS config file is used or an
error is reported.

Options:
`, filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
}

func main() {
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("[ffuse] ")

	switch {
	case *mountPoint == "":
		log.Fatal("You must set a non-empty -mount path")
	case *rootKey == "":
		log.Fatal("You must set a non-empty -root pointer key")
	}

	cfg, err := config.Load(config.Path())
	if err != nil {
		log.Fatalf("Loading configuration: %v", err)
	}
	if *storeAddr != "" {
		cfg.DefaultStore = *storeAddr
	}

	ctx := context.Background()
	cas, err := cfg.OpenStore()
	if err != nil {
		log.Fatalf("Opening blob store: %v", err)
	}
	defer blob.CloseStore(ctx, cas)

	pi, err := config.OpenPath(ctx, cas, *rootKey)
	if err != nil {
		log.Fatalf("Loading root path: %v", err)
	}
	if pi.Root != nil {
		log.Printf("Loaded filesystem from %q (%x)", pi.RootKey, pi.FileKey)
		if pi.Root.Description != "" {
			log.Printf("| Description %s", pi.Root.Description)
		}
	} else {
		log.Printf("Loaded filesystem at %x (no root pointer)", pi.FileKey)
	}

	// Mount the filesystem and serve from our filesystem root.
	opts := []fuse.MountOption{
		fuse.FSName("ffs"),
		fuse.Subtype("ffs"),
		fuse.VolumeName("FFS"),
		fuse.NoAppleDouble(),
	}
	if *doReadOnly {
		opts = append(opts, fuse.ReadOnly())
	}

	c, err := fuse.Mount(*mountPoint, opts...)
	if err != nil {
		log.Fatalf("Mount failed: %v", err)
	}

	server := fs.New(c, nil)
	fsys := ffuse.New(pi.File, server)
	done := make(chan error, 1)
	go func() { defer close(done); done <- fs.Serve(c, fsys) }()

	// Wait for the server to come up, and check that it successfully mounted.
	<-c.Ready
	if err := c.MountError; err != nil {
		c.Close()
		log.Fatalf("Mount error: %v", err)
	}

	// Block indefinitely to let the server run, but handle interrupt and
	// termination signals to unmount and flush the root.
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-done:
		log.Printf("Server exited: %v", err)
	case s := <-sig:
		log.Printf("Received signal: %v", s)
		log.Printf("Unmounting %q", *mountPoint)
		if err := fuse.Unmount(*mountPoint); err != nil {
			log.Printf("Warning: unmount failed: %v", err)
		}
	}
	if err := c.Close(); err != nil {
		log.Printf("Warning: closing fuse connection failed: %v", err)
	} else {
		log.Print("Closed fuse connection")
	}
	rk, err := pi.Flush(ctx)
	if err != nil {
		log.Fatalf("Flushing file data: %v", err)
	}
	fmt.Printf("%x\n", rk)
}
