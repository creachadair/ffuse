package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/creachadair/ffs/blob"
	"github.com/creachadair/ffs/file"
	"github.com/creachadair/ffstools/ffs/config"
	"github.com/creachadair/ffuse"
	"github.com/seaweedfs/fuse"
	"github.com/seaweedfs/fuse/fs"
)

const (
	debugFFS  = 1
	debugFUSE = 2
)

// Service holds the state of the mounted filesystem.
type Service struct {
	MountPath string        // path of mount point
	RootKey   string        // root or file key
	StoreSpec string        // blob store spec
	ReadOnly  bool          // whether the mount is read-only
	DebugLog  int           // debug logging level
	AutoFlush time.Duration // auto-flush interval (0=disabled)

	// Fuse library settings.
	MountOptions []fuse.MountOption
	FileServer   *fs.Server
	Conn         *fuse.Conn

	// Filesystem settings.
	Options ffuse.Options
	FS      *ffuse.FS
	Status  *http.ServeMux

	// Blob storage.
	Config *config.Settings
	Store  blob.CAS
	Path   atomic.Pointer[config.PathInfo]
}

// Init checks the settings, and loads the initial filesystem state from the
// specified blob store. It terminates the process if any of these steps fail.
func (s *Service) Init(ctx context.Context) {
	// Check flags for consistency.
	if s.MountPath == "" {
		log.Fatal("You must set a non-empty -mount path")
	} else if s.RootKey == "" {
		log.Fatal("You must set a non-empty -root pointer key")
	} else if s.ReadOnly && s.AutoFlush > 0 {
		log.Fatal("You may not enable -auto-flush with -read-only")
	}

	var err error

	// Load configuration file.
	s.Config, err = config.Load(config.Path())
	if err != nil {
		log.Fatalf("Loading configuration: %v", err)
	}
	if s.StoreSpec != "" {
		s.Config.DefaultStore = s.StoreSpec
	} else {
		// Copy the default so it shows up in /status.
		s.StoreSpec = s.Config.DefaultStore
	}
	if s.DebugLog&debugFFS != 0 {
		s.Config.EnableDebugLogging = true
	}
	if s.DebugLog&debugFUSE != 0 {
		fuse.Debug = func(arg any) { log.Printf("FUSE: %v", arg) }
	}

	// Open blob store.
	s.Store, err = s.Config.OpenStore()
	if err != nil {
		log.Fatalf("Opening blob store: %v", err)
	}

	// Load the root of the filesystem.
	pi, err := config.OpenPath(ctx, s.Store, s.RootKey)
	if err != nil {
		log.Fatalf("Loading root path: %v", err)
	}
	s.Path.Store(pi)
	if pi.Root != nil {
		log.Printf("Loaded filesystem from %q (%x)", pi.RootKey, pi.FileKey)
		if pi.Root.Description != "" {
			log.Printf("| Description: %q", pi.Root.Description)
		}
	} else {
		log.Printf("Loaded filesystem at %x (no root pointer)", pi.FileKey)
	}

	s.Status = http.NewServeMux()
	s.Status.HandleFunc("/status", s.handleStatus)
	s.Status.HandleFunc("/flush", s.handleStatus)
	s.Status.HandleFunc("/root/", s.handleRoot)
}

// Mount establishes a connection for the filesystem mount point and prepares
// the filesystem root for service.
func (s *Service) Mount() error {
	opts := s.MountOptions
	if s.ReadOnly {
		opts = append(opts, fuse.ReadOnly())
	}

	var err error
	s.Conn, err = fuse.Mount(s.MountPath, opts...)
	if err != nil {
		return err
	}

	s.FileServer = fs.New(s.Conn, nil)
	s.FS = ffuse.New(s.Path.Load().File, s.FileServer, &s.Options)
	return nil
}

// Run starts the file service and runs until it exits or ctx ends.
func (s *Service) Run(ctx context.Context) error {
	// Start the server running and wait for the connection to be ready.
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- s.FileServer.Serve(s.FS)
	}()

	<-s.Conn.Ready
	if err := s.Conn.MountError; err != nil {
		s.Conn.Close()
		return err
	}

	// If we are supposed to auto-flush, set up a task fto do that now.
	if s.AutoFlush > 0 {
		log.Printf("Enabling auto-flush every %v", s.AutoFlush)
		go s.autoFlush(ctx, s.AutoFlush)
	}

	select {
	case err := <-done:
		return err

	case <-ctx.Done():
		log.Printf("Unmounting %q", s.MountPath)
		if err := fuse.Unmount(s.MountPath); err != nil {
			log.Printf("WARNING: Unmount failed: %v", err)
		}
		return errors.New("terminated by signal")
	}
}

