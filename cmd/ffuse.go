// Program ffuse mounts an FFS filesystem via FUSE.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bitbucket.org/creachadair/ffs/blob"
	"bitbucket.org/creachadair/ffs/blob/filestore"
	"bitbucket.org/creachadair/ffs/blob/memstore"
	"bitbucket.org/creachadair/ffs/blob/store"
	"bitbucket.org/creachadair/ffs/file"
	"bitbucket.org/creachadair/ffuse"
)

// TODO: Add encryption support.
// TODO: Add compression.

var (
	storeAddr  = flag.String("store", "", "Blob storage address (required)")
	mountPoint = flag.String("mount", "", "Path of mount point (required)")
	rootKey    = flag.String("root", "", "If set, the key of the root node (hex encoded)")
	doDebug    = flag.Bool("debug", false, "If set, enable debug logging")
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %[1]s -mount path -store addr [-root key]

Mount a FFS filesystem via FUSE at the specified path, using the blob store
described by addr. If -debug is set, verbose FUSE debug logs are written to
stderr.

Options:
`, filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}

	store.Default.Register("file", filestore.Opener)
	store.Default.Register("mem", memstore.Opener)
}

func main() {
	flag.Parse()
	switch {
	case *storeAddr == "":
		log.Fatal("You must set a non-empty -store address")
	case *mountPoint == "":
		log.Fatal("You must set a non-empty -mount path")
	case *doDebug:
		fuse.Debug = func(msg interface{}) { log.Printf("[ffs] %v", msg) }
		log.Print("Enabled FUSE debug logging")
	}
	ctx := context.Background()

	// Set up the CAS for the filesystem.
	s, err := store.Default.Open(ctx, *storeAddr)
	if err != nil {
		log.Fatalf("Opening blob storage: %v", err)
	}
	cas := blob.NewCAS(s, sha256.New)

	// Open an existing root, or start a fresh one.
	var root *file.File
	if *rootKey != "" {
		rk, err := hex.DecodeString(*rootKey)
		if err != nil {
			log.Fatalf("Invalid root key %q: %v", *rootKey, err)
		}
		root, err = file.Open(ctx, cas, string(rk))
		if err != nil {
			log.Fatalf("Opening root %q: %v", *rootKey, err)
		}
		log.Printf("Loaded filesystem from %q", *rootKey)
	} else {
		root = file.New(cas, &file.NewOptions{
			Stat: file.Stat{Mode: os.ModeDir | 0755},
		})
		log.Print("Creating empty filesystem root")
	}

	// Mount the filesystem and serve from our filesystem root.
	server := ffuse.New(root)
	c, err := fuse.Mount(*mountPoint,
		fuse.FSName("ffs"),
		fuse.Subtype("ffs"),
		fuse.VolumeName("FFS"),
		fuse.NoAppleDouble(),
	)
	if err != nil {
		log.Fatalf("Mount failed: %v", err)
	}
	defer c.Close()

	if err := fs.Serve(c, server); err != nil {
		log.Fatalf("Serve failed: %v", err)
	}

	<-c.Ready
	if err := c.MountError; err != nil {
		log.Fatalf("Mount error: %v", err)
	}
	key, err := root.Flush(ctx)
	if err != nil {
		log.Fatalf("Flush error; %v", err)
	}
	fmt.Println(hex.EncodeToString([]byte(key)))
}
