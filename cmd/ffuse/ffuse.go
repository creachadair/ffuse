// Copyright (C) 2019 Michael J. Fromberger. All Rights Reserved.

// Program ffuse mounts an FFS filesystem via FUSE.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/creachadair/ffs/blob"
	"github.com/creachadair/ffs/file"
	"github.com/creachadair/ffs/file/root"
	"github.com/creachadair/ffuse"
	"github.com/creachadair/jrpc2"
	"github.com/creachadair/jrpc2/channel"
	"github.com/creachadair/rpcstore"

	"github.com/seaweedfs/fuse"
	"github.com/seaweedfs/fuse/fs"
)

var (
	storeAddr  = flag.String("store", os.Getenv("BLOB_STORE"), "Blob storage address (required)")
	mountPoint = flag.String("mount", "", "Path of mount point (required)")
	doDebug    = flag.Bool("debug", false, "If set, enable debug logging")
	doReadOnly = flag.Bool("read-only", false, "Mount the filesystem as read-only")
	rootKey    = flag.String("root", "", "Storage key of root pointer")
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %[1]s -mount path -store addr -root key
       %[1]s -root key -new "Description"

Mount a FFS filesystem via FUSE at the specified path, using the blob store
described by addr. If -debug is set, verbose FUSE debug logs are written to
stderr.

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
	case *storeAddr == "":
		log.Fatal("You must set a non-empty -store address")
	case *mountPoint == "":
		log.Fatal("You must set a non-empty -mount path")
	case *rootKey == "":
		log.Fatal("You must set a non-empty -root pointer key")
	}

	if !strings.HasPrefix(*rootKey, "root:") {
		*rootKey = "root:" + *rootKey
	}

	copts := new(jrpc2.ClientOptions)
	if *doDebug {
		fuse.Debug = func(msg interface{}) { log.Printf("[ffs] %v", msg) }
		log.Print("Enabled FUSE debug logging")
		copts.Logger = log.New(os.Stderr, "[rpcstore] ", log.LstdFlags)
		log.Print("Enabled storage client logging")
	}

	ctx := context.Background()

	// Set up the CAS for the filesystem.
	conn, err := net.Dial(jrpc2.Network(*storeAddr))
	if err != nil {
		log.Fatalf("Dialing blob server: %v", err)
	}
	defer conn.Close()
	cas := rpcstore.NewCAS(jrpc2.NewClient(channel.Line(conn, conn), copts), nil)
	defer blob.CloseStore(ctx, cas)

	// Load the designated root and extract its file.
	rootPointer, err := root.Open(ctx, cas, *rootKey)
	if err != nil {
		log.Fatalf("Loading root pointer: %v", err)
	}
	rootFile, err := rootPointer.File(ctx)
	if err != nil {
		log.Fatalf("Loading root file: %v", err)
	}
	log.Printf("Loaded filesystem from %q (%x)", *rootKey, rootPointer.FileKey)
	if rootPointer.Description != "" {
		log.Printf("| Description: %s", rootPointer.Description)
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
	fsys := ffuse.New(rootFile, server)
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
	flushRoot(ctx, rootFile, rootPointer)
}

func flushRoot(ctx context.Context, rf *file.File, rp *root.Root) {
	// At exit, flush and update the root pointer.
	key, err := rf.Flush(ctx)
	if err != nil {
		log.Fatalf("Flushing root: %v", err)
	}
	if key != rp.FileKey {
		rp.IndexKey = "" // invalidate the index, if there is one
	}
	rp.FileKey = key
	if err := rp.Save(ctx, *rootKey, true); err != nil {
		log.Fatalf("Updating root pointer: %v", err)
	}
	fmt.Printf("%x\n", key)
}
