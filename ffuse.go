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
// filesystem package (github.com/creachadair/ffs). It is compatible with the
// github.com/hanwen/go-fuse packages for FUSE integration.
package ffuse

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/creachadair/ffs/file"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/crypto/sha3"
)

type errno = syscall.Errno

// NewFS constructs a new FS with the given root file.
func NewFS(root *file.File) *FS { return &FS{file: root} }

type FS struct {
	// The fs.Inode is self-synchronizing, and is accessed via its
	// impelmentation of the fs.InodeEmbedder interface. Its key requirement is
	// that we must not copy the value.
	fs.Inode

	file *file.File
}

// Verify that the FS supports interfaces required by the FUSE integration.
var (
	_ fs.InodeEmbedder = (*FS)(nil)

	_ fs.NodeAccesser      = (*FS)(nil)
	_ fs.NodeCreater       = (*FS)(nil)
	_ fs.NodeFsyncer       = (*FS)(nil)
	_ fs.NodeGetattrer     = (*FS)(nil)
	_ fs.NodeGetxattrer    = (*FS)(nil)
	_ fs.NodeLinker        = (*FS)(nil)
	_ fs.NodeListxattrer   = (*FS)(nil)
	_ fs.NodeLookuper      = (*FS)(nil)
	_ fs.NodeMkdirer       = (*FS)(nil)
	_ fs.NodeOpener        = (*FS)(nil)
	_ fs.NodeReaddirer     = (*FS)(nil)
	_ fs.NodeReadlinker    = (*FS)(nil)
	_ fs.NodeRemovexattrer = (*FS)(nil)
	_ fs.NodeRenamer       = (*FS)(nil)
	_ fs.NodeRmdirer       = (*FS)(nil)
	_ fs.NodeSetattrer     = (*FS)(nil)
	_ fs.NodeSetxattrer    = (*FS)(nil)
	_ fs.NodeSymlinker     = (*FS)(nil)
	_ fs.NodeUnlinker      = (*FS)(nil)
)

// Access implements the fs.NodeAccesser interface.
func (f *FS) Access(ctx context.Context, mask uint32) errno {
	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return syscall.ENOSYS
	}
	s := f.file.Stat()
	bits := uint32(s.Mode.Perm())

	// Root is not special inside the FUSE mount, so treat the caller as
	// equivalent to owner of paths with ID 0.
	if s.OwnerID == 0 || s.OwnerID == int(caller.Uid) {
		bits >>= 6 // use owner bits
	} else if s.GroupID == int(caller.Gid) {
		bits >>= 3 // use group bits
	} // default to world bits

	// At this point bits has the relevant permissions in lsb.
	// Mask off anything above that and compare.
	bits &= 7
	if mask&bits != mask {
		return syscall.EACCES
	}
	return 0
}

// Create implements the fs.NodeCreater interface.
func (f *FS) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (in *fs.Inode, fh fs.FileHandle, _ uint32, _ errno) {
	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return nil, nil, 0, syscall.ENOSYS
	}

	nf, err := f.file.Open(ctx, name)
	if err == nil {
		// The file already exists; if O_EXCL is set the request fails.
		if flags&syscall.O_EXCL != 0 {
			return nil, nil, 0, syscall.EEXIST
		}
	} else if !errors.Is(err, file.ErrChildNotFound) {
		return nil, nil, 0, errorToErrno(err)
	} else {
		// The file does not exist; create a new empty file.
		// Note that directories go through Mkdir instead.
		nf = f.file.New(&file.NewOptions{
			Name: name,
			Stat: &file.Stat{
				Mode:    fromSysMode(mode, true),
				ModTime: time.Now(),
				OwnerID: int(caller.Uid),
				GroupID: int(caller.Gid),
			},
		})
		f.file.Child().Set(name, nf)
	}

	// IF the request wants the file truncated, do that now.
	if flags&syscall.O_TRUNC != 0 {
		if err := nf.Truncate(ctx, 0); err != nil {
			return nil, nil, 0, errorToErrno(err)
		}
	}

	nfs := &FS{file: nf}
	nfs.fillAttr(&out.Attr)
	in = f.NewInode(ctx, nfs, fileStableAttr(nf))
	fh = &fileHandle{fs: nfs, writable: !isReadOnly(flags), append: flags&syscall.O_APPEND != 0}
	return
}

