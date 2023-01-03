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

// Package ffuse implements a FUSE filesystem driver backed by the flexible
// filesystem package (github.com/creachadair/ffs). It is compatible with
// the github.com/seaweedfs/fuse and github.com/seaweedfs/fuse/fs packages.
package ffuse

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/creachadair/ffs/file"
	"github.com/seaweedfs/fuse"
	"github.com/seaweedfs/fuse/fs"
)

// New constructs a new FS with the given root file.  The resulting value is
// safe for concurrent use by multiple goroutines.
func New(root *file.File, server *fs.Server, opts *Options) *FS {
	fs := &FS{root: root, server: server}
	fs.rnode = &Node{fs: fs, file: fs.root}
	return fs
}

// WithRoot calls f with the root file of the filesystem, while holding the
// filesystem lock exclusively.
func (fs *FS) WithRoot(f func(*file.File)) {
	inv := fs.newInvalSet()
	defer inv.process()

	fs.μ.Lock()
	defer fs.μ.Unlock()
	inv.attr(fs.rnode)
	f(fs.root)
}

// Update replaces the root of the filesystem with root and invalidates the
// attribute cache for the root.
func (fs *FS) Update(newRoot *file.File) {
	inv := fs.newInvalSet()
	defer inv.process()

	fs.μ.Lock()
	defer fs.μ.Unlock()
	fs.rnode.file = newRoot
	inv.attr(fs.rnode)
}

// Options control optional settings for a FS. A nil *Options is valid and
// provides default values as described.
type Options struct{}

// FS implements the fs.FS interface.
type FS struct {
	// All operations on any node of the filesystem must hold μ.
	// Operations that modify the contents of the tree must hold a write lock.
	μ      sync.RWMutex
	root   *file.File
	rnode  *Node
	server *fs.Server
}

// Root implements the fs.FS interface.
func (fs *FS) Root() (fs.Node, error) {
	fs.μ.RLock()
	defer fs.μ.RUnlock()
	return fs.rnode, nil
}

// A Node implements the fs.Node interface along with other node-related
// interfaces from the github.com/seaweedfs/fuse/fs package.
type Node struct {
	fs   *FS
	file *file.File
}

// Verify interface satisfactions.
var (
	_ fs.FS                  = (*FS)(nil)
	_ fs.Node                = Node{}
	_ fs.NodeCreater         = Node{}
	_ fs.NodeFsyncer         = Node{}
	_ fs.NodeGetxattrer      = Node{}
	_ fs.NodeLinker          = Node{}
	_ fs.NodeListxattrer     = Node{}
	_ fs.NodeMkdirer         = Node{}
	_ fs.NodeOpener          = Node{}
	_ fs.NodeReadlinker      = Node{}
	_ fs.NodeRemover         = Node{}
	_ fs.NodeRenamer         = Node{}
	_ fs.NodeRequestLookuper = Node{}
	_ fs.NodeSetattrer       = Node{}
	_ fs.NodeSetxattrer      = Node{}
	_ fs.NodeSymlinker       = Node{}
	_ fs.HandleFlusher       = (*Handle)(nil)
	_ fs.HandleReadDirAller  = (*Handle)(nil)
	_ fs.HandleReader        = (*Handle)(nil)
	_ fs.HandleReleaser      = (*Handle)(nil)
	_ fs.HandleWriter        = (*Handle)(nil)
)

// Attr implements fs.Node.
func (n Node) Attr(ctx context.Context, attr *fuse.Attr) error {
	return n.readLock(func() error {
		n.fillAttr(attr)
		return nil
	})
}

// fillAttr populates the fields of attr with stat metadata from the file in n.
// The caller must hold the filesystem lock.
func (n Node) fillAttr(attr *fuse.Attr) {
	attr.Inode = fileInode(n.file)

	nb := n.file.Size()
	attr.Size = uint64(nb)
	attr.Blocks = uint64((nb + 511) / 512)

	s := n.file.Stat()
	attr.Mode = s.Mode
	attr.Mtime = s.ModTime
	attr.Uid = uint32(s.OwnerID)
	attr.Gid = uint32(s.GroupID)
	attr.Nlink = 1
	if s.Mode.IsDir() {
		attr.Nlink = uint32(2 + n.file.Child().Len())
	}
}

