// Package library scans the media folder and maintains the current episode list.
package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/silevitas/podcaptain/internal/config"
	"github.com/silevitas/podcaptain/internal/metadata"
)

// Episode is one media file in the library.
type Episode struct {
	// ID is a stable identifier derived from the path relative to the library root.
	ID      string
	Path    string
	RelPath string
	Size    int64
	ModTime time.Time
	MIME    string
	Info    metadata.Info
}

// Snapshot is an immutable view of the library after a scan.
type Snapshot struct {
	// Episodes are sorted chronologically, oldest first.
	Episodes  []Episode
	ScannedAt time.Time
	// ChangedAt is when the episode set (or any episode's file) last changed.
	ChangedAt time.Time
	byID      map[string]int
}

// Lookup returns the episode with the given ID.
func (s *Snapshot) Lookup(id string) (Episode, bool) {
	if s == nil {
		return Episode{}, false
	}
	i, ok := s.byID[id]
	if !ok {
		return Episode{}, false
	}
	return s.Episodes[i], true
}

// Library scans a folder tree for media files.
type Library struct {
	cfg     config.Library
	x       *metadata.Extractor
	workers int
	log     *slog.Logger
	now     func() time.Time

	scanMu sync.Mutex
	cache  map[string]Episode // keyed by RelPath; reused while size+mtime are unchanged
	snap   atomic.Pointer[Snapshot]
}

// New creates a Library. No scan is performed until Scan or Run is called.
func New(cfg config.Library, x *metadata.Extractor, workers int, log *slog.Logger) *Library {
	return &Library{
		cfg:     cfg,
		x:       x,
		workers: max(workers, 1),
		log:     log,
		now:     time.Now,
		cache:   map[string]Episode{},
	}
}

// Snapshot returns the most recent scan result (nil before the first successful scan).
func (l *Library) Snapshot() *Snapshot { return l.snap.Load() }

// Run scans immediately, then every interval and whenever trigger fires,
// calling onScan after each successful scan. It returns when ctx is done.
func (l *Library) Run(ctx context.Context, interval time.Duration, trigger <-chan struct{}, onScan func(*Snapshot)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if snap, err := l.Scan(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Keep serving the previous snapshot: if the folder is on a volume
			// that is temporarily unmounted, we must not publish an empty feed.
			l.log.Error("scan failed; keeping previous feed", "err", err)
		} else if onScan != nil {
			onScan(snap)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-trigger:
		}
	}
}

type candidate struct {
	rel, abs string
	fi       os.FileInfo
}

// Scan walks the library folder and publishes a new Snapshot.
func (l *Library) Scan(ctx context.Context) (*Snapshot, error) {
	l.scanMu.Lock()
	defer l.scanMu.Unlock()
	start := l.now()

	root := l.cfg.Path
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("library folder: %w", err)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("library folder %s is not a directory", root)
	}

	exts := make(map[string]bool, len(l.cfg.Extensions))
	for _, e := range l.cfg.Extensions {
		exts[e] = true
	}
	settleBefore := start.Add(-l.cfg.SettleTime.D())

	var cands []candidate
	var settling int
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if p == root {
				return err
			}
			l.log.Warn("skipping unreadable path", "path", p, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p != root && !l.cfg.IncludeHidden && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// Directory symlinks are not followed (WalkDir reports them as non-dirs,
		// and the Stat below rejects them) to avoid cycles.
		if d.IsDir() {
			return nil
		}
		if !exts[strings.ToLower(strings.TrimPrefix(filepath.Ext(p), "."))] {
			return nil
		}
		fi, err := os.Stat(p) // follows file symlinks
		if err != nil {
			l.log.Warn("skipping file", "path", p, "err", err)
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		if fi.ModTime().After(settleBefore) {
			settling++
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		cands = append(cands, candidate{rel: filepath.ToSlash(rel), abs: p, fi: fi})
		return nil
	})
	if err != nil {
		return nil, err
	}

	episodes, extracted := l.resolve(ctx, cands)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	slices.SortFunc(episodes, func(a, b Episode) int {
		if c := a.Info.Date.Compare(b.Info.Date); c != 0 {
			return c
		}
		return strings.Compare(a.RelPath, b.RelPath)
	})
	snap := &Snapshot{Episodes: episodes, ScannedAt: start, byID: make(map[string]int, len(episodes))}
	newCache := make(map[string]Episode, len(episodes))
	for i, e := range episodes {
		snap.byID[e.ID] = i
		newCache[e.RelPath] = e
	}

	added, removed := 0, 0
	for k := range newCache {
		if _, ok := l.cache[k]; !ok {
			added++
		}
	}
	for k := range l.cache {
		if _, ok := newCache[k]; !ok {
			removed++
		}
	}
	changedAt := start
	if prev := l.snap.Load(); prev != nil && added == 0 && removed == 0 && extracted == 0 {
		changedAt = prev.ChangedAt
	}
	snap.ChangedAt = changedAt
	l.cache = newCache
	l.snap.Store(snap)

	l.log.Info("scan complete",
		"episodes", len(episodes), "added", added, "removed", removed,
		"settling", settling, "took", time.Since(start).Round(time.Millisecond))
	return snap, nil
}

// resolve returns an Episode per candidate, reusing cached metadata for
// unchanged files and extracting it concurrently for new or modified ones.
func (l *Library) resolve(ctx context.Context, cands []candidate) ([]Episode, int) {
	out := make([]Episode, len(cands))
	var todo []int
	for i, c := range cands {
		if e, ok := l.cache[c.rel]; ok && e.Size == c.fi.Size() && e.ModTime.Equal(c.fi.ModTime()) {
			out[i] = e
			continue
		}
		todo = append(todo, i)
	}

	var wg sync.WaitGroup
	jobs := make(chan int)
	for range min(l.workers, len(todo)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				c := cands[i]
				info := l.x.Extract(ctx, c.abs, c.fi)
				out[i] = Episode{
					ID:      episodeID(c.rel),
					Path:    c.abs,
					RelPath: c.rel,
					Size:    c.fi.Size(),
					ModTime: c.fi.ModTime(),
					MIME:    MIMEType(c.abs),
					Info:    info,
				}
				l.log.Debug("indexed", "path", c.rel, "title", info.Title,
					"date", info.Date.Format(time.RFC3339), "date_source", info.DateSource,
					"duration", info.Duration.Round(time.Second))
			}
		}()
	}
	for _, i := range todo {
		if ctx.Err() != nil {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out, len(todo)
}

func episodeID(rel string) string {
	sum := sha256.Sum256([]byte(rel))
	return hex.EncodeToString(sum[:12])
}

var mimeTypes = map[string]string{
	".mp3":  "audio/mpeg",
	".m4a":  "audio/x-m4a",
	".m4b":  "audio/x-m4a",
	".aac":  "audio/aac",
	".wav":  "audio/wav",
	".flac": "audio/flac",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".opus": "audio/opus",
	".mp4":  "video/mp4",
	".m4v":  "video/x-m4v",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".webm": "video/webm",
}

// MIMEType returns the enclosure MIME type for a media file.
func MIMEType(path string) string {
	if t, ok := mimeTypes[strings.ToLower(filepath.Ext(path))]; ok {
		return t
	}
	return "application/octet-stream"
}