func (f *FS) fillAttr(out *fuse.Attr) {
	s := f.file.Stat()
	var nb int64
	var nlink uint32 = 1
	if s.Mode.IsDir() {
		nlink = uint32(2 + f.file.Child().Len())
		for _, kid := range f.file.Child().Names() {
			nb += int64(32 + len(kid))
			// +32 for the storage key. This is just an estimate; the point here
			// is to have some stable number that approximates how much storage
			// the directory occupies.
		}
	} else {
		nb = f.file.Data().Size()
	}

	mtns := s.ModTime.UnixNano()

	out.Size = uint64(nb)
	out.Blocks = uint64((nb + 511) / 512)
	out.Mode = toSysMode(s.Mode)
	out.Mtime = uint64(mtns / 1e9)     // seconds
	out.Mtimensec = uint32(mtns % 1e9) // nanoseconds within second
	out.Owner.Uid = uint32(s.OwnerID)
	out.Owner.Gid = uint32(s.GroupID)
	out.Nlink = nlink
}

// Fsync implements the fs.NodeFsyncer interface.
func (f *FS) Fsync(ctx context.Context, fh fs.FileHandle, flags uint32) errno {
	_, err := f.file.Flush(ctx)
	return errorToErrno(err)
}

// Getattr implements the fs.NodeGetattrer interface.
func (f *FS) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) errno {
	if fh != nil {
		if ga, ok := fh.(fs.FileGetattrer); ok {
			return ga.Getattr(ctx, out)
		}
	}
	f.fillAttr(&out.Attr)
	return 0
}

const (
	ffsStorageKey    = "ffs.storageKey"
	ffsStorageKeyB64 = ffsStorageKey + ".b64"
	ffsStorageKeyHex = ffsStorageKey + ".hex"
	ffsDataHash      = "ffs.dataHash"
	ffsDataHashB64   = ffsDataHash + ".b64"
	ffsDataHashHex   = ffsDataHash + ".hex"
	ffsLinkTo        = "ffs.link."
)

// xattrEncoding returns an encoding function for the specified xattr name.
// This should only be used for the "ffs.*" attributes.
func xattrEncoding(name string) func([]byte) string {
	switch path.Ext(name) {
	case ".b64":
		return base64.StdEncoding.EncodeToString
	case ".hex":
		return hex.EncodeToString
	default:
		return nil
	}
}

// Getxattr implements the fs.NodeGetxattrer interface.
func (f *FS) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, errno) {
	buf := dest[:0]
	var encode func([]byte) string
	switch attr {
	case ffsStorageKey, ffsStorageKeyB64, ffsStorageKeyHex:
		encode = xattrEncoding(attr)
		key, err := f.file.Flush(ctx)
		if err != nil {
			return 0, errorToErrno(err)
		}
		buf = append(buf, key...)
	case ffsDataHash, ffsDataHashB64, ffsDataHashHex:
		encode = xattrEncoding(attr)
		h := sha3.New256()
		for _, key := range f.file.Data().Keys() {
			io.WriteString(h, key)
		}
		buf = h.Sum(buf)
	default:
		xa := f.file.XAttr()
		if !xa.Has(attr) {
			return 0, xattrErrnoNotFound
		}
		buf = append(buf, xa.Get(attr)...)
	}
	if encode != nil {
		buf = append(buf[:0], encode(buf)...)
	}
	if len(buf) > len(dest) {
		return uint32(len(buf)), syscall.ERANGE
	}
	return uint32(len(buf)), 0
}

