// Command podcasts serves the podcast and video-channel library: web UI, JSON API, refreshes and downloader engine streams.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"podcasts/internal/api"
	"podcasts/internal/config"
	"podcasts/internal/engine"
	"podcasts/internal/feed"
	"podcasts/internal/jobs"
	"podcasts/internal/library"
	"podcasts/internal/signin"
	"podcasts/internal/status"
	"podcasts/internal/store"
	"podcasts/internal/stream"
	"podcasts/internal/web"
)

var version = "dev"

const usage = `usage: podcasts <command>

commands:
  serve      run the web UI, the app API, the scheduler and downloader engine streams
  password   print a random PODCASTS_PASSWORD and PODCASTS_MASTER_KEY for a new install
`

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = serve(ctx, log)
	case "password":
		err = password()
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func password() error {
	pw := make([]byte, 18)
	key := make([]byte, 32)
	if _, err := rand.Read(pw); err != nil {
		return err
	}
	if _, err := rand.Read(key); err != nil {
		return err
	}
	fmt.Printf("PODCASTS_PASSWORD=%s\nPODCASTS_MASTER_KEY=%s\n", hex.EncodeToString(pw), hex.EncodeToString(key))
	return nil
}

func serve(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DB, cfg.MasterKey)
	if err != nil {
		return err
	}
	defer st.Close()
	fc := feed.NewClient(feed.Guard{Local: cfg.AllowLocalNetwork})
	var eng engine.Engine = engine.None{}
	if cfg.Engine != "" {
		eng = engine.NewCommand(cfg.Engine, cfg.EngineArgs, engine.CommandOptions{URL: cfg.EngineURL, Guard: fc.Guard})
	}
	track := status.New()
	jm := jobs.New(st, fc, eng, track, log)
	lib := library.NewLoader(st)
	if cfg.Demo {
		if err := seedDemo(ctx, st, jm, lib); err != nil {
			return err
		}
	}
	ss, err := stream.New(ctx, st, eng, track, log, stream.Options{MasterKey: cfg.MasterKey, FFmpeg: "ffmpeg", FFprobe: "ffprobe", Client: fc, EnsureEngine: jm.EnsureEngine, CacheDir: cfg.Cache})
	if err != nil {
		return err
	}
	gate := signin.New(cfg.Password, signin.Throttle(!cfg.Demo))
	ui, err := web.New(st, lib, jm, track, ss, fc, gate, log, web.Options{Password: cfg.Password, MasterKey: cfg.MasterKey, SecureCookie: cfg.SecureCookie, Demo: cfg.Demo, LockSettings: cfg.LockSettings})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("GET /stream/{id}", ss)
	api.New(st, lib, jm, track, ss, fc, gate, log, api.Options{Demo: cfg.Demo, LockSettings: cfg.LockSettings}).Routes(mux)
	ui.Routes(mux)
	nosniff := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: cfg.Addr, Handler: nosniff, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}

	done := make(chan struct{})
	go func() { jm.Run(ctx); close(done) }()
	go jm.EnsureEngine(ctx)
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "version", version, "demo", cfg.Demo, "lock_settings", cfg.LockSettings, "allow_local_network", cfg.AllowLocalNetwork)
		errc <- srv.ListenAndServe()
	}()
	var runErr error
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
	<-done
	return runErr
}
