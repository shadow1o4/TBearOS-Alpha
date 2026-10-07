package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zalando/go-keyring"
)

// ErrNoToken is returned when no token has been stored (user not logged in).
var ErrNoToken = errors.New("not logged in: run `onedrive-fuse login`")

type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	Expiry       time.Time `json:"expiry"`
}

func (t *Token) Valid(skew time.Duration) bool {
	return t != nil && t.AccessToken != "" && time.Now().Add(skew).Before(t.Expiry)
}

type Store interface {
	Load() (*Token, error)
	Save(*Token) error
	Delete() error
	Name() string
}

const keyringService = "onedrive-fuse"

// KeyringStore keeps the token in the desktop Secret Service (GNOME Keyring,
// KWallet, ...) via D-Bus.
type KeyringStore struct{ Account string }

func (k KeyringStore) Name() string { return "keyring" }

func (k KeyringStore) Load() (*Token, error) {
	s, err := keyring.Get(keyringService, k.Account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNoToken
	}
	if err != nil {
		return nil, err
	}
	var t Token
	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return nil, fmt.Errorf("decode keyring token: %w", err)
	}
	return &t, nil
}

func (k KeyringStore) Save(t *Token) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return keyring.Set(keyringService, k.Account, string(b))
}

func (k KeyringStore) Delete() error {
	err := keyring.Delete(keyringService, k.Account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// Available reports whether a Secret Service is reachable.
func (k KeyringStore) Available() bool {
	_, err := keyring.Get(keyringService, "__probe__")
	return err == nil || errors.Is(err, keyring.ErrNotFound)
}

// FileStore keeps the token in a 0600 file inside a 0700 directory.
type FileStore struct{ Path string }

func (f FileStore) Name() string { return "file:" + f.Path }

func (f FileStore) Load() (*Token, error) {
	st, err := os.Stat(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoToken
	}
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("refusing to use %s: permissions %v are too open (want 0600)", f.Path, st.Mode().Perm())
	}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("decode %s: %w", f.Path, err)
	}
	return &t, nil
}

func (f FileStore) Save(t *Token) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.Path)
}

func (f FileStore) Delete() error {
	err := os.Remove(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// NewStore picks a token store. kind is "keyring", "file", or "auto" (keyring
// when a Secret Service is available, otherwise file).
func NewStore(kind, configDir string) (Store, error) {
	file := FileStore{Path: filepath.Join(configDir, "token.json")}
	kr := KeyringStore{Account: "default"}
	switch kind {
	case "file":
		return file, nil
	case "keyring":
		return kr, nil
	case "", "auto":
		if kr.Available() {
			return kr, nil
		}
		return file, nil
	}
	return nil, fmt.Errorf("unknown token store %q (want auto, keyring, or file)", kind)
}