// Link implements the fs.NodeLinker interface.
func (f *FS) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, errno) {
	if f.file.Child().Has(name) {
		return nil, syscall.EEXIST // disallow linking over an existing name
	}
	tf, ok := target.EmbeddedInode().Operations().(*FS)
	if !ok {
		return nil, syscall.EIO // not expected to happen
	}
	if tf.file.Stat().Mode.IsDir() {
		return nil, syscall.EPERM // disallow hard-linking a directory
	}
	f.file.Child().Set(name, tf.file)
	nfs := &FS{file: tf.file}
	nfs.fillAttr(&out.Attr)
	return f.NewInode(ctx, nfs, fileStableAttr(nfs.file)), 0
}

// Listxattr implements the fs.NodeListxattrer interface.
func (f *FS) Listxattr(ctx context.Context, dest []byte) (uint32, errno) {
	buf := dest[:0]
	for _, name := range f.file.XAttr().Names() {
		buf = append(buf, name...)
		buf = append(buf, 0) // NUL terminator
	}
	if len(buf) > len(dest) {
		// Insufficient capacity: Report the desired size and an error.
		return uint32(len(buf)), syscall.ERANGE
	}
	return uint32(len(buf)), 0
}

// Lookup implements the fs.NodeLookuper interface.
func (f *FS) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, errno) {
	nf, err := f.file.Open(ctx, name)
	if errors.Is(err, file.ErrChildNotFound) {
		return nil, syscall.ENOENT
	} else if err != nil {
		return nil, errorToErrno(err)
	}
	nfs := &FS{file: nf}
	nfs.fillAttr(&out.Attr)
	return f.NewInode(ctx, nfs, fileStableAttr(nf)), 0
}

// Mkdir implements the fs.NodeMkdirer interface.
func (f *FS) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, errno) {
	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return nil, syscall.ENOSYS
	}
	if f.file.Child().Has(name) {
		return nil, syscall.EEXIST
	}
	nf := f.file.New(&file.NewOptions{
		Name: name,
		Stat: &file.Stat{
			// N.B.: macOS FUSE populates S_IFMT, but Linux FUSE does not, so
			// explicitly set the directory bit.
			Mode:    fromSysMode(mode, true) | os.ModeDir,
			ModTime: time.Now(),
			OwnerID: int(caller.Uid),
			GroupID: int(caller.Gid),
		},
	})
	f.file.Child().Set(name, nf)
	nfs := &FS{file: nf}
	nfs.fillAttr(&out.Attr)
	return f.NewInode(ctx, nfs, fileStableAttr(nf)), 0
}

// Open implements the fs.NodeOpener interface.
func (f *FS) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, errno) {
	return &fileHandle{fs: f, writable: !isReadOnly(flags), append: flags&syscall.O_APPEND != 0}, 0, 0
}

// Readdir implements the fs.NodeReaddirer interface.
func (f *FS) Readdir(ctx context.Context) (fs.DirStream, errno) {
	kids := f.file.Child()
	elts := make([]fuse.DirEntry, kids.Len())
	for i, name := range kids.Names() { // already sorted
		kid, err := f.file.Open(ctx, name)
		if err != nil {
			return nil, errorToErrno(err)
		}
		elts[i] = fuse.DirEntry{
			Mode: toSysMode(kid.Stat().Mode),
			Name: name,
		}
	}
	return fs.NewListDirStream(elts), 0
}

// Readlink implements the fs.NodeReadlinker interface.
func (f *FS) Readlink(ctx context.Context) ([]byte, errno) {
	buf := make([]byte, int(f.file.Data().Size()))
	if _, err := f.file.ReadAt(ctx, buf, 0); err != nil {
		return nil, errorToErrno(err)
	}
	return buf, 0
}

