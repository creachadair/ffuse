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

	"github.com/creachadair/ctrl"
	"github.com/creachadair/ffstools/ffs/config"
	"github.com/creachadair/flax"
)

var svc = &Service{Options: fuseOptions}

func init() {
	flax.MustBind(flag.CommandLine, svc)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %[1]s [-read-only] -store addr -mount path -root key[/path...]

Mount a FFS filesystem via FUSE at the specified -mount path, using the store
described by addr. The starting point for the mount may be the name of a
root pointer, or a path relative to a root pointer, or a specific storage key
prefixed by "@".

If -store is not set, the FFS_STORE environment variable is used as a default
if it is defined; otherwise the default from the FFS config file is used or an
error is reported.

If -exec is set, the non-flag arguments remaining on the command line are
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

	ctrl.Run(func() error {
		ctx := context.Background()
		svc.Init(ctx)
		defer svc.Store.Close(ctx)

		// Set up a context to propagate signals to the serving loop.
		rctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT)
		defer cancel()

		if err := svc.Run(rctx); err != nil {
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
