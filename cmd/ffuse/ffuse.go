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
	"github.com/creachadair/ffs/file"
	"github.com/creachadair/ffstools/ffs/config"
	"github.com/creachadair/ffuse"

	"github.com/seaweedfs/fuse"
	"github.com/seaweedfs/fuse/fs"
)

var (
	storeAddr   = flag.String("store", os.Getenv("FFS_STORE"), "Blob storage address (required)")
	mountPoint  = flag.String("mount", "", "Path of mount point (required)")
	doReadOnly  = flag.Bool("read-only", false, "Mount the filesystem as read-only")
	doDebugLog  = flag.Bool("debug", false, "Enable debug logging (warning: noisy)")
	rootKey     = flag.String("root", "", "Storage key of root pointer")
	autoFlush   = flag.Duration("auto-flush", 0, "Automatically flush the root at this interval")
	doSigFlush  = flag.Bool("sig-flush", false, "If true, SIGUSR1 will trigger a root flush")
	doSigUpdate = flag.Bool("sig-update", false, "If true, SIGUSR2 will trigger a root reload")
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
	if *doDebugLog {
		cfg.EnableDebugLogging = true
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
		fuse.MaxReadahead(1 << 16),
	}
	if *doReadOnly {
		opts = append(opts, fuse.ReadOnly())
	}

	c, err := fuse.Mount(*mountPoint, opts...)
	if err != nil {
		log.Fatalf("Mount failed: %v", err)
	}

	// Set up auto-flush if it was requested.
	var fsOpts *ffuse.Options
	if *autoFlush > 0 {
		fsOpts = &ffuse.Options{
			AutoFlushInterval: *autoFlush,
			OnAutoFlush: func(f *file.File, err error) {
				if err == nil {
					_, err = pi.Flush(ctx)
				}
				if err != nil {
					log.Printf("WARNING: Auto-flushing failed: %v", err)
				} else {
					log.Print("Root auto-flushed to storage: OK")
				}
			},
		}
		log.Printf("Enabling auto-flush every %v", *autoFlush)
		if *doReadOnly {
			log.Print("NOTE: It does not make sense to -auto-flush with -read-only")
		}
	}

	server := fs.New(c, nil)
	fsys := ffuse.New(pi.File, server, fsOpts)
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
	handleSignals(ctx, done, cas, pi, fsys)

	if err := c.Close(); err != nil {
		log.Printf("WARNING: closing fuse connection failed: %v", err)
	} else {
		log.Print("Closed fuse connection")
	}
	rk, err := pi.Flush(ctx)
	if err != nil {
		log.Fatalf("Flushing file data: %v", err)
	}
	fmt.Printf("%x\n", rk)
}

func handleSignals(ctx context.Context, done <-chan error, cas blob.CAS, pi *config.PathInfo, fsys *ffuse.FS) {
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	for {
		select {
		case err := <-done:
			log.Printf("Server exited: %v", err)
			return

		case s := <-sig:
			log.Printf("Received signal: %v", s)
			switch s {
			case syscall.SIGUSR1:
				if !*doSigFlush {
					log.Print("- Ignoring signal because -sig-flush is false")
					continue
				}
				if _, err := pi.Flush(ctx); err != nil {
					log.Printf("WARNING: Root flush failed; %v", err)
				} else {
					log.Print("Root flushed to storage: OK")
				}
				continue

			case syscall.SIGUSR2:
				if !*doSigUpdate {
					log.Print("- Ignoring signal because -sig-update is false")
					continue
				}
				npi, err := config.OpenPath(ctx, cas, *rootKey)
				if err != nil {
					log.Printf("WARNING: Reloading filesystem root failed: %v", err)
				} else {
					*pi = *npi
					fsys.Update(pi.File)
					log.Print("Reloaded filesystem root: OK")
				}
				continue
			}

			log.Printf("Unmounting %q", *mountPoint)
			if err := fuse.Unmount(*mountPoint); err != nil {
				log.Printf("WARNING: unmount failed: %v", err)
			}
		}
		return
	}
}
