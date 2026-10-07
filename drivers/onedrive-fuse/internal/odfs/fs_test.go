package odfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/graph"
	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/graph/graphtest"
)

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

type env struct {
	t        *testing.T
	srv      *graphtest.Server
	fs       *FS
	mnt      string
	cacheDir string
	server   *fuse.Server
}

func requireFUSE(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("FUSE unavailable: %v", err)
	}
	f.Close()
}

func newEnv(t *testing.T, srv *graphtest.Server, cacheDir string, mutate func(*Options)) *env {
	t.Helper()
	requireFUSE(t)
	if srv == nil {
		srv = graphtest.New()
		t.Cleanup(srv.Close)
	}
	if cacheDir == "" {
		cacheDir = t.TempDir()
	}
	opts := Options{
		CacheDir:      cacheDir,
		CacheMaxBytes: 1 << 30,
		MetadataTTL:   0,
		SyncInterval:  time.Hour,
		MountPoint:    "test",
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if testing.Verbose() {
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if mutate != nil {
		mutate(&opts)
	}
	f, err := New(opts, graph.New(srv.GraphURL(), staticToken(srv.IssueToken())))
	if err != nil {
		t.Fatal(err)
	}
	mnt := t.TempDir()
	zero := time.Duration(0)
	server, err := fs.Mount(mnt, f.Root(), &fs.Options{
		EntryTimeout:   &zero,
		AttrTimeout:    &zero,
		RootStableAttr: &fs.StableAttr{Ino: 1, Mode: fuse.S_IFDIR},
		MountOptions:   fuse.MountOptions{FsName: "onedrive-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Start()
	e := &env{t: t, srv: srv, fs: f, mnt: mnt, cacheDir: cacheDir, server: server}
	t.Cleanup(e.close)
	return e
}

func (e *env) close() {
	if e.server == nil {
		return
	}
	_ = e.server.Unmount()
	e.server.Wait()
	e.server = nil
	if err := e.fs.Close(); err != nil {
		e.t.Errorf("close: %v", err)
	}
}

func (e *env) p(rel string) string { return filepath.Join(e.mnt, rel) }

// eventually runs sync passes until cond holds. FUSE RELEASE is asynchronous
// relative to close(2), so an upload may need a pass or two to start.
func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for: %s (status %+v)", what, e.fs.Status())
		}
		_ = e.fs.SyncNow(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) remoteIs(path, want string) func() bool {
	return func() bool {
		got, ok := e.srv.Get(path)
		return ok && string(got) == want
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestBrowseAndRead(t *testing.T) {
	srv := graphtest.New()
	defer srv.Close()
	srv.PageSize = 2
	srv.Put("Documents/a.txt", []byte("alpha"))
	srv.Put("Documents/b.txt", []byte("bravo"))
	srv.Put("Documents/sub/c.txt", []byte("charlie"))
	srv.Put("top.txt", []byte("top"))
	e := newEnv(t, srv, "", nil)

	if got := names(t, e.mnt); len(got) != 2 || got[0] != "Documents" || got[1] != "top.txt" {
		t.Fatalf("root listing = %v", got)
	}
	if got := names(t, e.p("Documents")); len(got) != 3 {
		t.Fatalf("Documents listing = %v", got)
	}
	if got := mustRead(t, e.p("Documents/sub/c.txt")); got != "charlie" {
		t.Fatalf("read = %q", got)
	}
	st, err := os.Stat(e.p("Documents/a.txt"))
	if err != nil || st.Size() != 5 || st.IsDir() {
		t.Fatalf("stat = %v, %v", st, err)
	}
	if _, err := os.Stat(e.p("missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat missing = %v", err)
	}
	var sfs syscall.Statfs_t
	if err := syscall.Statfs(e.mnt, &sfs); err != nil || sfs.Blocks == 0 {
		t.Fatalf("statfs = %+v, %v", sfs, err)
	}
}

func TestWriteOperationsPropagate(t *testing.T) {
	e := newEnv(t, nil, "", nil)
	e.srv.Put("existing.txt", []byte("old content"))

	if err := os.WriteFile(e.p("new.txt"), []byte("brand new"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.eventually("new.txt uploaded", e.remoteIs("new.txt", "brand new"))

	if err := os.WriteFile(e.p("existing.txt"), []byte("replaced"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.eventually("existing.txt replaced", e.remoteIs("existing.txt", "replaced"))

	fh, err := os.OpenFile(e.p("existing.txt"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString("+more"); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	e.eventually("append uploaded", e.remoteIs("existing.txt", "replaced+more"))

	if err := os.MkdirAll(e.p("a/b/c"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.p("a/b/c/deep.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.eventually("nested file uploaded", e.remoteIs("a/b/c/deep.txt", "deep"))

	if err := os.Rename(e.p("new.txt"), e.p("a/moved.txt")); err != nil {
		t.Fatal(err)
	}
	e.eventually("move applied", func() bool { return e.remoteIs("a/moved.txt", "brand new")() && !e.srv.Exists("new.txt") })

	if err := os.Rename(e.p("a/moved.txt"), e.p("existing.txt")); err != nil {
		t.Fatal(err)
	}
	e.eventually("rename over existing", func() bool { return e.remoteIs("existing.txt", "brand new")() && !e.srv.Exists("a/moved.txt") })

	if err := os.Remove(e.p("a/b")); err == nil {
		t.Fatal("rmdir of non-empty dir succeeded")
	}
	if err := os.Truncate(e.p("existing.txt"), 5); err != nil {
		t.Fatal(err)
	}
	e.eventually("truncate uploaded", e.remoteIs("existing.txt", "brand"))

	if err := os.RemoveAll(e.p("a")); err != nil {
		t.Fatal(err)
	}
	e.eventually("tree deleted", func() bool { return !e.srv.Exists("a") })

	if err := os.WriteFile(e.p("bad:name.txt"), nil, 0o644); err == nil {
		t.Fatal("name with ':' accepted")
	}
	if got := e.srv.Paths(); len(got) != 1 || got[0] != "existing.txt" {
		t.Fatalf("remote paths = %v", got)
	}
	if s := e.fs.Status(); s.PendingChanges != 0 || s.PendingDeletes != 0 {
		t.Fatalf("pending after sync: %+v", s)
	}
}

func TestLargeFileUpload(t *testing.T) {
	e := newEnv(t, nil, "", nil)
	data := bytes.Repeat([]byte("onedrive-fuse!\n"), 900_000) // ~13 MiB → upload session
	if err := os.WriteFile(e.p("big.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	e.eventually("big file uploaded", func() bool {
		got, ok := e.srv.Get("big.bin")
		return ok && bytes.Equal(got, data)
	})
}

func TestRemoteChangesVisible(t *testing.T) {
	e := newEnv(t, nil, "", nil)
	e.srv.Put("f.txt", []byte("v1"))
	if got := mustRead(t, e.p("f.txt")); got != "v1" {
		t.Fatalf("read = %q", got)
	}
	e.srv.Put("f.txt", []byte("version two"))
	e.srv.Put("added.txt", []byte("x"))
	if got := mustRead(t, e.p("f.txt")); got != "version two" {
		t.Fatalf("after remote edit read = %q", got)
	}
	if got := names(t, e.mnt); len(got) != 2 {
		t.Fatalf("listing = %v", got)
	}
	e.srv.Remove("added.txt")
	if got := names(t, e.mnt); len(got) != 1 {
		t.Fatalf("listing after remote delete = %v", got)
	}
}

func TestSyncToggleQueuesChanges(t *testing.T) {
	e := newEnv(t, nil, "", nil)
	e.srv.Put("cached.txt", []byte("cached"))
	e.srv.Put("uncached.txt", []byte("remote only"))
	e.srv.Put("gone.txt", []byte("bye"))
	if got := mustRead(t, e.p("cached.txt")); got != "cached" {
		t.Fatal(got)
	}

	e.fs.SetSyncEnabled(false)
	before := e.srv.RequestCount("GET") + e.srv.RequestCount("PUT")

	if got := mustRead(t, e.p("cached.txt")); got != "cached" {
		t.Fatalf("cached read while paused = %q", got)
	}
	if _, err := os.ReadFile(e.p("uncached.txt")); !errors.Is(err, syscall.ENETDOWN) {
		t.Fatalf("uncached read while paused: %v, want ENETDOWN", err)
	}
	if err := os.WriteFile(e.p("offline.txt"), []byte("written offline"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.p("offline-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.p("offline-dir/x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.p("gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(e.p("cached.txt"), e.p("offline-dir/cached.txt")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := e.fs.SyncNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := e.srv.RequestCount("GET") + e.srv.RequestCount("PUT"); after != before {
		t.Fatalf("%d Graph requests while paused", after-before)
	}
	if e.srv.Exists("offline.txt") || !e.srv.Exists("gone.txt") {
		t.Fatal("changes leaked to OneDrive while paused")
	}
	if s := e.fs.Status(); s.SyncEnabled || s.PendingChanges == 0 || s.PendingDeletes != 1 {
		t.Fatalf("status while paused = %+v", s)
	}

	e.fs.SetSyncEnabled(true)
	e.eventually("queued changes pushed", func() bool {
		return e.remoteIs("offline.txt", "written offline")() &&
			e.remoteIs("offline-dir/x.txt", "x")() &&
			e.remoteIs("offline-dir/cached.txt", "cached")() &&
			!e.srv.Exists("gone.txt") && !e.srv.Exists("cached.txt")
	})
	if got := mustRead(t, e.p("uncached.txt")); got != "remote only" {
		t.Fatalf("read after resume = %q", got)
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	srv := graphtest.New()
	defer srv.Close()
	srv.Put("docs/keep.txt", []byte("keep me"))
	cache := t.TempDir()

	e := newEnv(t, srv, cache, nil)
	if got := mustRead(t, e.p("docs/keep.txt")); got != "keep me" {
		t.Fatal(got)
	}
	e.fs.SetSyncEnabled(false)
	if err := os.WriteFile(e.p("docs/pending.txt"), []byte("queued"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.close()
	if srv.Exists("docs/pending.txt") {
		t.Fatal("uploaded while paused")
	}

	// Restart: sync state (paused) and pending upload must survive; cached
	// content must be browsable without network.
	srv.SetOffline(true)
	e2 := newEnv(t, srv, cache, nil)
	if e2.fs.SyncEnabled() {
		t.Fatal("sync state not persisted")
	}
	if got := mustRead(t, e2.p("docs/keep.txt")); got != "keep me" {
		t.Fatalf("offline read after restart = %q", got)
	}
	if got := mustRead(t, e2.p("docs/pending.txt")); got != "queued" {
		t.Fatalf("pending file after restart = %q", got)
	}
	srv.SetOffline(false)
	e2.fs.SetSyncEnabled(true)
	e2.eventually("pending upload after restart", e2.remoteIs("docs/pending.txt", "queued"))
}

func TestSyncFlagOverridesPersistedState(t *testing.T) {
	cache := t.TempDir()
	e := newEnv(t, nil, cache, nil)
	e.fs.SetSyncEnabled(false)
	e.close()
	on := true
	e2 := newEnv(t, e.srv, cache, func(o *Options) { o.SyncEnabled = &on })
	if !e2.fs.SyncEnabled() {
		t.Fatal("--sync=true did not override saved state")
	}
}

func TestCacheEviction(t *testing.T) {
	e := newEnv(t, nil, "", func(o *Options) { o.CacheMaxBytes = 1500 })
	for _, n := range []string{"1", "2", "3"} {
		e.srv.Put(n, bytes.Repeat([]byte(n), 1000))
	}
	for _, n := range []string{"1", "2", "3"} {
		mustRead(t, e.p(n))
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let RELEASE land
	e.fs.evict()
	if s := e.fs.Status(); s.CacheBytes > 1500 {
		t.Fatalf("cache %d bytes after eviction", s.CacheBytes)
	}
	ents, _ := os.ReadDir(filepath.Join(e.cacheDir, "content"))
	if len(ents) != 1 {
		t.Fatalf("%d cached files, want 1 (most recent)", len(ents))
	}
	if got := mustRead(t, e.p("1")); len(got) != 1000 {
		t.Fatal("re-download after eviction failed")
	}
}
