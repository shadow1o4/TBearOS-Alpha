// Package control exposes a local JSON-over-HTTP API on a Unix socket so the
// CLI and a settings GUI can query and toggle the running daemon.
//
//	GET  /v1/status          -> odfs.Status
//	GET  /v1/sync            -> {"enabled": bool}
//	PUT  /v1/sync            <- {"enabled": bool}
//	POST /v1/sync/now        runs a sync pass and waits for it
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/odfs"
)

type Controller interface {
	Status() odfs.Status
	SetSyncEnabled(bool)
	SyncEnabled() bool
	SyncNow(ctx context.Context) error
}

type syncBody struct {
	Enabled *bool `json:"enabled"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func Handler(c Controller) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, c.Status())
	})
	mux.HandleFunc("GET /v1/sync", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"enabled": c.SyncEnabled()})
	})
	mux.HandleFunc("PUT /v1/sync", func(w http.ResponseWriter, r *http.Request) {
		var b syncBody
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b); err != nil || b.Enabled == nil {
			writeErr(w, 400, `body must be {"enabled": true|false}`)
			return
		}
		c.SetSyncEnabled(*b.Enabled)
		writeJSON(w, 200, map[string]bool{"enabled": c.SyncEnabled()})
	})
	mux.HandleFunc("POST /v1/sync/now", func(w http.ResponseWriter, r *http.Request) {
		if !c.SyncEnabled() {
			writeErr(w, 409, "sync is paused")
			return
		}
		if err := c.SyncNow(r.Context()); err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, c.Status())
	})
	return mux
}

// Listen creates the socket (0600, in a 0700 directory), replacing a stale
// one, and serves until ctx is cancelled.
func Listen(ctx context.Context, socketPath string, c Controller, log *slog.Logger) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return err
	}
	if conn, err := net.Dial("unix", socketPath); err == nil {
		conn.Close()
		return fmt.Errorf("another onedrive-fuse daemon is already listening on %s", socketPath)
	}
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: Handler(c), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		_ = os.Remove(socketPath)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("control socket", "err", err)
		}
	}()
	return nil
}

type Client struct {
	http *http.Client
}

func NewClient(socketPath string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}}
	return &Client{http: &http.Client{Transport: tr}}
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://onedrive-fuse"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("daemon not reachable (is `onedrive-fuse mount` running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("daemon: %s", e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Status(ctx context.Context) (odfs.Status, error) {
	var s odfs.Status
	return s, c.call(ctx, http.MethodGet, "/v1/status", nil, &s)
}

func (c *Client) SetSync(ctx context.Context, on bool) error {
	return c.call(ctx, http.MethodPut, "/v1/sync", map[string]bool{"enabled": on}, nil)
}

func (c *Client) SyncNow(ctx context.Context) (odfs.Status, error) {
	var s odfs.Status
	return s, c.call(ctx, http.MethodPost, "/v1/sync/now", nil, &s)
}
