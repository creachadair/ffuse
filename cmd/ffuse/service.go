package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/creachadair/ffs/file"
	"github.com/creachadair/ffs/file/root"
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
	MountPath string        `flag:"mount,Path of mount point (required)"`
	RootKey   string        `flag:"root,Storage key of root pointer"`
	StoreSpec string        `flag:"store,default=$FFS_STORE,Blob storage address (required)"`
	ReadOnly  bool          `flag:"read-only,Mount the filesystem as read-only"`
	DebugLog  int           `flag:"debug,Set debug logging level (1=ffs, 2=fuse, 3=both)"`
	AutoFlush time.Duration `flag:"auto-flush,Automatically flush the root at this interval"`

	// Fuse library settings.
	MountOptions []fuse.MountOption
	FileServer   *fs.Server
	Conn         *fuse.Conn

	// Filesystem settings.
	Options ffuse.Options
	FS      *ffuse.FS
	Status  *http.ServeMux

	// Execution settings.
	Exec    bool `flag:"exec,Execute a command, then unmount and exit"`
	Verbose bool `flag:"v,Enable verbose logging"`
	Args    []string

	// Blob storage.
	Config *config.Settings
	Store  config.CAS
	Path   atomic.Pointer[config.PathInfo]
}

func (s *Service) logf(msg string, args ...any) {
	if s.Verbose || !s.Exec {
		log.Printf(msg, args...)
	}
}

// Init checks the settings, and loads the initial filesystem state from the
// specified blob store. It terminates the process if any of these steps fail.
func (s *Service) Init(ctx context.Context) {
	// Check flags for consistency.
	switch {
	case s.MountPath == "":
		log.Fatal("You must set a non-empty -mount path")
	case s.RootKey == "":
		log.Fatal("You must set a non-empty -root pointer key")
	case s.ReadOnly && s.AutoFlush > 0:
		log.Fatal("You may not enable -auto-flush with -read-only")
	case s.Exec && len(s.Args) == 0:
		log.Fatal("You must provide a command to execute with -exec")
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
		fuse.Debug = func(arg any) { s.logf("FUSE: %v", arg) }
	}

	// Open blob store.
	s.Store, err = s.Config.OpenStore(ctx)
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
		s.logf("Loaded filesystem from %q (%s)", pi.RootKey, config.FormatKey(pi.FileKey))
		if pi.Root.Description != "" {
			s.logf("| Description: %q", pi.Root.Description)
		}
	} else {
		s.logf("Loaded filesystem at %s (no root pointer)", config.FormatKey(pi.FileKey))
	}

	s.Status = http.NewServeMux()
	s.Status.HandleFunc("/status", s.handleStatus)
	s.Status.HandleFunc("/flush", s.handleStatus)
	s.Status.HandleFunc("/root/", s.handleRoot)
	s.Status.HandleFunc("/snapshot/", s.handleSnapshot)
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

	// If we are supposed to auto-flush, set up a task to do that now.
	if s.AutoFlush > 0 {
		s.logf("Enabling auto-flush every %v", s.AutoFlush)
		go s.autoFlush(ctx, s.AutoFlush)
	}

	// If we are supposed to execute a command, do that now.
	var errc chan error
	if s.Exec {
		name := s.Args[0]
		s.logf("Starting subprocess %q", name)
		cmd := exec.CommandContext(ctx, name, s.Args[1:]...)
		cmd.Dir = s.MountPath
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		errc = make(chan error, 1)
		go func() {
			defer close(errc)
			errc <- cmd.Run()
		}()
	}

	unmount := func() {
		s.logf("Unmounting %q", s.MountPath)
		if err := fuse.Unmount(s.MountPath); err != nil {
			log.Printf("WARNING: Unmount failed: %v", err)
		}
	}
	select {
	case err := <-done:
		return err

	case err := <-errc:
		unmount()
		return err

	case <-ctx.Done():
		unmount()
		return errors.New("terminated by signal")
	}
}

// Shutdown closes the FUSE connection and flushes any unsaved data back out to
// the blob store.
func (s *Service) Shutdown(ctx context.Context) {
	if err := s.Conn.Close(); err != nil {
		log.Printf("WARNING: closing fuse connection failed: %v", err)
	} else {
		s.logf("Closed fuse connection")
	}
	if !s.ReadOnly {
		pi := *s.Path.Load()
		rk, err := pi.Flush(ctx)
		if err != nil {
			s.Store.Close(ctx)
			log.Fatalf("Flushing file data: %v", err)
		}
		fmt.Printf("%s\n", config.FormatKey(rk))
	}
	s.Store.Close(ctx)
}

