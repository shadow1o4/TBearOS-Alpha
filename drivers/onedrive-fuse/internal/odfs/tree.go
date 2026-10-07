// Package odfs implements the OneDrive FUSE filesystem: an in-memory metadata
// tree backed by a persistent on-disk cache and a background syncer.
package odfs

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/graph"
)

// Remote is the subset of the Graph client the filesystem needs.
type Remote interface {
	Root(ctx context.Context) (*graph.Item, error)
	Children(ctx context.Context, id string) ([]graph.Item, error)
	Child(ctx context.Context, parentID, name string) (*graph.Item, error)
	Download(ctx context.Context, id string, w io.Writer) error
	Upload(ctx context.Context, parentID, name string, r io.ReaderAt, size int64) (*graph.Item, error)
	Mkdir(ctx context.Context, parentID, name string) (*graph.Item, error)
	Delete(ctx context.Context, id string) error
	Move(ctx context.Context, id, newParentID, newName string) (*graph.Item, error)
	Quota(ctx context.Context) (*graph.Quota, error)
}

// Entry is one file or folder. All fields are guarded by FS.mu.
type Entry struct {
	Ino      uint64
	ID       string // OneDrive item ID; empty until created remotely
	CacheKey string // content file name under cache/content; never changes
	Name     string
	Parent   *Entry
	IsDir    bool
	Size     int64
	Mtime    time.Time
	CTag     string

	Children map[string]*Entry // nil until listed
	ListedAt time.Time

	Cached   bool   // content file is present and current
	Dirty    bool   // local content changes not yet uploaded
	DirtyGen uint64 // bumped on every write
	Moved    bool   // local rename/move not yet applied remotely

	open     int
	openW    int
	detached bool
	dl       chan struct{} // non-nil while a download is in flight
}

func (e *Entry) depth() int {
	d := 0
	for p := e.Parent; p != nil; p = p.Parent {
		d++
	}
	return d
}

func (e *Entry) needsSync() bool { return !e.detached && (e.ID == "" || e.Dirty || e.Moved) }

type Options struct {
	CacheDir      string
	CacheMaxBytes int64
	MetadataTTL   time.Duration
	SyncInterval  time.Duration
	// SyncEnabled, if non-nil, overrides the persisted sync state.
	SyncEnabled *bool
	MountPoint  string
	Logger      *slog.Logger
}

type FS struct {
	opts   Options
	remote Remote
	log    *slog.Logger

	mu          sync.Mutex
	root        *Entry
	byID        map[string]*Entry
	nextIno     uint64
	pending     map[*Entry]struct{}
	deletions   map[string]struct{}
	syncEnabled bool
	stateDirty  bool
	quota       *graph.Quota
	lastSync    time.Time
	lastErr     string
	cacheBytes  int64

	kick   chan chan error
	stop   chan struct{}
	doneWG sync.WaitGroup
}

