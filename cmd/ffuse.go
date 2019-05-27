package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bitbucket.org/creachadair/ffs/blob"
	"bitbucket.org/creachadair/ffs/blob/filestore"
	"bitbucket.org/creachadair/ffs/file"
	"bitbucket.org/creachadair/ffuse"
)

var (
	storeDir   = flag.String("store", "", "Path of blob storage directory")
	mountPoint = flag.String("mount", "", "Path of mount point")
	rootKey    = flag.String("root", "", "If set, the key of the root node")
	doDebug    = flag.Bool("debug", false, "If set, enable debug logging")
)

func main() {
	flag.Parse()
	switch {
	case *storeDir == "":
		log.Fatal("You must set a non-empty -store directory")
	case *mountPoint == "":
		log.Fatal("You must set a non-empty -mount path")
	case *doDebug:
		fuse.Debug = func(msg interface{}) { log.Printf("[ffs] %v", msg) }
		log.Print("Enabled FUSE debug logging")
	}

	// Open a CAS backed by a filestore.
	s, err := filestore.New(*storeDir)
	if err != nil {
		log.Fatalf("Filestore: %v", err)
	}
	cas := blob.NewCAS(s, sha256.New)

	// Open an existing root, or start a new one.
	ctx := context.Background()
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
	} else {
		root = file.New(cas, &file.NewOptions{
			Stat: file.Stat{Mode: os.ModeDir | 0755},
		})
	}

	// Mount the filesystem and serve from our filesystem root.
	server := ffuse.New(root)
	c, err := fuse.Mount(*mountPoint,
		fuse.FSName("ffs"),
		fuse.Subtype("ffs"),
		fuse.VolumeName("FFS"),
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