// Removexattr implements the fs.NodeRemovexattrer interface.
func (f *FS) Removexattr(ctx context.Context, attr string) errno {
	if strings.HasPrefix(attr, ffsStorageKey) || strings.HasPrefix(attr, ffsDataHash) {
		return syscall.EPERM // virtual attributes, not writable
	}

	// If f is a directory, then removing ffs.link.<name> causes <name> to be
	// unlinked as a child of f, regardless of its type. This differs from
	// Unlink in that it can immediately unlink a complete directory.
	if t, ok := strings.CutPrefix(attr, ffsLinkTo); ok {
		if !f.file.Stat().Mode.IsDir() {
			return syscall.EPERM
		}
		if !f.file.Child().Remove(t) {
			return xattrErrnoNotFound
		}
		go f.NotifyEntry(t) // outside the lock
		return 0
	}
	xa := f.file.XAttr()
	if !xa.Has(attr) {
		return xattrErrnoNotFound
	}
	xa.Remove(attr)
	return 0
}

// Rename implements the fs.NodeRenameer interface.
func (f *FS) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) errno {
	np, ok := newParent.EmbeddedInode().Operations().(*FS)
	if !ok {
		return syscall.ENOSYS
	}
	cf, err := f.file.Open(ctx, name) // this is the file to be renamed
	if errors.Is(err, file.ErrChildNotFound) {
		return syscall.ENOENT
	} else if err != nil {
		return errorToErrno(err)
	}
	tf, err := np.file.Open(ctx, newName) // this is the target name
	if err == nil {
		if tf.Stat().Mode.IsDir() {
			return syscall.EEXIST // disallow replacement of an existing directory

			// The rename(2) documentation implies src can replace tgt if they are
			// both directories, but in practice most filesystems appear to reject
			// an attempt to replace a directory with anything, even if they are
			// both empty. So I have adopted the same semantics here.
		} else if cf.Stat().Mode.IsDir() {
			return syscall.EEXIST // disallow overwriting a file with a directory
		}
	} else if !errors.Is(err, file.ErrChildNotFound) {
		return errorToErrno(err)
	}

	// Order matters here, since we may be renaming the child within the same
	// directory: Remove the old entry, then add the new entry.
	f.file.Child().Remove(name)
	np.file.Child().Set(newName, cf)
	return 0
}

// Rmdir implements the fs.NodeRmdirer interface.
func (f *FS) Rmdir(ctx context.Context, name string) errno {
	uf, err := f.file.Open(ctx, name)
	if errors.Is(err, file.ErrChildNotFound) {
		return syscall.ENOENT
	} else if err != nil {
		return errorToErrno(err)
	}

	if uf.Child().Len() != 0 {
		return syscall.ENOTEMPTY
	} else if !f.file.Child().Remove(name) {
		return syscall.ENOENT
	}
	return 0
}

// Setattr implements the fs.NodeSetattrer interface.
func (f *FS) Setattr(ctx context.Context, _ fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) errno {
	// Update the fields of the stat marked as valid in the request.
	//
	// Setting stat cannot fail unless it changes the size of the file, so we
	// will check that first.
	if sz, ok := in.GetSize(); ok {
		if err := f.file.Truncate(ctx, int64(sz)); err != nil {
			return errorToErrno(err)
		}
	}

	s := f.file.Stat()
	if id, ok := in.GetGID(); ok {
		s.GroupID = int(id)
	}
	if id, ok := in.GetUID(); ok {
		s.OwnerID = int(id)
	}
	if m, ok := in.GetMode(); ok {
		s.Mode = s.Mode.Type() | fromSysMode(m, false) // omit type
	}
	if mt, ok := in.GetMTime(); ok {
		s.ModTime = mt
	}
	s.Update()
	f.fillAttr(&out.Attr)
	return 0
}

