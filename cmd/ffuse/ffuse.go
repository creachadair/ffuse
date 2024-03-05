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
	"time"

	"github.com/creachadair/ctrl"
	"github.com/creachadair/ffstools/ffs/config"
	"github.com/creachadair/ffuse/driver"
	"github.com/creachadair/flax"
)

var svc = &driver.Service{Options: fuseOptions}

var flags struct {
	MountPath string        `flag:"mount,Path of mount point (required)"`
	RootKey   string        `flag:"root,Root path or @file-key of filesystem root (required)"`
	StoreSpec string        `flag:"store,default=$FFS_STORE,Blob storage address"`
	ReadOnly  bool          `flag:"read-only,Mount the filesystem as read-only"`
	DebugLog  int           `flag:"debug,Set debug logging level (1=ffs, 2=fuse, 3=both)"`
	AutoFlush time.Duration `flag:"auto-flush,Automatically flush the root at this interval"`
	Verbose   bool          `flag:"v,Enable verbose logging"`
	Exec      bool          `flag:"exec,Execute a command, then unmount and exit"`
}

func init() {
	flax.MustBind(flag.CommandLine, &flags)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %[1]s [--read-only] --store addr --mount path --root key[/path...]

Mount a FFS filesystem via FUSE at the specified -mount path, using the store
described by addr. The starting point for the mount may be the name of a
root pointer, or a path relative to a root pointer, or a specific storage key
prefixed by "@".

If --store is not set, the FFS_STORE environment variable is used as a default
if it is defined; otherwise the default from the FFS config file is used or an
error is reported.

If --exec is set, the non-flag arguments remaining on the command line are
executed as a subprocess with the current working directory set to the mount
point, and when the subprocess exits the filesystem is unmounted. The stdin
of %[1]s is piped to the subprocess, and the subprocess's stdout and stderr
are routed to the stdout and stderr of %[1]s.

Options:
`, filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
}

func main() {
	flag.Parse()
	log.SetPrefix("[ffuse] ")

	svc.MountPath = flags.MountPath
	svc.RootKey = flags.RootKey
	svc.StoreSpec = flags.StoreSpec
	svc.ReadOnly = flags.ReadOnly
	svc.DebugLog = flags.DebugLog
	svc.AutoFlush = flags.AutoFlush
	svc.Verbose = flags.Verbose
	svc.Exec = flags.Exec
	svc.ExecArgs = flag.Args()

	ctrl.Run(func() error {
		ctx := context.Background()
		if err := svc.Init(ctx); err != nil {
			return err
		}
		defer svc.Store.Close(ctx)

		// Set up a context to propagate signals to the serving loop.
		rctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT)
		defer cancel()

		if err := svc.Run(rctx); err != nil {
			// The filesystem failed, so don't overwrite the root with changes.
			// But do give the user feedback about the latest state.
			if key, err := svc.Path.Base.Flush(ctx); err == nil {
				fmt.Printf("state: %s\n", config.FormatKey(key))
			} else {
				log.Printf("WARNING: Flushing file state failed: %v", err)
			}
			ctrl.Fatalf("Filesystem failed: %v", err)
		}

		if !svc.ReadOnly {
			rk, err := svc.Path.Flush(ctx)
			if err != nil {
				ctrl.Fatalf("Flushing file data: %v", err)
			}
			fmt.Printf("%s\n", config.FormatKey(rk))
		}
		return nil
	})
}
