// Package config loads onedrive-fuse settings from defaults, the config file,
// and environment variables.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const AppName = "onedrive-fuse"

type Config struct {
	ClientID      string `json:"client_id"`
	Tenant        string `json:"tenant"`
	MountPoint    string `json:"mountpoint"`
	CacheDir      string `json:"cache_dir"`
	CacheMaxBytes int64  `json:"cache_max_bytes"`
	// SyncEnabled is the default sync state when no runtime state was saved.
	SyncEnabled bool `json:"sync_enabled"`
	// MetadataTTLSeconds is how long directory listings are trusted before
	// being refreshed from OneDrive.
	MetadataTTLSeconds int    `json:"metadata_ttl_seconds"`
	SyncIntervalSecs   int    `json:"sync_interval_seconds"`
	TokenStore         string `json:"token_store"` // auto | keyring | file
	SocketPath         string `json:"socket_path"`
	GraphURL           string `json:"graph_url"`
	AuthURL            string `json:"auth_url"`
}

func Dir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, AppName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", AppName)
}

func Path() string {
	if p := os.Getenv("ONEDRIVE_FUSE_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(Dir(), "config.json")
}

func defaultCacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, AppName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", AppName)
}

func DefaultSocketPath() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, AppName, "control.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", AppName, os.Getuid()), "control.sock")
}

func Default() Config {
	return Config{
		Tenant:             "common",
		MountPoint:         "/OneDrive",
		CacheDir:           defaultCacheDir(),
		CacheMaxBytes:      5 << 30,
		SyncEnabled:        true,
		MetadataTTLSeconds: 30,
		SyncIntervalSecs:   30,
		TokenStore:         "auto",
		SocketPath:         DefaultSocketPath(),
		GraphURL:           "https://graph.microsoft.com/v1.0",
		AuthURL:            "https://login.microsoftonline.com",
	}
}

// Load returns defaults overlaid with the config file (if present) and
// ONEDRIVE_FUSE_* environment variables.
func Load() (Config, error) {
	c := Default()
	data, err := os.ReadFile(Path())
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &c); err != nil {
			return c, fmt.Errorf("parse %s: %w", Path(), err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return c, err
	}
	env := map[string]*string{
		"ONEDRIVE_FUSE_CLIENT_ID":   &c.ClientID,
		"ONEDRIVE_FUSE_TENANT":      &c.Tenant,
		"ONEDRIVE_FUSE_MOUNTPOINT":  &c.MountPoint,
		"ONEDRIVE_FUSE_CACHE_DIR":   &c.CacheDir,
		"ONEDRIVE_FUSE_TOKEN_STORE": &c.TokenStore,
		"ONEDRIVE_FUSE_SOCKET":      &c.SocketPath,
		"ONEDRIVE_FUSE_GRAPH_URL":   &c.GraphURL,
		"ONEDRIVE_FUSE_AUTH_URL":    &c.AuthURL,
	}
	for k, p := range env {
		if v := os.Getenv(k); v != "" {
			*p = v
		}
	}
	if v := os.Getenv("ONEDRIVE_FUSE_CACHE_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return c, fmt.Errorf("ONEDRIVE_FUSE_CACHE_MAX_BYTES: %w", err)
		}
		c.CacheMaxBytes = n
	}
	return c, nil
}

// Save writes the config file with 0600 permissions.
func Save(c Config) error {
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), append(data, '\n'), 0o600)
}

func (c Config) MetadataTTL() time.Duration {
	return time.Duration(c.MetadataTTLSeconds) * time.Second
}

func (c Config) SyncInterval() time.Duration {
	if c.SyncIntervalSecs <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.SyncIntervalSecs) * time.Second
}
