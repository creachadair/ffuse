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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/seaweedfs/fuse"
)

var (
	serveAddr = flag.String("listen", "", "Status service address")

	svc = &Service{
		MountOptions: []fuse.MountOption{
			fuse.FSName("ffs"),
			fuse.Subtype("ffs"),
			fuse.VolumeName("FFS"),
			fuse.NoAppleDouble(),
			fuse.MaxReadahead(1 << 16),
		},
	}
)

func init() {
	flag.StringVar(&svc.StoreSpec, "store", os.Getenv("FFS_STORE"), "Blob storage address (required)")
	flag.StringVar(&svc.MountPath, "mount", "", "Path of mount point (required)")
	flag.BoolVar(&svc.ReadOnly, "read-only", false, "Mount the filesystem as read-only")
	flag.BoolVar(&svc.DebugLog, "debug", false, "Enable debug logging (warning: noisy)")
	flag.StringVar(&svc.RootKey, "root", "", "Storage key of root pointer")
	flag.DurationVar(&svc.AutoFlush, "auto-flush", 0, "Automatically flush the root at this interval")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %[1]s [-read-only] -store addr -mount path -root key[/path...]

Mount a FFS filesystem via FUSE at the specified -mount path, using the store
described by addr. The starting point for the mount may be the name of a
root pointer, or a path relative to a root pointer, or a specific storage key
prefixed by "@".

If -store is not set, the FFS_STORE environment variable is used as a default
if it is defined; otherwise the default from the FFS config file is used or an
error is reported.

If -listen is set, an HTTP service is exposed at that address which supports
the following operations:

   GET /status     -- return a JSON blob of filesystem status
   GET /flush      -- as /status, but also flushes the root to storage
   POST /root/:key -- update the filesystem root to the specified key

Updating the filesystem changes what is visible through the mount point.
You can effect a "reload" of the filesystem contents by putting the same value
the filesystem was started with.

Options:
`, filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
}

func main() {
	flag.Parse()
	log.SetPrefix("[ffuse] ")

	ctx := context.Background()
	svc.Init(ctx)

	if *serveAddr != "" {
		log.Printf("Starting status service at %q...", *serveAddr)
		go func() {
			if err := http.ListenAndServe(*serveAddr, svc.Status); err != nil {
				log.Fatalf("HTTP service failed: %v", err)
			}
		}()
	}

	if err := svc.Mount(); err != nil {
		log.Fatalf("Mount failed: %v", err)
	}

	// Set up a context to propagate signals to the serving loop.
	rctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT)
	err := svc.Run(rctx)
	cancel()

	log.Printf("Server exited: %v", err)
	svc.Shutdown(ctx)
}