// touchIfOK updates the last-modified timestamp of the file in n, if err == nil.
// The caller must hold the filesystem write lock.
func (n Node) touchIfOK(err error) {
	if err == nil {
		n.file.Stat().Edit(func(stat *file.Stat) {
			stat.ModTime = time.Now()
		}).Update()
	}
}

// Create implements fs.NodeCreater.
func (n Node) Create(ctx context.Context, req *fuse.CreateRequest, rsp *fuse.CreateResponse) (node fs.Node, handle fs.Handle, err error) {
	err = n.writeLock(func() error {
		f, err := n.file.Open(ctx, req.Name)
		if err == nil {
			// The file already exists; if O_EXCL is set the request fails (EEXIST).
			if req.Flags&fuse.OpenExclusive != 0 {
				return fuse.EEXIST
			}
		} else if !errors.Is(err, file.ErrChildNotFound) {
			return err
		} else {
			// The file doesn't exist; create a new empty file or directory.
			f = n.file.New(&file.NewOptions{
				Name: req.Name,
				Stat: &file.Stat{
					Mode:    req.Mode,
					OwnerID: int(req.Uid),
					GroupID: int(req.Gid),
				},
			})
			n.file.Child().Set(req.Name, f)
		}
		defer n.touchIfOK(nil)

		// If the request wants the file truncated, do that now.
		if req.Flags&fuse.OpenTruncate != 0 {
			if err := f.Truncate(ctx, 0); err != nil {
				return err
			}
		}

		// Now all is well, and we can safely return a file.
		fnode := Node{fs: n.fs, file: f}
		fnode.fillAttr(&rsp.Attr)
		defer fnode.touchIfOK(nil)

		node = fnode
		handle = &Handle{
			Node:     fnode,
			writable: !req.Flags.IsReadOnly(),
			append:   req.Flags&fuse.OpenAppend != 0,
		}
		return nil
	})
	return
}

// Fsync implements fs.NodeFsyncer.
func (n Node) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	return n.writeLock(func() error {
		// With FFS it is not possible to sync metadata separately from data, so
		// this implementation ignores the datasync bit.
		_, err := n.file.Flush(ctx)
		return err
	})
}

const (
	ffsStorageKey    = "ffs.storageKey"
	ffsStorageKeyHex = ffsStorageKey + ".hex"
)

// Getxattr implements fs.NodeGetxattrer. Each node has a synthesized xattr
// called "ffs.storageKey" that returns the storage key for the node. Reading
// the attribute implicitly flushes the target node to storage.
func (n Node) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, rsp *fuse.GetxattrResponse) error {
	// Reading the storage key requires a write lock so we can flush.
	if req.Name == ffsStorageKey || req.Name == ffsStorageKeyHex {
		return n.writeLock(func() error {
			key, err := n.file.Flush(ctx)
			if err == nil {
				if req.Name == ffsStorageKeyHex {
					key = hex.EncodeToString([]byte(key))
				}
				if cap := int(req.Size); cap > 0 && cap < len(key) {
					key = key[:cap]
				}
				rsp.Xattr = []byte(key)
			}
			return err
		})
	}

	// Other attributes require only a read lock.
	return n.readLock(func() error {
		val, ok := n.file.XAttr().Get(req.Name)
		if !ok {
			return xattrErrnoNotFound
		}
		if cap := int(req.Size); cap > 0 && cap < len(val) {
			val = val[:cap]
		}
		rsp.Xattr = []byte(val)
		return nil
	})
}

// Link implements fs.NodeLinker.
func (n Node) Link(ctx context.Context, req *fuse.LinkRequest, old fs.Node) (node fs.Node, err error) {
	inv := n.fs.newInvalSet()
	defer inv.process()
	err = n.writeLock(func() error {
		if n.file.Child().Has(req.NewName) {
			return fuse.EEXIST
		}
		tgt := old.(Node)
		if tgt.file.Stat().Mode.IsDir() {
			return fuse.EPERM
		}

		n.file.Child().Set(req.NewName, tgt.file)
		inv.entry(n, req.NewName)
		inv.attr(n)
		node = tgt
		defer n.touchIfOK(nil)
		return nil
	})
	return
}

