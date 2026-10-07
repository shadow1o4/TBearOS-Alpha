# onedrive-fuse

Mount Microsoft OneDrive on Linux as a regular folder (default: `/OneDrive`) using FUSE and the Microsoft Graph API.

- **On-demand files**: the folder tree is listed lazily and a file is downloaded the first time you open it.
- **Local cache**: downloaded content and metadata live in `~/.cache/onedrive-fuse`, so cached files stay available offline and across restarts. The cache size is capped and the least recently used clean files are evicted first.
- **Write-back sync**: writes go to the local copy right away. A background syncer uploads new and changed files once they are closed, and applies mkdir, rename/move and delete in OneDrive. Files larger than 4 MiB use resumable upload sessions.
- **Sync toggle**: `onedrive-fuse sync off` pauses all OneDrive traffic and `sync on` resumes it. While paused, cached files are still readable and writable and changes are queued. The toggle is also available over a local JSON API for GUIs.
- **Secure OAuth2**: sign-in uses the device-code flow, so the app never sees your password. Tokens go in the desktop keyring (Secret Service) when one is available, otherwise in a `0600` file. A token file with looser permissions is refused.

## Why FUSE and not a kernel module

OneDrive is an HTTPS + OAuth2 REST service. Running that inside the kernel would mean TLS, JSON and token refresh in ring 0, with no access to the user's keyring. FUSE keeps all of that in an unprivileged user process, and the mount still behaves like a native filesystem to every application.

## Requirements

- Linux with FUSE (`fuse3` package; `/dev/fuse` must be accessible)
- Go 1.22+ to build
- An Azure AD app registration (free, about two minutes; see below)

## 1. Register an Azure app (one time)

1. Open https://entra.microsoft.com → *Applications* → *App registrations* → *New registration*.
2. Name: `onedrive-fuse`. Supported account types: *Accounts in any organizational directory and personal Microsoft accounts*. Leave the redirect URI empty.
3. Under *Authentication*, set **Allow public client flows** to **Yes**.
4. Under *API permissions*, add Microsoft Graph delegated permissions `Files.ReadWrite.All`, `User.Read` and `offline_access`.
5. Copy the **Application (client) ID**.

Personal accounts only? Use `--tenant consumers`. Work or school accounts only? Use `--tenant organizations` or your tenant ID.

## 2. Build and install

```sh
make build            # bin/onedrive-fuse, bin/mock-graph
make install          # ~/.local/bin/onedrive-fuse + systemd user unit
sudo install -d -o "$USER" -g "$USER" /OneDrive   # one-time: create the root-level mount point
```

## 3. Log in and mount

```sh
onedrive-fuse login --client-id <APP_CLIENT_ID>
onedrive-fuse mount                       # foreground; Ctrl-C to unmount
# or run it in the background:
systemctl --user enable --now onedrive-fuse
```

## CLI

```
onedrive-fuse login   [--client-id ID] [--tenant T] [--token-store auto|keyring|file]
onedrive-fuse logout
onedrive-fuse mount   [--mountpoint DIR] [--sync=true|false] [--cache-dir DIR]
                      [--cache-size BYTES] [--allow-other] [--debug]
onedrive-fuse unmount [--mountpoint DIR]
onedrive-fuse sync    on|off|now|status [--json]
onedrive-fuse status  [--json]
```

`--sync=false` on `mount` starts the daemon paused. Without the flag, the daemon keeps the last state set with `sync on|off`, which is persisted.

## Settings GUI integration

The daemon serves a small HTTP/JSON API on a Unix socket at `$XDG_RUNTIME_DIR/onedrive-fuse/control.sock`. The socket is mode `0600`, so only your user can reach it.

| Method | Path           | Body / response |
|--------|----------------|-----------------|
| GET    | `/v1/status`   | `{"sync_enabled":true,"mountpoint":"/OneDrive","pending_changes":0,"pending_deletes":0,"cache_bytes":38,"cache_max_bytes":5368709120,"last_sync":"…","last_error":"","quota_total":…,"quota_used":…}` |
| GET    | `/v1/sync`     | `{"enabled":true}` |
| PUT    | `/v1/sync`     | request `{"enabled":false}` → `{"enabled":false}` |
| POST   | `/v1/sync/now` | runs a sync pass, returns status (409 if paused) |

```sh
curl --unix-socket "$XDG_RUNTIME_DIR/onedrive-fuse/control.sock" -X PUT \
     -d '{"enabled":false}' http://localhost/v1/sync
```

## Configuration

`~/.config/onedrive-fuse/config.json` (written by `login`). Every key is optional:

```json
{
  "client_id": "…",
  "tenant": "common",
  "mountpoint": "/OneDrive",
  "cache_dir": "~/.cache/onedrive-fuse",
  "cache_max_bytes": 5368709120,
  "sync_enabled": true,
  "metadata_ttl_seconds": 30,
  "sync_interval_seconds": 30,
  "token_store": "auto"
}
```

Environment overrides: `ONEDRIVE_FUSE_CLIENT_ID`, `_TENANT`, `_MOUNTPOINT`, `_CACHE_DIR`, `_CACHE_MAX_BYTES`, `_TOKEN_STORE`, `_SOCKET`, `_GRAPH_URL`, `_AUTH_URL`.

## Semantics and limitations

- **Paused sync** means no network at all. Opening a file that isn't cached, or listing a folder that was never listed, fails with `ENETDOWN` ("Network is down").
- **Conflicts**: last writer wins. A locally modified file overwrites the OneDrive version when it is uploaded.
- **Remote changes** are picked up when a folder's listing is older than `metadata_ttl_seconds`. The Graph delta API is not used yet.
- Names containing `" * : < > ? \ |`, or ending in a space or `.`, are rejected (`EINVAL`) because OneDrive does not allow them.
- No symlinks, hard links, xattrs or Unix permissions. Everything shows as `0644`/`0755` and is owned by the mounting user.
- OneNote notebooks (Graph "package" items) are hidden.

## Architecture

```
cmd/onedrive-fuse         CLI + daemon entry point
cmd/mock-graph            fake OneDrive server for trying the CLI without an account
internal/auth             device-code OAuth2, refresh, keyring/file token stores
internal/graph            minimal Graph client (list, download, upload sessions, mkdir, move, delete, quota)
internal/odfs             FUSE nodes, metadata tree, persistent state, syncer, cache eviction
internal/control          Unix-socket JSON API + client
internal/graph/graphtest  in-memory fake Graph/login server used by tests
```

## Development

```sh
make test          # unit tests + real FUSE mount tests against the fake Graph server
./bin/mock-graph & # prints the env vars that point the CLI at the fake server
```