// Shutdown closes the FUSE connection and flushes any unsaved data back out to
// the blob store.
func (s *Service) Shutdown(ctx context.Context) {
	if err := s.Conn.Close(); err != nil {
		log.Printf("WARNING: closing fuse connection failed: %v", err)
	} else {
		log.Print("Closed fuse connection")
	}
	if !s.ReadOnly {
		pi := s.Path.Swap(nil)
		rk, err := pi.Flush(ctx)
		if err != nil {
			blob.CloseStore(ctx, s.Store)
			log.Fatalf("Flushing file data: %v", err)
		}
		fmt.Printf("%x\n", rk)
	}
	blob.CloseStore(ctx, s.Store)
}

func (s *Service) autoFlush(ctx context.Context, d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Print("Stopping auto-flush routine")
			return
		case <-t.C:
			oldKey, newKey, err := s.flushRoot(ctx)
			if err != nil {
				log.Printf("WARNING: Error flushing root: %v", err)
			} else if newKey != oldKey {
				log.Printf("Root flushed, storage key is now %x", newKey)
			}
		}
	}
}

func (s *Service) flushRoot(ctx context.Context) (oldKey, newKey string, err error) {
	// We need to lock out filesystem operations while we do this, since it may
	// update state deeper inside the tree.
	s.FS.WithRoot(func(_ *file.File) {
		pi := s.Path.Load()
		if pi == nil {
			err = errors.New("current path not found")
			return
		}
		oldKey = pi.Root.FileKey
		newKey, err = pi.Flush(ctx)
	})
	return
}

func (s *Service) handleStatus(w http.ResponseWriter, req *http.Request) {
	if req.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	doFlush := req.URL.Path == "/flush"

	var oldKey, newKey string
	if doFlush {
		ok, nk, err := s.flushRoot(req.Context())
		if err != nil {
			log.Printf("WARNING: Error flushing root: %v", err)
		} else if ok != nk {
			oldKey = ok
		}
		newKey = nk
	} else if pi := s.Path.Load(); pi != nil {
		newKey = pi.FileKey
	}

	var autoFlush string
	if s.AutoFlush > 0 {
		autoFlush = s.AutoFlush.Round(time.Second).String()
	}
	writeJSON(w, http.StatusOK, statusReply{
		MountPath:  s.MountPath,
		RootKey:    s.RootKey,
		Store:      s.StoreSpec,
		ReadOnly:   s.ReadOnly,
		AutoFlush:  autoFlush,
		OldKey:     []byte(oldKey),
		StorageKey: []byte(newKey),
	})
}

type statusReply struct {
	MountPath  string `json:"mountPath"`
	RootKey    string `json:"rootKey"`
	Store      string `json:"store"`
	ReadOnly   bool   `json:"readOnly"`
	AutoFlush  string `json:"autoFlush,omitempty"`
	OldKey     []byte `json:"oldKey,omitempty"`
	StorageKey []byte `json:"storageKey"`
}

func (s *Service) handleRoot(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	newRoot := strings.TrimPrefix(req.URL.Path, "/root/")
	if newRoot == "" {
		writeJSON(w, http.StatusBadRequest, "missing new root pointer")
		return
	}
	pi, err := config.OpenPath(req.Context(), s.Store, newRoot)
	if err != nil {
		writeJSON(w, http.StatusNotFound, struct {
			K string `json:"rootKey"`
			E string `json:"error"`
		}{K: newRoot, E: err.Error()})
		return
	}
	s.Path.Store(pi)
	s.FS.Update(pi.File)
	log.Printf("Filesystem root updated to %q (%x)", newRoot, pi.FileKey)
	writeJSON(w, http.StatusOK, struct {
		K string `json:"rootKey"`
		S []byte `json:"storageKey"`
	}{K: newRoot, S: []byte(pi.FileKey)})
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(data)
}
