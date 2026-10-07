// Command onedrive-fuse mounts Microsoft OneDrive as a FUSE filesystem.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/auth"
	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/config"
	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/control"
	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/graph"
	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/odfs"
)

var version = "dev"

const usage = `onedrive-fuse - mount Microsoft OneDrive with FUSE

Usage:
  onedrive-fuse login   [--client-id ID] [--tenant T] [--token-store auto|keyring|file]
  onedrive-fuse logout
  onedrive-fuse mount   [--mountpoint DIR] [--sync=true|false] [--cache-dir DIR]
                        [--cache-size BYTES] [--allow-other] [--debug]
  onedrive-fuse unmount [--mountpoint DIR]
  onedrive-fuse sync    on|off|now|status [--json]
  onedrive-fuse status  [--json]
  onedrive-fuse version

Config file: %s
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, config.Path())
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "login":
		err = cmdLogin(cfg, args)
	case "logout":
		err = cmdLogout(cfg)
	case "mount":
		err = cmdMount(cfg, args)
	case "unmount", "umount":
		err = cmdUnmount(cfg, args)
	case "sync":
		err = cmdSync(cfg, args)
	case "status":
		err = cmdStatus(cfg, args)
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Printf(usage, config.Path())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n"+usage, cmd, config.Path())
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "onedrive-fuse:", err)
	os.Exit(1)
}

func newOAuth(cfg config.Config) *auth.OAuth {
	return &auth.OAuth{AuthURL: cfg.AuthURL, Tenant: cfg.Tenant, ClientID: cfg.ClientID}
}

func cmdLogin(cfg config.Config, args []string) error {
	fl := flag.NewFlagSet("login", flag.ExitOnError)
	clientID := fl.String("client-id", cfg.ClientID, "Azure AD application (client) ID")
	tenant := fl.String("tenant", cfg.Tenant, "tenant: common, consumers, organizations, or a tenant ID")
	storeKind := fl.String("token-store", cfg.TokenStore, "where to keep tokens: auto, keyring, or file")
	_ = fl.Parse(args)

	changed := *clientID != cfg.ClientID || *tenant != cfg.Tenant || *storeKind != cfg.TokenStore
	cfg.ClientID, cfg.Tenant, cfg.TokenStore = *clientID, *tenant, *storeKind
	if changed {
		if err := config.Save(cfg); err != nil {
			return err
		}
	}
	store, err := auth.NewStore(cfg.TokenStore, config.Dir())
	if err != nil {
		return err
	}
	o := newOAuth(cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	dc, err := o.StartDeviceCode(ctx)
	if err != nil {
		return err
	}
	fmt.Println(dc.Message)
	tok, err := o.PollDeviceCode(ctx, dc)
	if err != nil {
		return err
	}
	if err := store.Save(tok); err != nil {
		return fmt.Errorf("save token to %s: %w", store.Name(), err)
	}
	fmt.Printf("Logged in. Token stored in %s.\n", store.Name())
	return nil
}

func cmdLogout(cfg config.Config) error {
	var errs []error
	for _, kind := range []string{"keyring", "file"} {
		s, _ := auth.NewStore(kind, config.Dir())
		if kind == "keyring" && !s.(auth.KeyringStore).Available() {
			continue
		}
		if err := s.Delete(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
		}
	}
	if len(errs) == 0 {
		fmt.Println("Logged out; stored tokens removed.")
	}
	return errors.Join(errs...)
}

func cmdMount(cfg config.Config, args []string) error {
	fl := flag.NewFlagSet("mount", flag.ExitOnError)
	mnt := fl.String("mountpoint", cfg.MountPoint, "directory to mount OneDrive on")
	syncOn := fl.Bool("sync", cfg.SyncEnabled, "start with syncing enabled (overrides the last saved state)")
	cacheDir := fl.String("cache-dir", cfg.CacheDir, "local cache directory")
	cacheSize := fl.Int64("cache-size", cfg.CacheMaxBytes, "maximum cache size in bytes")
	allowOther := fl.Bool("allow-other", false, "let other users access the mount (needs user_allow_other in /etc/fuse.conf)")
	debug := fl.Bool("debug", false, "verbose logging, including FUSE requests")
	_ = fl.Parse(args)
	syncSet := false
	fl.Visit(func(f *flag.Flag) { syncSet = syncSet || f.Name == "sync" })

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := ensureMountpoint(*mnt); err != nil {
		return err
	}
	store, err := auth.NewStore(cfg.TokenStore, config.Dir())
	if err != nil {
		return err
	}
	ts := &auth.TokenSource{OAuth: newOAuth(cfg), Store: store}
	if _, err := ts.Token(context.Background()); err != nil {
		if errors.Is(err, auth.ErrNoToken) {
			return err
		}
		log.Warn("could not obtain an access token; continuing with cached data", "err", err)
	}
	client := graph.New(cfg.GraphURL, ts)

	opts := odfs.Options{
		CacheDir:      *cacheDir,
		CacheMaxBytes: *cacheSize,
		MetadataTTL:   cfg.MetadataTTL(),
		SyncInterval:  cfg.SyncInterval(),
		MountPoint:    *mnt,
		Logger:        log,
	}
	if syncSet {
		opts.SyncEnabled = syncOn
	}
	fsys, err := odfs.New(opts, client)
	if err != nil {
		return err
	}

	sec := time.Second
	server, err := fs.Mount(*mnt, fsys.Root(), &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:     "onedrive",
			Name:       "onedrive",
			AllowOther: *allowOther,
			Debug:      *debug,
		},
		EntryTimeout:   &sec,
		AttrTimeout:    &sec,
		RootStableAttr: &fs.StableAttr{Ino: 1, Mode: fuse.S_IFDIR},
		UID:            uint32(os.Getuid()),
		GID:            uint32(os.Getgid()),
	})
	if err != nil {
		return fmt.Errorf("mount %s: %w", *mnt, err)
	}
	fsys.Start()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := control.Listen(ctx, cfg.SocketPath, fsys, log); err != nil {
		_ = server.Unmount()
		_ = fsys.Close()
		return err
	}
	log.Info("mounted", "mountpoint", *mnt, "sync", fsys.SyncEnabled(), "control", cfg.SocketPath)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Info("unmounting")
		if err := server.Unmount(); err != nil {
			log.Error("unmount failed (is the mount busy?)", "err", err)
		}
	}()
	server.Wait()
	cancel()
	log.Info("unmounted; flushing pending changes")
	return fsys.Close()
}

func ensureMountpoint(dir string) error {
	st, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			if errors.Is(err, os.ErrPermission) {
				return fmt.Errorf("cannot create %s: %w\ncreate it once with: sudo install -d -o %d -g %d %s",
					dir, err, os.Getuid(), os.Getgid(), dir)
			}
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return nil
}

func cmdUnmount(cfg config.Config, args []string) error {
	fl := flag.NewFlagSet("unmount", flag.ExitOnError)
	mnt := fl.String("mountpoint", cfg.MountPoint, "mount point")
	_ = fl.Parse(args)
	for _, bin := range []string{"fusermount3", "fusermount"} {
		if path, err := exec.LookPath(bin); err == nil {
			c := exec.Command(path, "-u", *mnt)
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			return c.Run()
		}
	}
	return errors.New("fusermount not found")
}

func printStatus(s odfs.Status, asJSON bool) {
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(s)
		return
	}
	state := "on"
	if !s.SyncEnabled {
		state = "off (paused)"
	}
	fmt.Printf("Mountpoint:      %s\n", s.MountPoint)
	fmt.Printf("Sync:            %s\n", state)
	fmt.Printf("Pending changes: %d (deletes: %d)\n", s.PendingChanges, s.PendingDeletes)
	fmt.Printf("Cache:           %s / %s\n", human(s.CacheBytes), human(s.CacheMaxBytes))
	if s.QuotaTotal > 0 {
		fmt.Printf("OneDrive quota:  %s / %s\n", human(s.QuotaUsed), human(s.QuotaTotal))
	}
	if s.LastSync != nil {
		fmt.Printf("Last sync:       %s\n", s.LastSync.Local().Format(time.RFC1123))
	}
	if s.LastError != "" {
		fmt.Printf("Last error:      %s\n", s.LastError)
	}
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func cmdStatus(cfg config.Config, args []string) error {
	fl := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fl.Bool("json", false, "machine-readable output")
	_ = fl.Parse(args)
	s, err := control.NewClient(cfg.SocketPath).Status(context.Background())
	if err != nil {
		return err
	}
	printStatus(s, *asJSON)
	return nil
}

func cmdSync(cfg config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: onedrive-fuse sync on|off|now|status [--json]")
	}
	fl := flag.NewFlagSet("sync", flag.ExitOnError)
	asJSON := fl.Bool("json", false, "machine-readable output")
	_ = fl.Parse(args[1:])
	c := control.NewClient(cfg.SocketPath)
	ctx := context.Background()
	switch args[0] {
	case "on", "off":
		if err := c.SetSync(ctx, args[0] == "on"); err != nil {
			return err
		}
		if !*asJSON {
			fmt.Printf("Sync %s.\n", map[string]string{"on": "enabled", "off": "paused"}[args[0]])
			return nil
		}
	case "now":
		s, err := c.SyncNow(ctx)
		if err != nil {
			return err
		}
		printStatus(s, *asJSON)
		return nil
	case "status":
	default:
		return fmt.Errorf("unknown sync action %q (want on, off, now, status)", args[0])
	}
	s, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]bool{"enabled": s.SyncEnabled})
	} else {
		fmt.Println(map[bool]string{true: "on", false: "off"}[s.SyncEnabled])
	}
	return nil
}
