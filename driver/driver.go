// Package driver implements a control interface to mount and unmount
// a FFS filesystem via FUSE.
package driver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/creachadair/ffstools/ffs/config"
	"github.com/creachadair/ffuse"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const (
	debugFFS  = 1
	debugFUSE = 2
)

// Service manages the mounting, unmounting, and service of requests for a FUSE
// filesystem. The caller must populate the fields marked as required.
//
// Before a Service can be used, it must be initialized:
//
//	if err := svc.Init(ctx); err != nil {
//	   log.Fatal("Initialization failed: %v", err)
//	}
//
// Once Init succeeds, the service is ready. Call Run to mount and serve FUSE
// requests for the filesystem:
//
//	err := svc.Run(ctx)
//
// Run blocks until ctx ends or until the subprocess specified by its arguments
// has exited, unmounts the filesystem, and reports its status. The caller is
// responsible for flushing out the final state of the filesystem, which can be
// recovered from the PathInfo field.
//
// Run mounts the fileysystem if it has not already been done, but if you need
// to perform tasks after mounting, you may call Mount separately before Run.
type Service struct {
	MountPath string // required
	RootKey   string // required
	StoreSpec string // optional
	ReadOnly  bool
	DebugLog  int
	AutoFlush time.Duration
	Verbose   bool
	Exec      bool
	ExecArgs  []string // command arguments, required if --exec is true

	// Logf, if set, is used as the target for log output.  If nil, the service
	// uses log.Printf. To suppress all log output, populate a no-op function.
	Logf func(string, ...any)

	// Store, if set, is used as the blob storage for filesystem operations.  In
	// that case, no configuration file is loaded. Otherwise Store will be
	// populated based on the config file.
	Store config.CAS

	// Config, if set, is used to locate blob storage settings.  If nil, it is
	// set by Init if Store is not set.  If Store is set, Config is ignored.
	Config *config.Settings

	// Path is set by Init to the path info for the filesystem root.
	Path *config.PathInfo

	// Fuse library settings.
	Options fs.Options
	Server  *fuse.Server // populated by Mount or Run
}

func (s *Service) logPrintf(msg string, args ...any) {
	if s.Logf == nil {
		log.Printf(msg, args...)
	} else {
		s.Logf(msg, args...)
	}
}

// vlogf writes a log message to the standard logger if verbose logging is
// enabled.
func (s *Service) vlogf(msg string, args ...any) {
	if s.Verbose || !s.Exec {
		s.logPrintf(msg, args...)
	}
}

// Init checks the settings, and loads the initial filesystem state from the
// specified blob store. It terminates the process if any of these steps fail.
func (s *Service) Init(ctx context.Context) error {
	// Check flags for consistency.
	switch {
	case s.MountPath == "":
		return errors.New("missing mount path")
	case s.RootKey == "":
		return errors.New("missing root key")
	case s.ReadOnly && s.AutoFlush > 0:
		return errors.New("cannot combine read-only with auto-flush")
	case s.Exec && len(s.ExecArgs) == 0:
		return errors.New("missing exec command")
	}

	// If s does not yet have a Store, load the configuration file and populate
	// one based on the StoreSpec.
	if s.Store == (config.CAS{}) {
		if s.Config == nil {
			cf, err := config.Load(config.Path())
			if err != nil {
				return fmt.Errorf("load configuration: %w", err)
			}
			s.Config = cf
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

		st, err := s.Config.OpenStore(ctx)
		if err != nil {
			return fmt.Errorf("opening blob store: %w", err)
		}
		s.Store = st
	}

	// Load the root of the filesystem.
	pi, err := config.OpenPath(ctx, s.Store, s.RootKey)
	if err != nil {
		s.Store.Close(ctx)
		return fmt.Errorf("load root path: %w", err)
	}
	s.Path = pi
	if pi.Root != nil {
		s.vlogf("Loaded filesystem from %q (%s)", pi.RootKey, config.FormatKey(pi.FileKey))
		if pi.Root.Description != "" {
			s.vlogf("| Description: %q", pi.Root.Description)
		}
	} else {
		s.vlogf("Loaded filesystem at %s (no root pointer)", config.FormatKey(pi.FileKey))
	}

	// If requested, hook up a logger for the FUSE internals (very noisy).
	if s.DebugLog&debugFUSE != 0 {
		s.Options.MountOptions.Logger = log.New(os.Stderr, "FUSE: ", log.LstdFlags|log.Lmicroseconds)
		s.Options.MountOptions.Debug = true
	}

	return nil
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
		s.vlogf("Server exited")
	}()

	// If we are supposed to auto-flush, set up a task to do that now.
	if s.AutoFlush > 0 {
		s.vlogf("Enabling auto-flush every %v", s.AutoFlush)
		go s.autoFlush(ctx, s.AutoFlush)
	}

	// If a subcommand was requested, start it now.
	var errc chan error
	if s.Exec {
		name := s.ExecArgs[0]
		s.vlogf("Starting subprocess %q", name)
		cmd := exec.CommandContext(sctx, name, s.ExecArgs[1:]...)
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
		s.logPrintf("Received signal, unmounting...")
		if err := s.Server.Unmount(); err != nil {
			s.logPrintf("WARNING: Unmount failed: %v", err)
		}
		return nil
	case err := <-errc:
		if err != nil {
			s.logPrintf("Error from subprocess: %v", err)
		}
		if err := s.Server.Unmount(); err != nil {
			s.logPrintf("WARNING: Unmount failed: %v", err)
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
			s.vlogf("Stopping auto-flush routine")
			return
		case <-t.C:
			oldKey := s.Path.BaseKey
			newKey, err := s.Path.Flush(ctx)
			if err != nil {
				s.logPrintf("WARNING: Error flushing root: %v", err)
			} else if oldKey != newKey {
				s.vlogf("Root flushed, storage key is now %s", config.FormatKey(newKey))
			}
		}
	}
}
