// Package server serves the feed, media files and artwork over HTTP(S).
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/silevitas/podcaptain/internal/config"
	"github.com/silevitas/podcaptain/internal/feed"
	"github.com/silevitas/podcaptain/internal/library"
	"github.com/silevitas/podcaptain/internal/metadata"
)

// Server publishes a library as a podcast feed.
type Server struct {
	cfg     config.Config
	lib     *library.Library
	log     *slog.Logger
	version string
	urls    URLs
	feed    atomic.Pointer[renderedFeed]
}

type renderedFeed struct {
	body    []byte
	etag    string
	modTime time.Time
}

// New creates a Server. cfg.Server.BaseURL must already be set.
func New(cfg config.Config, lib *library.Library, log *slog.Logger, version string) *Server {
	return &Server{
		cfg:     cfg,
		lib:     lib,
		log:     log,
		version: version,
		urls:    URLs{Base: cfg.Server.BaseURL + prefix(cfg.Server.Token), ShowImagePath: cfg.Feed.Image},
	}
}

func prefix(token string) string {
	if token == "" {
		return ""
	}
	return "/" + token
}

// FeedURL is the URL to subscribe to.
func (s *Server) FeedURL() string { return s.urls.Feed() }

// Update re-renders the feed from a snapshot. Safe for concurrent use.
func (s *Server) Update(snap *library.Snapshot) {
	body, err := feed.Render(s.cfg.Feed, snap, s.urls, s.version)
	if err != nil {
		s.log.Error("render feed", "err", err)
		return
	}
	sum := sha256.Sum256(body)
	s.feed.Store(&renderedFeed{
		body:    body,
		etag:    `"` + hex.EncodeToString(sum[:8]) + `"`,
		modTime: snap.ChangedAt,
	})
}

// WriteFeed writes the most recently rendered feed to w.
func (s *Server) WriteFeed(w io.Writer) error {
	f := s.feed.Load()
	if f == nil {
		return errors.New("feed has not been rendered")
	}
	_, err := w.Write(f.body)
	return err
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	p := prefix(s.cfg.Server.Token)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET "+p+"/feed.xml", s.serveFeed)
	mux.HandleFunc("GET "+p+"/media/{id}/{name}", s.serveMedia)
	mux.HandleFunc("GET "+p+"/art/{file}", s.serveArtwork)
	if img := s.cfg.Feed.Image; img != "" && !isURL(img) {
		mux.HandleFunc("GET "+p+"/cover"+strings.ToLower(filepath.Ext(img)), s.serveCover)
	}
	return s.logRequests(mux)
}

// ListenAndServe runs the server until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Server.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: media responses can legitimately stream for a long time.
	}
	errc := make(chan error, 1)
	go func() {
		if tls := s.cfg.Server.TLS; tls.Enabled() {
			errc <- srv.ListenAndServeTLS(tls.CertFile, tls.KeyFile)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func (s *Server) serveFeed(w http.ResponseWriter, r *http.Request) {
	f := s.feed.Load()
	if f == nil {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "feed not ready yet, initial scan in progress", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("ETag", f.etag)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "feed.xml", f.modTime, bytes.NewReader(f.body))
}

func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request) {
	// Only files present in the current index are served, so arbitrary
	// paths can never be requested.
	e, ok := s.lib.Snapshot().Lookup(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(e.Path)
	if err != nil {
		s.log.Warn("open media", "path", e.Path, "err", err)
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", e.MIME)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	// ServeContent handles HEAD, Range and conditional requests, all of which
	// Apple Podcasts relies on for streaming and seeking.
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

func (s *Server) serveArtwork(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	id := strings.TrimSuffix(file, filepath.Ext(file))
	e, ok := s.lib.Snapshot().Lookup(id)
	if !ok || !e.Info.HasArtwork {
		http.NotFound(w, r)
		return
	}
	data, mime, err := metadata.Artwork(e.Path)
	if err != nil {
		if !errors.Is(err, metadata.ErrNoArtwork) {
			s.log.Warn("read artwork", "path", e.Path, "err", err)
		}
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "", e.ModTime, bytes.NewReader(data))
}

func (s *Server) serveCover(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(s.cfg.Feed.Image)
	if err != nil {
		s.log.Warn("open show image", "path", s.cfg.Feed.Image, "err", err)
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ReadFrom keeps the underlying writer's sendfile fast path for media.
func (r *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	return io.Copy(r.ResponseWriter, src)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	token := s.cfg.Server.Token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		path := r.URL.Path
		if token != "" {
			path = strings.Replace(path, "/"+token, "/<token>", 1)
		}
		s.log.Debug("request", "method", r.Method, "path", path, "status", rec.status,
			"range", r.Header.Get("Range"), "remote", r.RemoteAddr, "ua", r.UserAgent(),
			"took", time.Since(start).Round(time.Millisecond))
	})
}

// URLs builds the absolute URLs used in the feed.
type URLs struct {
	Base          string // base URL including token prefix, no trailing slash
	ShowImagePath string // local path or http(s) URL
}

func (u URLs) Feed() string { return u.Base + "/feed.xml" }

func (u URLs) Media(e library.Episode) string {
	return u.Base + "/media/" + e.ID + "/" + url.PathEscape(asciiName(filepath.Base(e.Path)))
}

// Artwork URLs carry a .jpg/.png extension because Apple Podcasts expects one.
func (u URLs) Artwork(e library.Episode) string { return u.Base + "/art/" + e.ID + e.Info.ArtworkExt }

func (u URLs) ShowImage() string {
	switch {
	case u.ShowImagePath == "":
		return ""
	case isURL(u.ShowImagePath):
		return u.ShowImagePath
	default:
		return u.Base + "/cover" + strings.ToLower(filepath.Ext(u.ShowImagePath))
	}
}

// asciiName makes a URL-safe ASCII file name; Apple recommends ASCII-only
// media URLs. The name is cosmetic: files are looked up by ID.
func asciiName(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range name {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_') {
			b.WriteRune(r)
			dash = false
		} else if !dash { // any other run of characters, '-' included, becomes one '-'
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" || strings.HasPrefix(out, ".") {
		out = "media" + out
	}
	return out
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