// Listxattr implements fs.NodeListxattrer.
func (n Node) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, rsp *fuse.ListxattrResponse) error {
	cap := int(req.Size)
	add := func(name string) {
		if cap == 0 || len(rsp.Xattr)+len(name) < cap {
			rsp.Append(name)
		}
	}

	// TODO: Should we include the storage key entries in the list?  I did at
	// first, but then it complicates command-line usage because they're magic.
	// So for now I've removed them.
	//
	//add(ffsStorageKey)
	//add(ffsStorageKeyHex)

	return n.readLock(func() error {
		n.file.XAttr().List(func(key, _ string) {
			add(key)
		})
		return nil
	})
}

// Lookup implements fs.NodeRequestLookuper.
func (n Node) Lookup(ctx context.Context, req *fuse.LookupRequest, rsp *fuse.LookupResponse) (node fs.Node, err error) {
	// N.B. This requires a write lock because paging in children updates caches.
	err = n.writeLock(func() error {
		f, err := n.file.Open(ctx, req.Name)
		if errors.Is(err, file.ErrChildNotFound) {
			return fuse.ENOENT
		} else if err != nil {
			return err
		}

		fnode := Node{fs: n.fs, file: f}
		fnode.fillAttr(&rsp.Attr)
		node = fnode
		return nil
	})
	return
}

// Mkdir implements fs.NodeMkdirer.
func (n Node) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (node fs.Node, err error) {
	err = n.writeLock(func() error {
		if n.file.Child().Has(req.Name) {
			return fuse.EEXIST
		}
		defer n.touchIfOK(nil)
		f := n.file.New(&file.NewOptions{
			Name: req.Name,
			Stat: &file.Stat{
				Mode:    req.Mode,
				ModTime: time.Now(),
				OwnerID: int(req.Uid),
				GroupID: int(req.Gid),
			},
		})
		n.file.Child().Set(req.Name, f)
		node = Node{fs: n.fs, file: f}
		return nil
	})
	return
}

// Open implements fs.NodeOpener.
func (n Node) Open(ctx context.Context, req *fuse.OpenRequest, rsp *fuse.OpenResponse) (handle fs.Handle, err error) {
	err = n.readLock(func() error {
		handle = &Handle{
			Node:     n,
			writable: !req.Flags.IsReadOnly(),
			append:   req.Flags&fuse.OpenAppend != 0,
		}
		return nil
	})
	return
}

// Readlink implements fs.NodeReadlinker.
func (n Node) Readlink(ctx context.Context, req *fuse.ReadlinkRequest) (target string, err error) {
	err = n.readLock(func() error {
		buf := make([]byte, int(n.file.Size()))
		if _, err := n.file.ReadAt(ctx, buf, 0); err != nil {
			return err
		}
		target = string(buf)
		return nil
	})
	return
}

// Remove implements fs.NodeRemover.
func (n Node) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	inv := n.fs.newInvalSet()
	defer inv.process()
	return n.writeLock(func() error {
		f, err := n.file.Open(ctx, req.Name)
		if errors.Is(err, file.ErrChildNotFound) {
			return fuse.ENOENT
		} else if err != nil {
			return err
		}

		if f.Stat().Mode.IsDir() {
			if !req.Dir {
				return fuse.EPERM // unlink(directory)
			} else if f.Child().Len() != 0 {
				return fuse.Errno(syscall.ENOTEMPTY)
			}
		} else if req.Dir {
			return fuse.EPERM // rmdir(non-directory)
		}
		n.file.Child().Remove(req.Name)
		inv.entry(n, req.Name)
		inv.attr(n)
		return nil
	})
}

// Removexattr implements fs.NodeRemovexattrer.
func (n Node) Removexattr(ctx context.Context, req *fuse.RemovexattrRequest) error {
	if req.Name == ffsStorageKey || req.Name == ffsStorageKeyHex {
		return fuse.EPERM // these are read-only
	}
	return n.writeLock(func() error {
		x := n.file.XAttr()
		if _, ok := x.Get(req.Name); !ok {
			return xattrErrnoNotFound
		}

		// N.B. Do not update the modtime for extended attribute changes.  POSIX
		// defines mtime as for a change of file contents.
		x.Remove(req.Name)
		return nil
	})
}

