// Package metadata determines episode properties for a media file.
//
// Sources are consulted in priority order and each field is taken from the
// first source that provides it:
//
//  1. embedded metadata via ffprobe (if available)
//  2. embedded metadata via the pure-Go tag reader
//  3. the filename (title, and a date such as 2024-05-01 or 20240501)
//  4. the file's modification time (date only)
package metadata

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Info is everything Pod Captain knows about one media file.
type Info struct {
	Title       string
	Description string
	Author      string
	Album       string
	Date        time.Time
	Duration    time.Duration
	HasArtwork  bool
	// ArtworkExt is ".jpg" or ".png" when HasArtwork is set.
	ArtworkExt string
	// DateSource records where Date came from ("embedded", "filename", "mtime"), for logging.
	DateSource string
}

// embedded is what an embedded-metadata reader can return. Dates are kept
// with their precision so a full date from a lower-priority source can beat
// a bare year from a higher one.
type embedded struct {
	Title       string
	Description string
	Author      string
	Album       string
	Date        date
	Duration    time.Duration
	HasArtwork  bool
	ArtworkExt  string
}

func (e *embedded) fill(o embedded) {
	fillStr(&e.Title, o.Title)
	fillStr(&e.Description, o.Description)
	fillStr(&e.Author, o.Author)
	fillStr(&e.Album, o.Album)
	if o.Date.precision > e.Date.precision {
		e.Date = o.Date
	}
	if e.Duration == 0 {
		e.Duration = o.Duration
	}
	// Artwork is served via the tag reader, so only it may set HasArtwork.
	if !e.HasArtwork && o.HasArtwork {
		e.HasArtwork, e.ArtworkExt = true, o.ArtworkExt
	}
}

func fillStr(dst *string, src string) {
	if *dst == "" {
		*dst = strings.TrimSpace(src)
	}
}

// Extractor resolves Info for files.
type Extractor struct {
	ffprobe string // empty when unavailable/disabled
	log     *slog.Logger
}

// NewExtractor returns an Extractor. ffprobePath follows the config semantics:
// "" = search $PATH, "none" = disabled, anything else = explicit binary.
func NewExtractor(ffprobePath string, log *slog.Logger) *Extractor {
	return &Extractor{ffprobe: resolveFFprobe(ffprobePath, log), log: log}
}

// FFprobe returns the ffprobe binary in use, or "" if none.
func (x *Extractor) FFprobe() string { return x.ffprobe }

// Extract reads metadata for the file at path. fi is its (already stat'ed) FileInfo.
// It never fails: missing metadata degrades to filename and mtime.
func (x *Extractor) Extract(ctx context.Context, path string, fi os.FileInfo) Info {
	var e embedded
	if x.ffprobe != "" {
		if m, err := probe(ctx, x.ffprobe, path); err != nil {
			x.log.Debug("ffprobe failed", "path", path, "err", err)
		} else {
			e.fill(m)
		}
	}
	if m, err := readTags(path); err != nil {
		x.log.Debug("tag read failed", "path", path, "err", err)
	} else {
		e.fill(m)
	}

	fn := parseFilename(filepath.Base(path))
	info := Info{
		Title:       e.Title,
		Description: e.Description,
		Author:      e.Author,
		Album:       e.Album,
		Duration:    e.Duration,
		HasArtwork:  e.HasArtwork,
		ArtworkExt:  e.ArtworkExt,
	}
	if info.Title == "" {
		info.Title = fn.Title
	}

	mtime := fi.ModTime()
	switch {
	case e.Date.precision >= precDay:
		info.Date, info.DateSource = e.Date.t, "embedded"
	case fn.Date.precision >= precDay:
		info.Date, info.DateSource = fn.Date.t, "filename"
	case e.Date.precision > precNone && !e.Date.contains(mtime):
		// Only a year/month is embedded and the mtime disagrees with it
		// (e.g. the file was copied later): trust the embedded value.
		info.Date, info.DateSource = e.Date.t, "embedded"
	default:
		info.Date, info.DateSource = mtime, "mtime"
	}
	return info
}
