// Program ffuse mounts an FFS filesystem via FUSE.
package main

import (
	"context"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"hash"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/creachadair/ffs/blob"
	"github.com/creachadair/ffs/blob/badgerstore"
	"github.com/creachadair/ffs/blob/encrypted"
	"github.com/creachadair/ffs/blob/filestore"
	"github.com/creachadair/ffs/blob/memstore"
	"github.com/creachadair/ffs/blob/store"
	"github.com/creachadair/ffs/file"
	"github.com/creachadair/ffuse"
	"github.com/creachadair/getpass"
	"github.com/creachadair/keyfile"
)

// TODO: Add compression.

var (
	storeAddr  = flag.String("store", "", "Blob storage address (required)")
	mountPoint = flag.String("mount", "", "Path of mount point (required)")
	rootKey    = flag.String("root", "", "If set, the key of the root node (hex encoded)")
	doDebug    = flag.Bool("debug", false, "If set, enable debug logging")
	keyFile    = flag.String("keyfile", os.Getenv("KEYFILE_PATH"), "Path of encryption key file")
	doEncrypt  = flag.String("encrypt", "", "Enable encryption with this key slug")
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

	store.Default.Register("badger", badgerstore.Opener)
	store.Default.Register("file", filestore.Opener)
	store.Default.Register("mem", memstore.Opener)
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
	digest := sha256.New

	if *doEncrypt != "" {
		if *keyFile == "" {
			log.Fatal("No -keyfile path was specified")
		}
		pp, err := getpass.Prompt("Passphrase for " + *doEncrypt + ": ")
		if err != nil {
			log.Fatalf("Reading passphrase: %v", err)
		}
		key, err := keyfile.LoadKey(os.ExpandEnv(*keyFile), *doEncrypt, pp)
		if err != nil {
			log.Fatalf("Loading encryption key: %v", err)
		}
		c, err := aes.NewCipher(key)
		if err != nil {
			log.Fatalf("Creating cipher: %v", err)
		}
		s = encrypted.New(s, c, nil)
		digest = func() hash.Hash {
			return hmac.New(sha256.New, key)
		}
		log.Printf("Enabled encryption with key %q", *doEncrypt)
	}
	cas := blob.NewCAS(s, digest)

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
	done := make(chan error)
	go func() { defer close(done); done <- fs.Serve(c, server) }()

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

	// TODO: Put the root somewhere persistent.
	key, err := root.Flush(ctx)
	if err != nil {
		log.Fatalf("Flush error: %v", err)
	}
	fmt.Println(hex.EncodeToString([]byte(key)))
}