type persistedEntry struct {
	Ino      uint64    `json:"ino"`
	Parent   uint64    `json:"parent,omitempty"`
	ID       string    `json:"id,omitempty"`
	CacheKey string    `json:"cache_key"`
	Name     string    `json:"name"`
	IsDir    bool      `json:"dir,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Mtime    time.Time `json:"mtime"`
	CTag     string    `json:"ctag,omitempty"`
	Listed   bool      `json:"listed,omitempty"`
	Cached   bool      `json:"cached,omitempty"`
	Dirty    bool      `json:"dirty,omitempty"`
	Moved    bool      `json:"moved,omitempty"`
}

type persistedState struct {
	Version     int              `json:"version"`
	NextIno     uint64           `json:"next_ino"`
	SyncEnabled bool             `json:"sync_enabled"`
	Deletions   []string         `json:"deletions,omitempty"`
	Entries     []persistedEntry `json:"entries"`
}

// New loads (or initialises) the persisted tree under opts.CacheDir.
func New(opts Options, remote Remote) (*FS, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.SyncInterval <= 0 {
		opts.SyncInterval = 30 * time.Second
	}
	if err := os.MkdirAll(filepath.Join(opts.CacheDir, "content"), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(opts.CacheDir, 0o700); err != nil {
		return nil, err
	}
	f := &FS{
		opts:        opts,
		remote:      remote,
		log:         opts.Logger,
		byID:        map[string]*Entry{},
		pending:     map[*Entry]struct{}{},
		deletions:   map[string]struct{}{},
		syncEnabled: true,
		kick:        make(chan chan error, 1),
		stop:        make(chan struct{}),
	}
	if err := f.loadState(); err != nil {
		return nil, err
	}
	if opts.SyncEnabled != nil {
		f.syncEnabled = *opts.SyncEnabled
	}
	f.cleanOrphans()
	return f, nil
}

func (f *FS) statePath() string { return filepath.Join(f.opts.CacheDir, "state.json") }

func (f *FS) cachePath(e *Entry) string {
	return filepath.Join(f.opts.CacheDir, "content", e.CacheKey)
}

func cacheKeyForID(id string) string {
	return "r-" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func localCacheKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "l-" + hex.EncodeToString(b)
}

func (f *FS) loadState() error {
	data, err := os.ReadFile(f.statePath())
	if errors.Is(err, os.ErrNotExist) {
		f.root = &Entry{Ino: 1, ID: "root", CacheKey: "root", IsDir: true, Mtime: time.Now()}
		f.byID["root"] = f.root
		f.nextIno = 2
		return nil
	}
	if err != nil {
		return err
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	f.syncEnabled = st.SyncEnabled
	f.nextIno = st.NextIno
	byIno := map[uint64]*Entry{}
	for _, pe := range st.Entries {
		e := &Entry{
			Ino: pe.Ino, ID: pe.ID, CacheKey: pe.CacheKey, Name: pe.Name, IsDir: pe.IsDir,
			Size: pe.Size, Mtime: pe.Mtime, CTag: pe.CTag, Cached: pe.Cached, Dirty: pe.Dirty, Moved: pe.Moved,
		}
		if pe.Listed {
			e.Children = map[string]*Entry{}
		}
		if pe.Ino == 1 {
			f.root = e
		} else {
			p := byIno[pe.Parent]
			if p == nil || p.Children == nil {
				continue
			}
			e.Parent = p
			p.Children[e.Name] = e
		}
		byIno[e.Ino] = e
		if e.ID != "" {
			f.byID[e.ID] = e
		}
		if e.needsSync() {
			f.pending[e] = struct{}{}
		}
	}
	if f.root == nil {
		return errors.New("state.json has no root entry")
	}
	for _, id := range st.Deletions {
		f.deletions[id] = struct{}{}
	}
	return nil
}

// saveState persists the tree. Caller must NOT hold f.mu.
func (f *FS) saveState() error {
	f.mu.Lock()
	st := persistedState{Version: 1, NextIno: f.nextIno, SyncEnabled: f.syncEnabled}
	for id := range f.deletions {
		st.Deletions = append(st.Deletions, id)
	}
	queue := []*Entry{f.root}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		pe := persistedEntry{
			Ino: e.Ino, ID: e.ID, CacheKey: e.CacheKey, Name: e.Name, IsDir: e.IsDir, Size: e.Size,
			Mtime: e.Mtime, CTag: e.CTag, Listed: e.Children != nil, Cached: e.Cached, Dirty: e.Dirty, Moved: e.Moved,
		}
		if e.Parent != nil {
			pe.Parent = e.Parent.Ino
		}
		st.Entries = append(st.Entries, pe)
		names := make([]string, 0, len(e.Children))
		for n := range e.Children {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			queue = append(queue, e.Children[n])
		}
	}
	f.stateDirty = false
	f.mu.Unlock()

	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.opts.CacheDir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.statePath())
}

// cleanOrphans removes content files no entry refers to.
func (f *FS) cleanOrphans() {
	keep := map[string]bool{}
	f.walk(f.root, func(e *Entry) {
		if e.Cached {
			keep[e.CacheKey] = true
		}
	})
	dir := filepath.Join(f.opts.CacheDir, "content")
	ents, _ := os.ReadDir(dir)
	for _, de := range ents {
		if !keep[de.Name()] {
			_ = os.Remove(filepath.Join(dir, de.Name()))
		}
	}
	f.walk(f.root, func(e *Entry) {
		if e.Cached {
			if _, err := os.Stat(f.cachePath(e)); err != nil {
				e.Cached = false
			}
		}
	})
}

func (f *FS) walk(e *Entry, fn func(*Entry)) {
	fn(e)
	for _, c := range e.Children {
		f.walk(c, fn)
	}
}

func (f *FS) markPending(e *Entry) {
	if e.needsSync() {
		f.pending[e] = struct{}{}
	}
	f.stateDirty = true
}

func (f *FS) newEntry(parent *Entry, name string, isDir bool) *Entry {
	e := &Entry{Ino: f.nextIno, Name: name, Parent: parent, IsDir: isDir, Mtime: time.Now()}
	f.nextIno++
	if parent != nil {
		parent.Children[name] = e
	}
	f.stateDirty = true
	return e
}

func (f *FS) entryFromItem(parent *Entry, it *graph.Item) *Entry {
	e := f.newEntry(parent, it.Name, it.IsDir())
	e.ID = it.ID
	e.CacheKey = cacheKeyForID(it.ID)
	e.Size = it.Size
	e.Mtime = it.LastModifiedDateTime
	e.CTag = it.CTag
	f.byID[it.ID] = e
	return e
}

// detach unlinks e from its parent. It does not schedule a remote delete.
func (f *FS) detach(e *Entry) {
	if e.Parent != nil && e.Parent.Children[e.Name] == e {
		delete(e.Parent.Children, e.Name)
	}
	e.Parent = nil
	f.stateDirty = true
}

// forget drops e and its subtree from the tree and the cache.
func (f *FS) forget(e *Entry) {
	f.detach(e)
	f.walk(e, func(x *Entry) {
		x.detached = true
		delete(f.pending, x)
		if x.ID != "" && f.byID[x.ID] == x {
			delete(f.byID, x.ID)
		}
		if x.Cached && x.open == 0 {
			_ = os.Remove(f.cachePath(x))
		}
	})
}

// remove deletes e locally and schedules the remote delete.
func (f *FS) remove(e *Entry) {
	if e.ID != "" {
		f.deletions[e.ID] = struct{}{}
	}
	f.forget(e)
}

// invalidChars are rejected by OneDrive in file and folder names.
const invalidChars = "\"*:<>?\\|"

func validName(name string) bool {
	return name != "" && len(name) <= 255 && !strings.ContainsAny(name, invalidChars) &&
		!strings.HasSuffix(name, " ") && !strings.HasSuffix(name, ".")
}

// merge reconciles a fresh remote listing of dir into the tree. Caller holds mu.
func (f *FS) merge(dir *Entry, items []graph.Item) {
	if dir.Children == nil {
		dir.Children = map[string]*Entry{}
	}
	remote := map[string]*graph.Item{}
	for i := range items {
		it := &items[i]
		if it.Folder == nil && it.File == nil {
			continue // OneNote packages etc. are not representable
		}
		if _, gone := f.deletions[it.ID]; gone {
			continue
		}
		remote[it.ID] = it
	}
	for _, c := range dir.Children {
		if c.ID != "" && remote[c.ID] == nil && !c.Dirty && !c.Moved {
			f.forget(c)
		}
	}
	for _, it := range remote {
		e := f.byID[it.ID]
		if e != nil && e.Moved {
			continue // local move wins until pushed
		}
		if e != nil && e.Parent != dir {
			f.detach(e) // moved remotely
		}
		if other := dir.Children[it.Name]; other != nil && other != e {
			if other.ID == "" || other.Dirty || other.Moved {
				continue // pending local entry with the same name wins
			}
			f.forget(other)
		}
		if e == nil {
			f.entryFromItem(dir, it)
			continue
		}
		if e.Parent != dir || e.Name != it.Name {
			if e.Parent == dir {
				delete(dir.Children, e.Name)
			}
			e.Name, e.Parent = it.Name, dir
			dir.Children[e.Name] = e
		}
		if e.Dirty {
			continue
		}
		if e.CTag != it.CTag {
			e.CTag = it.CTag
			if e.Cached && e.openW == 0 {
				e.Cached = false
				if e.open == 0 {
					_ = os.Remove(f.cachePath(e))
				}
			}
		}
		e.Size, e.Mtime = it.Size, it.LastModifiedDateTime
	}
	dir.ListedAt = time.Now()
	f.stateDirty = true
}

// ensureListed makes sure dir.Children is populated and fresh enough.
// Caller holds mu; it may be released while talking to OneDrive.
func (f *FS) ensureListed(ctx context.Context, dir *Entry) error {
	fresh := dir.Children != nil && (dir.ID == "" || time.Since(dir.ListedAt) < f.opts.MetadataTTL)
	if fresh {
		return nil
	}
	if !f.syncEnabled {
		if dir.Children != nil {
			return nil
		}
		return errSyncPaused
	}
	id := dir.ID
	f.mu.Unlock()
	items, err := f.remote.Children(ctx, id)
	f.mu.Lock()
	if err != nil {
		if dir.Children != nil {
			f.log.Warn("refresh listing failed; serving cached", "dir", f.path(dir), "err", err)
			return nil
		}
		return err
	}
	if !dir.detached && dir.ID == id {
		f.merge(dir, items)
	}
	return nil
}

var errSyncPaused = errors.New("sync is paused and the item is not cached")

func (f *FS) path(e *Entry) string {
	var parts []string
	for ; e != nil && e.Parent != nil; e = e.Parent {
		parts = append(parts, e.Name)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return "/" + strings.Join(parts, "/")
}

func (f *FS) attr(e *Entry, out *fuse.Attr) {
	out.Ino = e.Ino
	if e.IsDir {
		out.Mode = fuse.S_IFDIR | 0o755
		out.Nlink = 2
		out.Size = 4096
	} else {
		out.Mode = fuse.S_IFREG | 0o644
		out.Nlink = 1
		out.Size = uint64(e.Size)
	}
	out.Blocks = (out.Size + 511) / 512
	out.Blksize = 4096
	t := e.Mtime
	if t.IsZero() {
		t = time.Unix(0, 0)
	}
	out.SetTimes(&t, &t, &t)
	out.Uid = uint32(os.Getuid())
	out.Gid = uint32(os.Getgid())
}