// Setxattr implements the fs.NodeSetxattrer interface.
func (f *FS) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) errno {
	if strings.HasPrefix(attr, ffsStorageKey) || strings.HasPrefix(attr, ffsDataHash) {
		return syscall.EPERM // virtual attributes, not writable
	}

	// If f is a directory, then setting ffs.linkTo.<name> causes <name> to be
	// set or replaced as a child of f, pointing to the file whose storage key
	// is given in the value.
	if t, ok := strings.CutPrefix(attr, ffsLinkTo); ok {
		if !f.file.Stat().Mode.IsDir() {
			return syscall.EPERM // only allow linking in a directory
		} else if t == "" || strings.ContainsAny(t, "/\x00") {
			return syscall.EINVAL // disallow empty names, directory separators, NUL
		}
		exists := f.file.Child().Has(t)
		if exists && flags&xattrCreate != 0 {
			return syscall.EEXIST
		} else if !exists && flags&xattrReplace != 0 {
			return syscall.ENOENT
		}

		tf, err := f.file.Load(ctx, string(data))
		if err != nil {
			return syscall.ENOENT
		}
		f.file.Child().Set(t, tf)
		go f.NotifyEntry(t) // outside the lock
		return 0
	}

	xa := f.file.XAttr()
	exists := xa.Has(attr)
	if exists && flags&xattrCreate != 0 {
		return syscall.EEXIST // create, but it already exists
	} else if !exists && flags&xattrReplace != 0 {
		return xattrErrnoNotFound // replace, but it doesn't exist
	}
	xa.Set(attr, string(data))
	return 0
}

// Symlink implements the fs.NodeSymlinker interface.
func (f *FS) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, errno) {
	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return nil, syscall.ENOSYS
	}
	if f.file.Child().Has(name) {
		return nil, syscall.EEXIST
	}
	nf := f.file.New(&file.NewOptions{
		Name: name,
		Stat: &file.Stat{
			Mode:    os.ModeSymlink | 0555,
			OwnerID: int(caller.Uid),
			GroupID: int(caller.Gid),
		},
	})
	if _, err := nf.WriteAt(ctx, []byte(target), 0); err != nil {
		return nil, errorToErrno(err)
	}
	f.file.Child().Set(name, nf)
	nfs := &FS{file: nf}
	nfs.fillAttr(&out.Attr)
	return f.NewInode(ctx, nfs, fileStableAttr(nf)), 0
}

// Unlink implements the fs.NodeUnlinker interface.
func (f *FS) Unlink(ctx context.Context, name string) errno {
	uf, err := f.file.Open(ctx, name)
	if errors.Is(err, file.ErrChildNotFound) {
		return syscall.ENOENT
	} else if err != nil {
		return errorToErrno(err)
	}

	// POSIX wants us not to allow removal of non-empty directories.
	if uf.Stat().Mode.IsDir() && uf.Child().Len() != 0 {
		return syscall.ENOTEMPTY
	} else if !f.file.Child().Remove(name) {
		return syscall.ENOENT
	}
	return 0
}

// Verify that filehandles support interfaces required by the FUSE integration.
var (
	_ fs.FileGetattrer = &fileHandle{}
	_ fs.FileReader    = &fileHandle{}
	_ fs.FileReleaser  = &fileHandle{}
	_ fs.FileFlusher   = &fileHandle{}
	_ fs.FileWriter    = &fileHandle{}
)

type fileHandle struct {
	fs               *FS
	writable, append bool
}

// Getattr implements the fs.FileGetattrer interface.
func (h fileHandle) Getattr(ctx context.Context, out *fuse.AttrOut) errno {
	h.fs.fillAttr(&out.Attr)
	return 0
}

// Read implements the fs.FileReader interface.
func (h fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, errno) {
	nr, err := h.fs.file.ReadAt(ctx, dest, off)
	if err != nil && err != io.EOF {
		// read(2) signals EOF by returning 0 bytes, but io.ReaderAt requires
		// that any short read report an error. We don't want to propagate that
		// error back to FUSE, however, because that will turn into EIO.
		return nil, errorToErrno(err)
	}
	return fuse.ReadResultData(dest[:nr]), 0
}

