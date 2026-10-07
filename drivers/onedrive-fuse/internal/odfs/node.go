package odfs

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// node is the go-fuse inode wrapper. go-fuse deduplicates inodes by StableAttr
// (Ino), so several node values may share one Entry; all state lives there.
type node struct {
	fs.Inode
	fsys *FS
	e    *Entry
}

var (
	_ fs.NodeLookuper  = (*node)(nil)
	_ fs.NodeReaddirer = (*node)(nil)
	_ fs.NodeGetattrer = (*node)(nil)
	_ fs.NodeSetattrer = (*node)(nil)
	_ fs.NodeOpener    = (*node)(nil)
	_ fs.NodeCreater   = (*node)(nil)
	_ fs.NodeMkdirer   = (*node)(nil)
	_ fs.NodeUnlinker  = (*node)(nil)
	_ fs.NodeRmdirer   = (*node)(nil)
	_ fs.NodeRenamer   = (*node)(nil)
	_ fs.NodeStatfser  = (*node)(nil)
)

// Root returns the go-fuse root node.
func (f *FS) Root() fs.InodeEmbedder { return &node{fsys: f, e: f.root} }

func stable(e *Entry) fs.StableAttr {
	if e.IsDir {
		return fs.StableAttr{Mode: fuse.S_IFDIR, Ino: e.Ino}
	}
	return fs.StableAttr{Mode: fuse.S_IFREG, Ino: e.Ino}
}

func (f *FS) errno(op string, e *Entry, err error) syscall.Errno {
	if errors.Is(err, errSyncPaused) {
		return syscall.ENETDOWN
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	f.log.Error(op, "path", f.path(e), "err", err)
	return syscall.EIO
}

func (n *node) child(ctx context.Context, c *Entry, out *fuse.EntryOut) *fs.Inode {
	n.fsys.attr(c, &out.Attr)
	return n.NewInode(ctx, &node{fsys: n.fsys, e: c}, stable(c))
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	f := n.fsys
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ensureListed(ctx, n.e); err != nil {
		return nil, f.errno("lookup", n.e, err)
	}
	c := n.e.Children[name]
	if c == nil {
		return nil, syscall.ENOENT
	}
	return n.child(ctx, c, out), 0
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	f := n.fsys
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ensureListed(ctx, n.e); err != nil {
		return nil, f.errno("readdir", n.e, err)
	}
	out := make([]fuse.DirEntry, 0, len(n.e.Children))
	for name, c := range n.e.Children {
		st := stable(c)
		out = append(out, fuse.DirEntry{Name: name, Mode: st.Mode, Ino: st.Ino})
	}
	return fs.NewListDirStream(out), 0
}

func (n *node) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.fsys.mu.Lock()
	defer n.fsys.mu.Unlock()
	n.fsys.attr(n.e, &out.Attr)
	return 0
}

func (n *node) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	f := n.fsys
	f.mu.Lock()
	defer f.mu.Unlock()
	e := n.e
	if sz, ok := in.GetSize(); ok && !e.IsDir {
		if h, ok := fh.(*handle); ok && h.file != nil {
			if err := h.file.Truncate(int64(sz)); err != nil {
				return fs.ToErrno(err)
			}
		} else {
			if sz == 0 && !e.Cached {
				if err := os.WriteFile(f.cachePath(e), nil, 0o600); err != nil {
					return fs.ToErrno(err)
				}
				e.Cached = true
			} else if err := f.ensureCached(ctx, e); err != nil {
				return f.errno("truncate", e, err)
			}
			if err := os.Truncate(f.cachePath(e), int64(sz)); err != nil {
				return fs.ToErrno(err)
			}
		}
		e.Size = int64(sz)
		e.Mtime = time.Now()
		e.Dirty = true
		e.DirtyGen++
		f.markPending(e)
		if e.openW == 0 {
			f.trigger(nil)
		}
	}
	if mt, ok := in.GetMTime(); ok {
		e.Mtime = mt
		f.stateDirty = true
	}
	f.attr(e, &out.Attr)
	return 0
}