// Rename implements fs.NodeRenamer.
func (n Node) Rename(ctx context.Context, req *fuse.RenameRequest, dir fs.Node) error {
	inv := n.fs.newInvalSet()
	defer inv.process()
	return n.writeLock(func() error {
		// N.B. Order matters here, since n and dir may be the same node.

		src, err := n.file.Open(ctx, req.OldName)
		if errors.Is(err, file.ErrChildNotFound) {
			return fuse.ENOENT
		} else if err != nil {
			return err
		}

		dir := dir.(Node)
		if tgt, err := dir.file.Open(ctx, req.NewName); err == nil {
			if tgt.Stat().Mode.IsDir() {
				return fuse.EEXIST // can't replace an existing directory

				// The rename(2) documentation implies src can replace tgt if they
				// are both directories, but in practice most filesystems appear
				// reject an attempt to replace a directory with anything, even if
				// they are both empty. So I have adopted the same semantics here.

			} else if src.Stat().Mode.IsDir() {
				return fuse.EEXIST // can't replace a file with a directory
			}

			// Remove the existing file from the target location.
			defer dir.touchIfOK(nil)
			dir.file.Child().Remove(req.NewName)
		} else if !errors.Is(err, file.ErrChildNotFound) {
			return err
		}

		defer n.touchIfOK(nil)
		n.file.Child().Remove(req.OldName) // remove from the old directory
		inv.entry(n, req.OldName)
		inv.attr(n)
		dir.file.Child().Set(req.NewName, src) // add to the new directory
		inv.entry(dir, req.NewName)
		inv.attr(dir)
		return nil
	})
}

// Setattr implements fs.NodeSetattrer.
func (n Node) Setattr(ctx context.Context, req *fuse.SetattrRequest, rsp *fuse.SetattrResponse) error {
	return n.writeLock(func() error {
		// Update the fields of the stat marked as valid in the request.
		//
		// Setting stat cannot fail unless it changes the size of the file, so we
		// will check that first.
		if req.Valid.Size() {
			if err := n.file.Truncate(ctx, int64(req.Size)); err != nil {
				return err
			}
		}
		n.file.Stat().Edit(func(s *file.Stat) {
			if req.Valid.Gid() {
				s.GroupID = int(req.Gid)
			}
			if req.Valid.Mode() {
				s.Mode = req.Mode
			}
			if req.Valid.Mtime() {
				s.ModTime = req.Mtime
			}
			if req.Valid.MtimeNow() {
				s.ModTime = time.Now()
			}
			if req.Valid.Uid() {
				s.OwnerID = int(req.Uid)
			}
		}).Update()
		n.fillAttr(&rsp.Attr)
		return nil
	})
}

// Setxattr implements fs.NodeSetxattrer.
func (n Node) Setxattr(ctx context.Context, req *fuse.SetxattrRequest) error {
	if req.Name == ffsStorageKey || req.Name == ffsStorageKeyHex {
		return fuse.EPERM
	} else if req.Position != 0 {
		return fuse.EPERM // macOS resource forks; don't store that crap
	}
	return n.writeLock(func() error {
		x := n.file.XAttr()
		if _, ok := x.Get(req.Name); ok {
			if req.Flags&xattrCreate != 0 {
				return fuse.EEXIST // create, but already exists
			}
		} else if req.Flags&xattrReplace != 0 {
			return xattrErrnoNotFound // replace, but does not exist
		}

		// N.B. Do not update the modtime for extended attribute changes.  POSIX
		// defines mtime as for a change of file contents.
		x.Set(req.Name, string(req.Xattr))
		return nil
	})
}

// Symlink implements fs.NodeSymlinker.
func (n Node) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (node fs.Node, err error) {
	err = n.writeLock(func() error {
		if n.file.Child().Has(req.NewName) {
			return fuse.EEXIST
		}
		f := n.file.New(&file.NewOptions{
			Name: req.NewName,
			Stat: &file.Stat{
				Mode:    os.ModeSymlink | 0555,
				OwnerID: int(req.Uid),
				GroupID: int(req.Gid),
			},
		})
		if _, err := f.WriteAt(ctx, []byte(req.Target), 0); err != nil {
			return err
		}
		defer n.touchIfOK(nil)
		n.file.Child().Set(req.NewName, f)

		fnode := Node{fs: n.fs, file: f}
		node = fnode
		return nil
	})
	return
}

// writeLock executes fn while holding a write lock on n.
func (n Node) writeLock(fn func() error) error {
	n.fs.μ.Lock()
	defer n.fs.μ.Unlock()
	return fn()
}

// readLock executes fn while holding a read lock on n.
func (n Node) readLock(fn func() error) error {
	n.fs.μ.RLock()
	defer n.fs.μ.RUnlock()
	return fn()
}