// Release implements the fs.FileReleaser interface.
func (h fileHandle) Release(ctx context.Context) errno {
	h.fs.file.Child().Release() // un-pin cached child files
	return errorToErrno(nil)
}

func (h fileHandle) touch() {
	stat := h.fs.file.Stat()
	stat.ModTime = time.Now()
	stat.Update()
}

// Write implements the fs.FileWriter interface.
func (h fileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, errno) {
	if !h.writable {
		return 0, syscall.EPERM
	} else if h.append {
		// If the file is open for appending, ignore the requested offset.
		off = h.fs.file.Data().Size()
	}
	nw, err := h.fs.file.WriteAt(ctx, data, off)
	if nw > 0 {
		h.touch()
	}
	return uint32(nw), errorToErrno(err)
}

// Flush implements the fs.FileFlusher interface.
func (h fileHandle) Flush(ctx context.Context) errno {
	_, err := h.fs.file.Flush(ctx)
	return errorToErrno(err)
}

func errorToErrno(err error) errno {
	if err == nil {
		return fs.OK
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return syscall.EINTR
	}
	return fs.ToErrno(err)
}

func fileStableAttr(file *file.File) fs.StableAttr {
	return fs.StableAttr{Mode: modeFileType(file.Stat().Mode)}
}

func isReadOnly(flags uint32) bool {
	return flags&syscall.O_RDWR == 0 && flags&syscall.O_WRONLY == 0
}

// modeFileType constructs a system-compatible representation of the type of a
// file from the Go-specific mode word. While the Go encoding is stable, it is
// not always consistent with the system layout.
func modeFileType(m os.FileMode) uint32 { return toSysMode(m) & syscall.S_IFMT }

// toSysMode converts a Go file mode to the system-specific layout.
// This presumes a Unix-like syscall package.
func toSysMode(m os.FileMode) uint32 {
	base := uint32(m.Perm())
	switch {
	// Check common cases early.
	case m.IsRegular():
	case m.IsDir():
		base |= syscall.S_IFDIR
	case m&os.ModeSymlink != 0:
		base |= syscall.S_IFLNK
	case m&os.ModeSocket != 0:
		base |= syscall.S_IFSOCK
	case m&os.ModeNamedPipe != 0:
		base |= syscall.S_IFIFO
	case m&os.ModeCharDevice != 0:
		base |= syscall.S_IFCHR
	case m&os.ModeDevice != 0:
		base |= syscall.S_IFBLK // ok, we checked for char devices first
	}
	if m&os.ModeSetuid != 0 {
		base |= syscall.S_ISUID
	}
	if m&os.ModeSetgid != 0 {
		base |= syscall.S_ISGID
	}
	if m&os.ModeSticky != 0 {
		base |= syscall.S_ISVTX
	}
	return base
}

func fromSysMode(m uint32, withType bool) os.FileMode {
	base := os.FileMode(m).Perm()
	if m&syscall.S_ISUID != 0 {
		base |= os.ModeSetuid
	}
	if m&syscall.S_ISGID != 0 {
		base |= os.ModeSetgid
	}
	if m&syscall.S_ISVTX != 0 {
		base |= os.ModeSticky
	}
	if withType {
		switch {
		// Check common cases early.
		case m&syscall.S_IFREG != 0:
			// OK, this is the default.
		case m&syscall.S_IFDIR != 0:
			base |= os.ModeDir
		case m&syscall.S_IFLNK != 0:
			base |= os.ModeSymlink

		case m&syscall.S_IFSOCK != 0:
			base |= os.ModeSocket
		case m&syscall.S_IFIFO != 0:
			base |= os.ModeNamedPipe
		case m&syscall.S_IFCHR != 0:
			base |= os.ModeDevice | os.ModeCharDevice
		case m&syscall.S_IFBLK != 0:
			base |= os.ModeDevice
		default:
			base |= os.ModeIrregular // "something else"
		}
	}
	return base
}
