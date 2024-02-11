package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/creachadair/ctrl"
	"github.com/creachadair/ffstools/ffs/config"
	"github.com/creachadair/ffuse"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
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
	Verbose   bool          `flag:"v,Enable verbose logging"`
	Exec      bool          `flag:"exec,Execute a command, then unmount and exit"`

	// Fuse library settings.
	Options fs.Options
	Server  *fuse.Server

	// Blob storage.
	Config *config.Settings
	Store  config.CAS
	Path   *config.PathInfo
}

// Logf writes a log message to the standard logger if verbose logging is
// enabled.
func (s *Service) Logf(msg string, args ...any) {
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
		ctrl.Fatalf("You must set a non-empty -mount path")
	case s.RootKey == "":
		ctrl.Fatalf("You must set a non-empty -root pointer key")
	case s.ReadOnly && s.AutoFlush > 0:
		ctrl.Fatalf("You may not enable -auto-flush with -read-only")
	case s.Exec && flag.NArg() == 0:
		ctrl.Fatalf("You must provide a command to execute with -exec")
	}

	var err error

	// Load configuration file.
	s.Config, err = config.Load(config.Path())
	if err != nil {
		ctrl.Fatalf("Loading configuration: %v", err)
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
		s.Options.MountOptions.Logger = log.New(os.Stderr, "FUSE: ", log.LstdFlags|log.Lmicroseconds)
		s.Options.MountOptions.Debug = true
	}

	// Open blob store.
	s.Store, err = s.Config.OpenStore(ctx)
	if err != nil {
		ctrl.Fatalf("Opening blob store: %v", err)
	}

	// Load the root of the filesystem.
	pi, err := config.OpenPath(ctx, s.Store, s.RootKey)
	if err != nil {
		ctrl.Fatalf("Loading root path: %v", err)
	}
	s.Path = pi
	if pi.Root != nil {
		s.Logf("Loaded filesystem from %q (%s)", pi.RootKey, config.FormatKey(pi.FileKey))
		if pi.Root.Description != "" {
			s.Logf("| Description: %q", pi.Root.Description)
		}
	} else {
		s.Logf("Loaded filesystem at %s (no root pointer)", config.FormatKey(pi.FileKey))
	}
}

// Mount establishes a connection for the filesystem mount point and prepares
// the filesystem root for service.
func (s *Service) Mount(ctx context.Context) error {
	if s.ReadOnly {
		s.Options.MountOptions.Options = append(s.Options.MountOptions.Options, "ro")
	}

	var err error
	s.Server, err = fs.Mount(s.MountPath, ffuse.NewFS(s.Path.File), &s.Options)
	if err != nil {
		return err
	} else if err := s.Server.WaitMount(); err != nil {
		return errors.Join(err, s.Server.Unmount())
	}
	return nil
}

// Run mounts the filesystem, if necessary, and starts up background tasks to
// monitor for completion of ctx.
func (s *Service) Run(ctx context.Context) error {
	if s.Server == nil {
		if err := s.Mount(ctx); err != nil {
			return fmt.Errorf("mount: %w", err)
		}
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer cancel()
		s.Server.Wait()
		s.Logf("Server exited")
	}()

	// If we are supposed to auto-flush, set up a task to do that now.
	if s.AutoFlush > 0 {
		s.Logf("Enabling auto-flush every %v", s.AutoFlush)
		go s.autoFlush(ctx, s.AutoFlush)
	}

	// If a subcommand was requested, start it now.
	var errc chan error
	if s.Exec {
		name := flag.Arg(0)
		s.Logf("Starting subprocess %q", name)
		cmd := exec.CommandContext(sctx, name, flag.Args()[1:]...)
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

	select {
	case <-sctx.Done():
		log.Printf("Received signal, unmounting...")
		if err := svc.Server.Unmount(); err != nil {
			log.Printf("WARNING: Unmount failed: %v", err)
		}
		return nil
	case err := <-errc:
		if err != nil {
			log.Printf("Error from subprocess: %v", err)
		}
		if err := svc.Server.Unmount(); err != nil {
			log.Printf("WARNING: Unmount failed: %v", err)
		}
		<-sctx.Done()
		return err
	}
}

func (s *Service) autoFlush(ctx context.Context, d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Logf("Stopping auto-flush routine")
			return
		case <-t.C:
			oldKey := s.Path.BaseKey
			newKey, err := s.Path.Flush(ctx)
			if err != nil {
				log.Printf("WARNING: Error flushing root: %v", err)
			} else if oldKey != newKey {
				s.Logf("Root flushed, storage key is now %s", config.FormatKey(newKey))
			}
		}
	}
}
