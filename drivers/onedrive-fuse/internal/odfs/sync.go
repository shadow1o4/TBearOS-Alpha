package odfs

import (
	"context"
	"errors"
	"os"
	"sort"
	"time"

	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/graph"
)

// Status is the daemon state reported over the control socket.
type Status struct {
	SyncEnabled    bool       `json:"sync_enabled"`
	MountPoint     string     `json:"mountpoint"`
	PendingChanges int        `json:"pending_changes"`
	PendingDeletes int        `json:"pending_deletes"`
	CacheBytes     int64      `json:"cache_bytes"`
	CacheMaxBytes  int64      `json:"cache_max_bytes"`
	LastSync       *time.Time `json:"last_sync,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	QuotaTotal     int64      `json:"quota_total,omitempty"`
	QuotaUsed      int64      `json:"quota_used,omitempty"`
}

func (f *FS) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := Status{
		SyncEnabled:    f.syncEnabled,
		MountPoint:     f.opts.MountPoint,
		PendingChanges: len(f.pending),
		PendingDeletes: len(f.deletions),
		CacheBytes:     f.cacheBytes,
		CacheMaxBytes:  f.opts.CacheMaxBytes,
		LastError:      f.lastErr,
	}
	if !f.lastSync.IsZero() {
		t := f.lastSync
		s.LastSync = &t
	}
	if f.quota != nil {
		s.QuotaTotal, s.QuotaUsed = f.quota.Total, f.quota.Used
	}
	return s
}

// SetSyncEnabled pauses or resumes all OneDrive traffic. While paused, cached
// files stay readable and writable and changes are queued.
func (f *FS) SetSyncEnabled(on bool) {
	f.mu.Lock()
	changed := f.syncEnabled != on
	f.syncEnabled = on
	f.stateDirty = true
	f.mu.Unlock()
	if changed {
		f.log.Info("sync state changed", "enabled", on)
	}
	if on {
		f.trigger(nil)
	}
	if err := f.saveState(); err != nil {
		f.log.Error("save state", "err", err)
	}
}

func (f *FS) SyncEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncEnabled
}

// SyncNow runs a sync pass and waits for it.
func (f *FS) SyncNow(ctx context.Context) error {
	ch := make(chan error, 1)
	select {
	case f.kick <- ch:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *FS) trigger(ch chan error) {
	select {
	case f.kick <- ch:
	default:
		if ch != nil {
			ch <- nil
		}
	}
}

// Start launches the background syncer.
func (f *FS) Start() {
	f.doneWG.Add(1)
	go f.loop()
}

// Close stops the syncer, makes a final upload attempt, and saves state.
func (f *FS) Close() error {
	close(f.stop)
	f.doneWG.Wait()
	if f.SyncEnabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := f.syncOnce(ctx); err != nil {
			f.log.Warn("final sync incomplete; changes stay queued", "err", err)
		}
		cancel()
	}
	return f.saveState()
}

func (f *FS) loop() {
	defer f.doneWG.Done()
	t := time.NewTicker(f.opts.SyncInterval)
	defer t.Stop()
	for {
		var reply chan error
		select {
		case <-f.stop:
			return
		case reply = <-f.kick:
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		err := f.syncOnce(ctx)
		cancel()
		f.evict()
		f.mu.Lock()
		dirty := f.stateDirty
		f.mu.Unlock()
		if dirty {
			if serr := f.saveState(); serr != nil {
				f.log.Error("save state", "err", serr)
			}
		}
		if reply != nil {
			reply <- err
		}
	}
}

func (f *FS) syncOnce(ctx context.Context) error {
	if !f.SyncEnabled() {
		return nil
	}
	err := f.syncPass(ctx)
	if errors.Is(err, errRetry) {
		err = f.syncPass(ctx)
	}
	if q, qerr := f.remote.Quota(ctx); qerr == nil {
		f.mu.Lock()
		f.quota = q
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.lastSync = time.Now()
	f.lastErr = ""
	if err != nil {
		f.lastErr = err.Error()
		f.log.Warn("sync pass", "err", err)
	}
	f.mu.Unlock()
	return err
}

var errRetry = errors.New("sync: conflicts pending retry")

// syncPass pushes queued local changes: folder creation and moves first
// (parents before children), then deletions, then file uploads.
func (f *FS) syncPass(ctx context.Context) error {
	f.mu.Lock()
	if f.root.ID == "root" {
		f.mu.Unlock()
		if it, err := f.remote.Root(ctx); err == nil {
			f.mu.Lock()
			delete(f.byID, "root")
			f.root.ID = it.ID
			f.byID[it.ID] = f.root
		} else {
			return err
		}
	}
	work := make([]*Entry, 0, len(f.pending))
	for e := range f.pending {
		work = append(work, e)
	}
	f.mu.Unlock()
	sort.Slice(work, func(i, j int) bool { return work[i].depth() < work[j].depth() })

	var firstErr error
	retry := false
	note := func(err error) {
		if graph.IsConflict(err) {
			retry = true
		}
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, e := range work {
		if err := f.pushStructure(ctx, e); err != nil {
			note(err)
		}
	}

	f.mu.Lock()
	dels := make([]string, 0, len(f.deletions))
	for id := range f.deletions {
		dels = append(dels, id)
	}
	f.mu.Unlock()
	for _, id := range dels {
		err := f.remote.Delete(ctx, id)
		if err == nil || graph.IsNotFound(err) {
			f.mu.Lock()
			delete(f.deletions, id)
			f.stateDirty = true
			f.mu.Unlock()
			continue
		}
		note(err)
	}

	for _, e := range work {
		if err := f.pushContent(ctx, e); err != nil {
			note(err)
		}
	}

	f.mu.Lock()
	for e := range f.pending {
		if !e.needsSync() {
			delete(f.pending, e)
		}
	}
	f.mu.Unlock()
	if retry {
		return errRetry
	}
	return firstErr
}

func (f *FS) setID(e *Entry, it *graph.Item) {
	if e.ID != "" && f.byID[e.ID] == e {
		delete(f.byID, e.ID)
	}
	e.ID = it.ID
	f.byID[it.ID] = e
	f.stateDirty = true
}

// pushStructure creates a local-only folder or applies a pending move.
func (f *FS) pushStructure(ctx context.Context, e *Entry) error {
	f.mu.Lock()
	if e.detached || e.Parent == nil || e.Parent.ID == "" {
		f.mu.Unlock()
		return nil
	}
	parentID, name, id := e.Parent.ID, e.Name, e.ID
	switch {
	case id == "" && e.IsDir:
		f.mu.Unlock()
		it, err := f.remote.Mkdir(ctx, parentID, name)
		if graph.IsConflict(err) {
			it, err = f.remote.Child(ctx, parentID, name)
			if err == nil && !it.IsDir() {
				return &graph.APIError{Status: 409, Code: "nameAlreadyExists", Message: name + " exists as a file"}
			}
		}
		if err != nil {
			return err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if e.detached {
			f.deletions[it.ID] = struct{}{}
			return nil
		}
		f.setID(e, it)
		e.CTag = it.CTag
		e.ListedAt = time.Time{} // merge remote content on next access
		if e.Parent == nil || e.Parent.ID != parentID || e.Name != name {
			e.Moved = true
		}
		f.markPending(e)
		return nil
	case id != "" && e.Moved:
		f.mu.Unlock()
		it, err := f.remote.Move(ctx, id, parentID, name)
		f.mu.Lock()
		defer f.mu.Unlock()
		if graph.IsNotFound(err) {
			// Deleted remotely: re-create from local copy if we have one.
			f.log.Warn("moved item vanished remotely", "path", f.path(e))
			e.Moved = false
			if !e.IsDir && e.Cached {
				delete(f.byID, id)
				e.ID, e.Dirty = "", true
				f.markPending(e)
			} else if !e.IsDir {
				f.forget(e)
			}
			return nil
		}
		if err != nil {
			return err
		}
		if !e.detached && e.Parent != nil && e.Parent.ID == parentID && e.Name == name {
			e.Moved = false
			f.stateDirty = true
		}
		_ = it
		return nil
	}
	f.mu.Unlock()
	return nil
}

// pushContent uploads a new or modified file from the cache.
func (f *FS) pushContent(ctx context.Context, e *Entry) error {
	f.mu.Lock()
	if e.detached || e.IsDir || (e.ID != "" && !e.Dirty) || e.openW > 0 ||
		e.Parent == nil || e.Parent.ID == "" || (e.ID != "" && e.Moved) {
		f.mu.Unlock()
		return nil
	}
	parentID, name, gen, p := e.Parent.ID, e.Name, e.DirtyGen, f.cachePath(e)
	f.mu.Unlock()

	file, err := os.Open(p)
	if err != nil {
		return err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return err
	}
	it, err := f.remote.Upload(ctx, parentID, name, file, st.Size())
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.detached {
		f.deletions[it.ID] = struct{}{}
		return nil
	}
	f.setID(e, it)
	e.CTag = it.CTag
	if e.DirtyGen == gen {
		e.Dirty = false
	}
	if e.Parent == nil || e.Parent.ID != parentID || e.Name != name {
		e.Moved = true
	}
	f.markPending(e)
	f.log.Info("uploaded", "path", f.path(e), "bytes", st.Size())
	return nil
}

// evict trims the content cache to CacheMaxBytes, least recently used
// first, never touching open or dirty files.
func (f *FS) evict() {
	f.mu.Lock()
	defer f.mu.Unlock()
	type cand struct {
		e     *Entry
		size  int64
		atime time.Time
	}
	var total int64
	var cands []cand
	f.walk(f.root, func(e *Entry) {
		if !e.Cached || e.IsDir {
			return
		}
		st, err := os.Stat(f.cachePath(e))
		if err != nil {
			return
		}
		total += st.Size()
		if !e.Dirty && e.open == 0 && e.ID != "" {
			cands = append(cands, cand{e, st.Size(), st.ModTime()})
		}
	})
	if f.opts.CacheMaxBytes > 0 && total > f.opts.CacheMaxBytes {
		sort.Slice(cands, func(i, j int) bool { return cands[i].atime.Before(cands[j].atime) })
		for _, c := range cands {
			if total <= f.opts.CacheMaxBytes {
				break
			}
			if os.Remove(f.cachePath(c.e)) == nil {
				c.e.Cached = false
				total -= c.size
				f.stateDirty = true
			}
		}
	}
	f.cacheBytes = total
}
