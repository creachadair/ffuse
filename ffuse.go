package ffuse

import (
	"context"
	"reflect"
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"bitbucket.org/creachadair/ffs/file"
	"golang.org/x/xerrors"
)

func New(root *file.File) *FS {
	return &FS{root: root}
}

type FS struct {
	// All operations on any node of the filesystem must hold μ.
	// Operations that modify the contents of the tree must hold a write lock.

	μ    sync.RWMutex
	root *file.File
}

// Root implements the fs.FS interface.
func (fs *FS) Root() (fs.Node, error) { return Node{fs: fs, file: fs.root}, nil }

type Node struct {
	fs   *FS
	file *file.File
}

// Verify interface satisfactions.
var (
	_ fs.FS                  = (*FS)(nil)
	_ fs.Node                = Node{}
	_ fs.NodeCreater         = Node{}
	_ fs.NodeMkdirer         = Node{}
	_ fs.NodeOpener          = Node{}
	_ fs.NodeRemover         = Node{}
	_ fs.NodeRenamer         = Node{}
	_ fs.NodeRequestLookuper = Node{}
	_ fs.NodeSetattrer       = Node{}
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

func (n Node) fillAttr(attr *fuse.Attr) {
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
		attr.Nlink = uint32(2 + len(n.file.Children()))
	}
}

func (n Node) touchIfOK(err error) {
	if err == nil {
		n.file.SetStat(func(s *file.Stat) { s.ModTime = time.Now() })
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
		} else if !xerrors.Is(err, file.ErrChildNotFound) {
			return err
		} else {
			// The file doesn't exist; create a new empty file or directory.
			f = n.file.New(&file.NewOptions{
				Name: req.Name,
				Stat: file.Stat{
					Mode:    req.Mode,
					OwnerID: int(req.Uid),
					GroupID: int(req.Gid),
				},
			})
			n.file.Set(req.Name, f)
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

// Lookup implements fs.NodeRequestLookuper.
func (n Node) Lookup(ctx context.Context, req *fuse.LookupRequest, rsp *fuse.LookupResponse) (node fs.Node, err error) {
	err = n.writeLock(func() error {
		f, err := n.file.Open(ctx, req.Name)
		if xerrors.Is(err, file.ErrChildNotFound) {
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
		if n.file.HasChild(req.Name) {
			return fuse.EEXIST
		}
		defer n.touchIfOK(nil)
		f := n.file.New(&file.NewOptions{
			Name: req.Name,
			Stat: file.Stat{
				Mode:    req.Mode,
				ModTime: time.Now(),
				OwnerID: int(req.Uid),
				GroupID: int(req.Gid),
			},
		})
		n.file.Set(req.Name, f)
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

// Remove implements fs.NodeRemover.
func (n Node) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	return n.writeLock(func() error {
		f, err := n.file.Open(ctx, req.Name)
		if xerrors.Is(err, file.ErrChildNotFound) {
			return fuse.ENOENT
		} else if err != nil {
			return err
		}

		if f.Stat().Mode.IsDir() {
			if !req.Dir {
				return fuse.EPERM // unlink(directory)
			} else if len(f.Children()) != 0 {
				return fuse.Errno(syscall.ENOTEMPTY)
			}
		} else if req.Dir {
			return fuse.EPERM // rmdir(non-directory)
		}
		n.file.Remove(req.Name)
		return nil
	})
}

// Rename implements fs.NodeRenamer.
func (n Node) Rename(ctx context.Context, req *fuse.RenameRequest, dir fs.Node) error {
	return n.writeLock(func() error {
		// N.B. Order matters here, since n and dir may be the same node.

		src, err := n.file.Open(ctx, req.OldName)
		if xerrors.Is(err, file.ErrChildNotFound) {
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
			dir.file.Remove(req.NewName)
		} else if !xerrors.Is(err, file.ErrChildNotFound) {
			return err
		}

		defer n.touchIfOK(nil)
		n.file.Remove(req.OldName)     // remove from the old directory
		dir.file.Set(req.NewName, src) // add to the new directory
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
		n.file.SetStat(func(s *file.Stat) {
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
		})
		n.fillAttr(&rsp.Attr)
		return nil
	})
}

// writeLock executes fn while holding a write lock on n.
func (n Node) writeLock(fn func() error) error {
	n.fs.μ.Lock()
	defer n.fs.μ.Unlock()
	return fn()
}

// readLock executs fn while holding a read lock on n.
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
		nr, err := h.file.ReadAt(ctx, rsp.Data, req.Offset)
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
func (h Handle) Release(ctx context.Context, req *fuse.ReleaseRequest) error { return h.flush(ctx) }

// ReadDirAll implements fs.HandleReadDirAller.
func (h Handle) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	// N.B. This requires a write lock because paging in children updates caches.
	var elts []fuse.Dirent
	err := h.writeLock(func() error {
		for _, name := range h.file.Children() {
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
