// Command podcaptain (Pod Captain) publishes a folder of audio/video files as a podcast feed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/silevitas/podcaptain/internal/config"
	"github.com/silevitas/podcaptain/internal/inject"
	"github.com/silevitas/podcaptain/internal/library"
	"github.com/silevitas/podcaptain/internal/metadata"
	"github.com/silevitas/podcaptain/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "podcaptain:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to config.yaml (default: first of "+strings.Join(config.SearchPaths(), ", ")+")")
	once := flag.Bool("once", false, "scan once, print the feed XML to stdout and exit")
	check := flag.Bool("check", false, "validate the config and exit")
	injectNow := flag.Bool("inject-now", false, "process everything in the inject folder immediately (no settle wait) and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("Pod Captain", version)
		return nil
	}

	path := *configPath
	if path == "" {
		var err error
		if path, err = config.Find(); err != nil {
			return err
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if *check {
		fmt.Printf("%s: OK\n", path)
		return nil
	}

	log := newLogger(cfg.Log, *once)
	if cfg.Server.BaseURL == "" {
		cfg.Server.BaseURL = defaultBaseURL(cfg.Server)
		log.Warn("server.base_url not set; using a guess. Set it to the URL your podcast app can reach (https recommended).",
			"base_url", cfg.Server.BaseURL)
	} else if strings.HasPrefix(cfg.Server.BaseURL, "http://") {
		log.Warn("server.base_url is plain http; Apple Podcasts may silently fail to follow http feeds. Use https (see README).")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	x := metadata.NewExtractor(cfg.Metadata.FFprobe, log)
	lib := library.New(cfg.Library, x, cfg.Metadata.Workers, log)
	srv := server.New(cfg, lib, log, version)

	var inj *inject.Injector
	if cfg.Inject.Enabled || *injectNow {
		if !cfg.Inject.Enabled {
			return errors.New("-inject-now requires inject.enabled: true")
		}
		if inj, err = inject.New(ctx, cfg.Inject, cfg.Library.Path, cfg.Metadata.FFprobe, log); err != nil {
			return err
		}
	}
	if *injectNow {
		n := inj.Pass(ctx, true)
		fmt.Printf("injected %d file(s)\n", n)
		return nil
	}

	if *once {
		snap, err := lib.Scan(ctx)
		if err != nil {
			return err
		}
		srv.Update(snap)
		return srv.WriteFeed(os.Stdout)
	}

	log.Info("starting Pod Captain", "version", version, "config", path, "library", cfg.Library.Path,
		"interval", cfg.Library.ScanInterval.D(), "ffprobe", x.FFprobe(), "listen", cfg.Server.Listen)
	log.Info("subscribe to this URL in your podcast app", "feed", srv.FeedURL())

	// SIGHUP triggers an immediate rescan.
	rescan := make(chan struct{}, 1)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			select {
			case rescan <- struct{}{}:
			default:
			}
		}
	}()

	go lib.Run(ctx, cfg.Library.ScanInterval.D(), rescan, srv.Update)
	if inj != nil {
		log.Info("injector enabled", "inject", cfg.Inject.Path, "interval", cfg.Inject.ScanInterval.D(),
			"settle", cfg.Inject.SettleTime.D(), "aac_encoder", inj.AACEncoder())
		requestRescan := func() {
			select {
			case rescan <- struct{}{}:
			default:
			}
		}
		// Rescan right after injecting so new episodes appear promptly, and again
		// once library.settle_time has passed: outputs keep their source's mtime,
		// so a freshly copied source can still be inside the library's settle window.
		go inj.Run(ctx, func() {
			requestRescan()
			time.AfterFunc(cfg.Library.SettleTime.D()+time.Second, requestRescan)
		})
	}

	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("stopped")
	return nil
}

func newLogger(c config.Log, quiet bool) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level))
	if quiet && level < slog.LevelWarn {
		level = slog.LevelWarn
	}
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func defaultBaseURL(s config.Server) string {
	scheme := "http"
	if s.TLS.Enabled() {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(s.Listen)
	if err != nil {
		port = "8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		if h, err := os.Hostname(); err == nil {
			host = h
		} else {
			host = "localhost"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}
