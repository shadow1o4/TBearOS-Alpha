package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/graph/graphtest"
)

func TestFileStorePermissions(t *testing.T) {
	dir := t.TempDir()
	s := FileStore{Path: filepath.Join(dir, "sub", "token.json")}
	if _, err := s.Load(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("Load on empty store = %v, want ErrNoToken", err)
	}
	tok := &Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}
	if err := s.Save(tok); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v, want 0600", st.Mode().Perm())
	}
	if dst, _ := os.Stat(filepath.Dir(s.Path)); dst.Mode().Perm() != 0o700 {
		t.Fatalf("token dir mode %v, want 0700", dst.Mode().Perm())
	}
	got, err := s.Load()
	if err != nil || got.RefreshToken != "r" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if err := os.Chmod(s.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("Load accepted a world-readable token file")
	}
	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestDeviceCodeAndRefresh(t *testing.T) {
	srv := graphtest.New()
	defer srv.Close()
	srv.TokenExpiresIn = 60 // inside the 2-minute refresh skew: every Token() refreshes

	o := &OAuth{AuthURL: srv.AuthURL(), Tenant: "common", ClientID: "test"}
	ctx := context.Background()
	dc, err := o.StartDeviceCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dc.UserCode == "" || dc.VerificationURI == "" {
		t.Fatalf("incomplete device code: %+v", dc)
	}
	tok, err := o.PollDeviceCode(ctx, dc)
	if err != nil {
		t.Fatal(err)
	}
	store := FileStore{Path: filepath.Join(t.TempDir(), "token.json")}
	if err := store.Save(tok); err != nil {
		t.Fatal(err)
	}
	ts := &TokenSource{OAuth: o, Store: store}
	at, err := ts.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if at == tok.AccessToken {
		t.Fatal("expected a refreshed access token")
	}
	saved, _ := store.Load()
	if saved.AccessToken != at || saved.RefreshToken == tok.RefreshToken {
		t.Fatal("refreshed token was not persisted")
	}

	if _, err := (&OAuth{AuthURL: srv.AuthURL(), Tenant: "common"}).StartDeviceCode(ctx); err == nil {
		t.Fatal("expected error without client ID")
	}
}

func TestNoTokenStored(t *testing.T) {
	ts := &TokenSource{OAuth: &OAuth{}, Store: FileStore{Path: filepath.Join(t.TempDir(), "x.json")}}
	if _, err := ts.Token(context.Background()); !errors.Is(err, ErrNoToken) {
		t.Fatalf("got %v, want ErrNoToken", err)
	}
}