// ensureCached downloads e's content into the cache if needed. Caller holds
// mu; it is released during the download.
func (f *FS) ensureCached(ctx context.Context, e *Entry) error {
	for !e.Cached {
		if e.dl != nil {
			ch := e.dl
			f.mu.Unlock()
			<-ch
			f.mu.Lock()
			continue
		}
		if e.detached {
			return syscall.ENOENT
		}
		if !f.syncEnabled {
			return errSyncPaused
		}
		ch := make(chan struct{})
		e.dl = ch
		id, ctag, dst := e.ID, e.CTag, f.cachePath(e)
		f.mu.Unlock()
		err := f.download(ctx, id, dst)
		f.mu.Lock()
		e.dl = nil
		close(ch)
		if err != nil {
			return err
		}
		if e.CTag == ctag && e.ID == id {
			e.Cached = true
			f.stateDirty = true
		}
	}
	return nil
}

func (f *FS) download(ctx context.Context, id, dst string) error {
	tmp, err := os.CreateTemp(f.opts.CacheDir, ".dl-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := f.remote.Download(ctx, id, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	f := n.fsys
	f.mu.Lock()
	defer f.mu.Unlock()
	e := n.e
	if e.IsDir {
		return nil, 0, syscall.EISDIR
	}
	writable := flags&syscall.O_ACCMODE != syscall.O_RDONLY
	trunc := writable && flags&syscall.O_TRUNC != 0
	for attempt := 0; ; attempt++ {
		if trunc && !e.Cached {
			if err := os.WriteFile(f.cachePath(e), nil, 0o600); err != nil {
				return nil, 0, fs.ToErrno(err)
			}
			e.Cached = true
		}
		if err := f.ensureCached(ctx, e); err != nil {
			return nil, 0, f.errno("open", e, err)
		}
		file, err := os.OpenFile(f.cachePath(e), int(flags)&^(syscall.O_APPEND|syscall.O_CREAT|syscall.O_EXCL), 0o600)
		if errors.Is(err, os.ErrNotExist) && attempt == 0 {
			e.Cached = false // cache file was removed behind our back
			continue
		}
		if err != nil {
			return nil, 0, fs.ToErrno(err)
		}
		now := time.Now()
		_ = os.Chtimes(f.cachePath(e), now, now) // LRU timestamp
		return f.newHandle(e, file, writable, trunc), 0, 0
	}
}

// newHandle registers an open file. Caller holds mu.
func (f *FS) newHandle(e *Entry, file *os.File, writable, trunc bool) *handle {
	e.open++
	if writable {
		e.openW++
	}
	if trunc {
		e.Size = 0
		e.Dirty = true
		e.DirtyGen++
		e.Mtime = time.Now()
		f.markPending(e)
	}
	return &handle{fsys: f, e: e, file: file, writable: writable}
}

func (n *node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	f := n.fsys
	if !validName(name) {
		return nil, nil, 0, syscall.EINVAL
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ensureListed(ctx, n.e); err != nil {
		return nil, nil, 0, f.errno("create", n.e, err)
	}
	if n.e.Children[name] != nil {
		return nil, nil, 0, syscall.EEXIST
	}
	e := f.newEntry(n.e, name, false)
	e.CacheKey = localCacheKey()
	file, err := os.OpenFile(f.cachePath(e), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		f.forget(e)
		return nil, nil, 0, fs.ToErrno(err)
	}
	e.Cached, e.Dirty = true, true
	f.markPending(e)
	h := f.newHandle(e, file, true, false)
	return n.child(ctx, e, out), h, 0, 0
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	f := n.fsys
	if !validName(name) {
		return nil, syscall.EINVAL
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ensureListed(ctx, n.e); err != nil {
		return nil, f.errno("mkdir", n.e, err)
	}
	if n.e.Children[name] != nil {
		return nil, syscall.EEXIST
	}
	e := f.newEntry(n.e, name, true)
	e.CacheKey = localCacheKey()
	e.Children = map[string]*Entry{}
	f.markPending(e)
	f.trigger(nil)
	return n.child(ctx, e, out), 0
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	f := n.fsys
	f.mu.Lock()
	defer f.mu.Unlock()
	c := n.e.Children[name]
	if c == nil {
		return syscall.ENOENT
	}
	if c.IsDir {
		return syscall.EISDIR
	}
	f.remove(c)
	f.trigger(nil)
	return 0
}

// requireEmpty fails unless dir is known to have no children.
func (f *FS) requireEmpty(ctx context.Context, dir *Entry) error {
	if err := f.ensureListed(ctx, dir); err != nil {
		return err
	}
	if len(dir.Children) > 0 {
		return syscall.ENOTEMPTY
	}
	return nil
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	f := n.fsys
	f.mu.Lock()
	defer f.mu.Unlock()
	c := n.e.Children[name]
	if c == nil {
		return syscall.ENOENT
	}
	if !c.IsDir {
		return syscall.ENOTDIR
	}
	if err := f.requireEmpty(ctx, c); err != nil {
		return f.errno("rmdir", c, err)
	}
	if n.e.Children[name] != c {
		return syscall.ENOENT
	}
	f.remove(c)
	f.trigger(nil)
	return 0
}

func (n *node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	f := n.fsys
	if flags&fs.RENAME_EXCHANGE != 0 {
		return syscall.ENOTSUP
	}
	if !validName(newName) {
		return syscall.EINVAL
	}
	np, ok := newParent.(*node)
	if !ok {
		return syscall.EXDEV
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	src := n.e.Children[name]
	if src == nil {
		return syscall.ENOENT
	}
	for p := np.e; p != nil; p = p.Parent {
		if p == src {
			return syscall.EINVAL
		}
	}
	if err := f.ensureListed(ctx, np.e); err != nil {
		return f.errno("rename", np.e, err)
	}
	if n.e.Children[name] != src {
		return syscall.ENOENT
	}
	if dst := np.e.Children[newName]; dst != nil && dst != src {
		const renameNoReplace = 0x1
		if flags&renameNoReplace != 0 {
			return syscall.EEXIST
		}
		if dst.IsDir != src.IsDir {
			if dst.IsDir {
				return syscall.EISDIR
			}
			return syscall.ENOTDIR
		}
		if dst.IsDir {
			if err := f.requireEmpty(ctx, dst); err != nil {
				return f.errno("rename", dst, err)
			}
		}
		f.remove(dst)
	}
	f.detach(src)
	src.Name, src.Parent = newName, np.e
	np.e.Children[newName] = src
	if src.ID != "" {
		src.Moved = true
	}
	f.markPending(src)
	f.trigger(nil)
	return 0
}

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	f := n.fsys
	f.mu.Lock()
	q := f.quota
	f.mu.Unlock()
	const bs = 4096
	out.Bsize, out.Frsize, out.NameLen = bs, bs, 255
	if q == nil || q.Total == 0 {
		out.Blocks, out.Bfree, out.Bavail = 1<<40/bs, 1<<40/bs, 1<<40/bs
		return 0
	}
	out.Blocks = uint64(q.Total) / bs
	out.Bfree = uint64(max(q.Remaining, 0)) / bs
	out.Bavail = out.Bfree
	return 0
}

// handle is an open file backed by the local cache copy.
type handle struct {
	fsys     *FS
	e        *Entry
	file     *os.File
	writable bool
}

var (
	_ fs.FileReader   = (*handle)(nil)
	_ fs.FileWriter   = (*handle)(nil)
	_ fs.FileFsyncer  = (*handle)(nil)
	_ fs.FileReleaser = (*handle)(nil)
	_ fs.FileFlusher  = (*handle)(nil)
)

func (h *handle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := h.file.ReadAt(dest, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fs.ToErrno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *handle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	n, err := h.file.WriteAt(data, off)
	f := h.fsys
	f.mu.Lock()
	defer f.mu.Unlock()
	if n > 0 {
		e := h.e
		e.Size = max(e.Size, off+int64(n))
		e.Dirty = true
		e.DirtyGen++
		e.Mtime = time.Now()
		f.markPending(e)
	}
	if err != nil {
		return uint32(n), fs.ToErrno(err)
	}
	return uint32(n), 0
}

func (h *handle) Flush(ctx context.Context) syscall.Errno { return 0 }

func (h *handle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	return fs.ToErrno(h.file.Sync())
}

func (h *handle) Release(ctx context.Context) syscall.Errno {
	err := h.file.Close()
	f := h.fsys
	f.mu.Lock()
	defer f.mu.Unlock()
	e := h.e
	e.open--
	if h.writable {
		e.openW--
	}
	if e.detached && e.open == 0 {
		_ = os.Remove(f.cachePath(e))
	}
	if e.openW == 0 && e.needsSync() {
		f.trigger(nil)
	}
	return fs.ToErrno(err)
}