// A Handle represents an open file pointer.
type Handle struct {
	Node
	writable bool
	append   bool
}

// Read implements fs.HandleReader.
func (h Handle) Read(ctx context.Context, req *fuse.ReadRequest, rsp *fuse.ReadResponse) error {
	return h.readLock(func() error {
		buf := make([]byte, req.Size)
		nr, err := h.file.ReadAt(ctx, buf, req.Offset)
		if err == io.EOF {
			// read(2) signals EOF by returning 0 bytes; but io.ReaderAt requires
			// that any short read report an error. We don't want to propagate
			// that error back to FUSE, however, because it will turn into EIO.
			err = nil
		}
		rsp.Data = buf[:nr]
		return err
	})
}

// Write implements fs.HandleWriter.
func (h Handle) Write(ctx context.Context, req *fuse.WriteRequest, rsp *fuse.WriteResponse) error {
	return h.writeLock(func() error {
		if !h.writable {
			return fuse.EPERM
		}
		offset := req.Offset
		if h.append {
			offset = h.file.Size() // ignore the requested offset for append-only files
		}
		nw, err := h.file.WriteAt(ctx, req.Data, offset)
		defer h.touchIfOK(err)
		rsp.Size = nw
		return err
	})
}

// Flush implements fs.HandleFlusher.
func (h Handle) Flush(ctx context.Context, req *fuse.FlushRequest) error { return h.flush(ctx) }

// Release implements fs.HandleReleaser.
func (h Handle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	if h.writable || h.append {
		inv := h.fs.newInvalSet()
		defer inv.process()
		inv.attr(h.Node)
	}
	return h.flush(ctx)
}

// ReadDirAll implements fs.HandleReadDirAller.
func (h Handle) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	// N.B. This requires a write lock because paging in children updates caches.
	var elts []fuse.Dirent
	err := h.writeLock(func() error {
		for _, name := range h.file.Child().Names() {
			kid, err := h.file.Open(ctx, name)
			if err != nil {
				return err
			}
			var ktype fuse.DirentType
			switch m := kid.Stat().Mode; {
			case m.IsDir():
				ktype = fuse.DT_Dir
			case m.IsRegular():
				ktype = fuse.DT_File
			case m&os.ModeSymlink != 0:
				ktype = fuse.DT_Link
			}
			elts = append(elts, fuse.Dirent{
				Inode: fileInode(kid),
				Name:  name,
				Type:  ktype,
			})
		}
		return nil
	})
	return elts, err
}

func (h Handle) flush(ctx context.Context) error {
	return h.writeLock(func() error {
		// Because the filesystem is a Merkle tree, flushing any node flushes all
		// its children recursively.
		_, err := h.file.Flush(ctx)
		return err
	})
}

// fileInode synthesizes an inode number for a file from its address.
// This is safe because a location cannot become another file until after a
// successful GC, which means the old one is no longer referenced.
func fileInode(f *file.File) uint64 { return uint64(reflect.ValueOf(f).Pointer()) }

// An inval represnts an attribute or entry invalidation request we need to
// make to FUSE.
type inval struct {
	fs.Node
	entryName string
}

// newInvalSet creates a new empty invalidation set. A caller that needs to
// invalidate nodes should populate the set and defer a call to the process
// method before returning.
//
// Invalidations cannot safely be processed during the main request flow, as
// FUSE may upcall into the driver, which will be blocked by the unresolved
// service routine that wants to do the invalidations. The invalSet handles
// this by processing the invalidations in a separate goroutine.
func (fs *FS) newInvalSet() *invalSet { return &invalSet{server: fs.server} }

type invalSet struct {
	server  *fs.Server
	entries []inval
}

// entry adds an invalidation for a directory entry.
func (v *invalSet) entry(n fs.Node, entry string) {
	v.entries = append(v.entries, inval{n, entry})
}

// attr adds an invalidation for node stat.
func (v *invalSet) attr(n fs.Node) {
	v.entries = append(v.entries, inval{Node: n})
}

func (v *invalSet) process() {
	if len(v.entries) == 0 {
		return // nothing to do
	}
	go func() {
		for _, e := range v.entries {
			if e.entryName != "" {
				v.server.InvalidateEntry(e.Node, e.entryName)
			} else {
				v.server.InvalidateNodeAttr(e.Node)
			}
		}
	}()
}
