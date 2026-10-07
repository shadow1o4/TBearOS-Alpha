package control

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/odfs"
)

type fakeCtl struct {
	on    bool
	syncs int
}

func (f *fakeCtl) Status() odfs.Status             { return odfs.Status{SyncEnabled: f.on, MountPoint: "/OneDrive"} }
func (f *fakeCtl) SetSyncEnabled(v bool)           { f.on = v }
func (f *fakeCtl) SyncEnabled() bool               { return f.on }
func (f *fakeCtl) SyncNow(ctx context.Context) error { f.syncs++; return nil }

func TestControlSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "odctl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "run", "control.sock")
	ctl := &fakeCtl{on: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Listen(ctx, sock, ctl, log); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(sock); st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, want 0600", st.Mode().Perm())
	}
	if err := Listen(ctx, sock, ctl, log); err == nil {
		t.Fatal("second daemon on the same socket should fail")
	}
	c := NewClient(sock)
	if err := c.SetSync(ctx, false); err != nil {
		t.Fatal(err)
	}
	s, err := c.Status(ctx)
	if err != nil || s.SyncEnabled || s.MountPoint != "/OneDrive" {
		t.Fatalf("status = %+v, %v", s, err)
	}
	if _, err := c.SyncNow(ctx); err == nil {
		t.Fatal("sync now while paused should fail")
	}
	if err := c.SetSync(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SyncNow(ctx); err != nil || ctl.syncs != 1 {
		t.Fatalf("sync now: %v (syncs=%d)", err, ctl.syncs)
	}
}