func (s *Service) autoFlush(ctx context.Context, d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logf("Stopping auto-flush routine")
			return
		case <-t.C:
			oldKey := s.Path.Load().BaseKey
			newKey, err := s.flushRoot(ctx)
			if err != nil {
				log.Printf("WARNING: Error flushing root: %v", err)
			} else if oldKey != newKey {
				s.logf("Root flushed, storage key is now %s", config.FormatKey(newKey))
			}
		}
	}
}

func (s *Service) flushRoot(ctx context.Context) (newKey string, err error) {
	// We need to lock out filesystem operations while we do this, since it may
	// update state deeper inside the tree.
	s.FS.WithRoot(func(_ *file.File) {
		newKey, err = s.Path.Load().Flush(ctx)
	})
	return
}

func (s *Service) handleStatus(w http.ResponseWriter, req *http.Request) {
	if req.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	doFlush := req.URL.Path == "/flush"

	storageKey := s.Path.Load().BaseKey
	var oldKey string
	if doFlush {
		if nk, err := s.flushRoot(req.Context()); err != nil {
			log.Printf("WARNING: Error flushing root: %v", err)
		} else if nk != storageKey {
			oldKey, storageKey = storageKey, nk
		}
	}

	var autoFlush string
	if s.AutoFlush > 0 {
		autoFlush = s.AutoFlush.Round(time.Second).String()
	}
	writeJSON(w, http.StatusOK, makeOpReply("status", statusReply{
		MountPath:  s.MountPath,
		Root:       effectiveRootKey(s.Path.Load()),
		Store:      s.StoreSpec,
		ReadOnly:   s.ReadOnly,
		AutoFlush:  autoFlush,
		OldKey:     []byte(oldKey),
		StorageKey: []byte(storageKey),
	}))
}

type rootReply struct {
	R string `json:"root"`
	S []byte `json:"storageKey"`
}

type errorReply struct {
	Err string `json:"error"`
}

func errorFmt(op string, msg string, args ...any) opReply {
	return makeOpReply(op, errorReply{Err: fmt.Sprintf(msg, args...)})
}

type opReply map[string]any

func makeOpReply(op string, v any) opReply { return opReply{op: v} }

type statusReply struct {
	MountPath  string `json:"mountPath"`
	Root       any    `json:"root"`
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
		writeJSON(w, http.StatusBadRequest, errorFmt("root", "missing new root pointer"))
		return
	}
	pi, err := config.OpenPath(req.Context(), s.Store, newRoot)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorFmt("root", err.Error()))
		return
	}
	s.Path.Store(pi)
	s.FS.Update(pi.File)
	s.logf("Filesystem root updated to %q (%s)", newRoot, config.FormatKey(pi.FileKey))
	writeJSON(w, http.StatusOK, makeOpReply("root", rootReply{
		R: newRoot, S: []byte(pi.FileKey),
	}))
}

func (s *Service) handleSnapshot(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	doReplace, _ := strconv.ParseBool(req.URL.Query().Get("replace"))
	newSnap := strings.TrimPrefix(req.URL.Path, "/snapshot/")
	if newSnap == "" {
		writeJSON(w, http.StatusBadRequest, errorFmt("snapshot", "missing snapshot name"))
		return
	}
	newKey, err := s.flushRoot(req.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorFmt("snapshot", err.Error()))
		return
	}
	pi := s.Path.Load()
	start := pi.RootKey
	if start == "" {
		start = pi.BaseKey
	}
	nr := root.New(s.Store.Roots(), &root.Options{
		FileKey:     newKey,
		Description: "Triggered snapshot of " + start,
	})
	if err := nr.Save(req.Context(), newSnap, doReplace); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorFmt("snapshot", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, makeOpReply("snapshot", rootReply{
		R: newSnap, S: []byte(newKey),
	}))
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

func effectiveRootKey(pi *config.PathInfo) any {
	if pi.RootKey == "" {
		return []byte(pi.BaseKey)
	}
	return pi.RootKey
}
