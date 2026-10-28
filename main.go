package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	// Before flag.Parse: the subcommand has its own flags.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := runMigrate(os.Args[2:], os.Getenv, os.Stdout); err != nil {
			log.Fatalf("migrate: %v", err)
		}
		return
	}
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz and exit 0/1 (for Docker)")
	flag.Parse()
	if *healthcheck {
		// Deliberately does not call LoadConfig: it needs no other env.
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":8080"
		}
		os.Exit(probe(addr))
	}
	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Auth == "none" {
		log.Print("WARNING: AUTH=none — anyone who can reach this address can use grab and fill your disk. Put an authenticating reverse proxy in front of it.")
	}
	sharesDir, enc, device, addr := cfg.SharesDir, cfg.Encoder, cfg.VAAPIDevice, cfg.ListenAddr

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	st, err := OpenStore(filepath.Join(cfg.DataDir, "grab.sqlite3"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	auth, err := newAuth(cfg, st)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	conv, err := NewConverter("ffmpeg", enc, device, filepath.Join(cfg.DataDir, "cache"))
	if err != nil {
		log.Fatalf("cache dir: %v", err)
	}
	tmpDir := filepath.Join(cfg.DataDir, "tmp") // apprise config files; URLs carry credentials
	hc := &http.Client{Timeout: 10 * time.Second}
	prober := NewFFprobe("ffprobe")
	tracker := &Tracker{Store: st, MT: &MeTube{Base: cfg.MeTubeURL, HC: hc}, SharesDir: sharesDir, Conv: conv,
		Prober: prober}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		log.Fatalf("tmp dir: %v", err)
	}
	notifier := &Notifier{Store: st, Sender: AppriseCLI{TmpDir: tmpDir}, BaseURL: cfg.BaseURL,
		InstanceURLs: cfg.AppriseURLs, Mode: cfg.Auth}
	tracker.Notify = notifier.Notify
	if err := os.MkdirAll(cfg.CookiesDir, 0o700); err != nil {
		log.Fatalf("cookies dir: %v", err)
	}
	cookies := &CookieStore{Dir: cfg.CookiesDir}
	tracker.Cookies = cookies
	lib := &Library{Store: st, SharesDir: sharesDir, Conv: conv}
	if cfg.Jellyfin.Enabled() {
		lib.Jellyfin = &Jellyfin{URL: cfg.Jellyfin.URL, APIKey: cfg.Jellyfin.APIKey, LibraryID: cfg.Jellyfin.LibraryID, HC: hc}
	}
	static, _ := fs.Sub(webFS, "web")
	app := &App{Static: static, Store: st, SharesDir: sharesDir, Prober: prober, Conv: conv, Library: lib, Auth: auth, Cookies: cookies, Notifier: notifier,
		Nudge: tracker.Nudge, HC: hc,
		Health: func() map[string]error {
			checks := map[string]error{}
			if cfg.HealthcheckFile != "" {
				// Mount probe, not just an app probe.
				_, checks["nfs"] = os.Stat(filepath.Join(sharesDir, cfg.HealthcheckFile))
			}
			if enc == EncVAAPI {
				f, err := os.OpenFile(device, os.O_RDWR, 0)
				if err == nil {
					f.Close()
				}
				checks["vaapi"] = err
			}
			return checks
		}}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go conv.Run(ctx)
	go tracker.Run(ctx, 3*time.Second)
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if n, err := conv.Sweep(7*24*time.Hour, time.Now()); n > 0 || err != nil {
				log.Printf("cache sweep: removed %d, err %v", n, err)
			}
			// Retention is opt-in (DELETE_AFTER_DAYS); 0 makes Sweep a no-op.
			if n, err := lib.Sweep(ctx, cfg.DeleteAfter, time.Now()); n > 0 || err != nil {
				log.Printf("retention sweep: removed %d, err %v", n, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	if msg := notifier.StartupNotice(); msg != "" {
		log.Print(msg)
	}

	srv := &http.Server{Addr: addr, Handler: app.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("grab %s listening on %s (encoder %s)", version, addr, enc)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
}

// newAuth picks the implementation for AUTH. OIDC discovery runs here, so a
// wrong issuer stops startup instead of failing the first login.
func newAuth(cfg Config, st *Store) (Auth, error) {
	if cfg.Auth == "oidc" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return NewOIDCAuth(ctx, cfg, st)
	}
	return NewNoAuth(st)
}

func probe(addr string) int {
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return 1
	}
	return 0
}
