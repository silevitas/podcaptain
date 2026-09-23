// Package inject implements the file injector: files dropped into the inject
// folder are converted with ffmpeg into a format Apple Podcasts can play and
// moved into the library, into a matching subfolder when one can be found.
package inject

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/example/podcaptain/internal/config"
)

// Injector watches the inject folder.
type Injector struct {
	cfg      config.Inject
	library  string
	ffmpeg   string
	ffprobe  string
	aac      string
	log      *slog.Logger
	now      func() time.Time
	observed map[string]observation
}

// observation tracks a file across scans to decide when it has settled.
type observation struct {
	size  int64
	mtime time.Time
	since time.Time // when this size+mtime was first seen
}

// New validates that ffmpeg and ffprobe are usable and returns an Injector.
// ffprobe follows the metadata.ffprobe config ("" or "none" → search $PATH).
func New(ctx context.Context, cfg config.Inject, libraryPath, ffprobe string, log *slog.Logger) (*Injector, error) {
	ffmpeg, err := exec.LookPath(orDefault(cfg.FFmpeg, "ffmpeg"))
	if err != nil {
		return nil, fmt.Errorf("inject: ffmpeg not found: %w", err)
	}
	if ffprobe == "none" {
		ffprobe = ""
	}
	if ffprobe, err = exec.LookPath(orDefault(ffprobe, "ffprobe")); err != nil {
		return nil, fmt.Errorf("inject: ffprobe not found: %w", err)
	}
	aac, err := resolveAACEncoder(ctx, ffmpeg, cfg.Audio.Encoder)
	if err != nil {
		return nil, fmt.Errorf("inject: %w", err)
	}
	for _, d := range []string{cfg.Path, cfg.FailedPath} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("inject: %w", err)
		}
	}
	return &Injector{
		cfg: cfg, library: libraryPath, ffmpeg: ffmpeg, ffprobe: ffprobe, aac: aac,
		log: log.With("component", "inject"), now: time.Now, observed: map[string]observation{},
	}, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// AACEncoder is the AAC encoder in use.
func (in *Injector) AACEncoder() string { return in.aac }

// Run checks the inject folder every scan interval until ctx is done,
// calling onInjected after any pass that added files to the library.
func (in *Injector) Run(ctx context.Context, onInjected func()) {
	t := time.NewTicker(in.cfg.ScanInterval.D())
	defer t.Stop()
	for {
		if n := in.Pass(ctx, false); n > 0 && onInjected != nil {
			onInjected()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type pending struct {
	abs, rel string
	fi       os.FileInfo
}

// Pass processes every settled file once and returns how many were added to
// the library. With force, files are processed without waiting to settle.
func (in *Injector) Pass(ctx context.Context, force bool) int {
	now := in.now()
	files, err := in.list()
	if err != nil {
		in.log.Error("scan inject folder", "err", err)
		return 0
	}

	ready := in.settled(files, now, force)
	slices.SortFunc(ready, func(a, b pending) int { return a.fi.ModTime().Compare(b.fi.ModTime()) })
	added := 0
	for _, f := range ready {
		if ctx.Err() != nil {
			break
		}
		delete(in.observed, f.abs)
		if in.process(ctx, f) {
			added++
		}
	}
	return added
}

// settled updates the per-file observations and returns the files that are
// ready: size and mtime unchanged since an earlier scan for at least
// settle_time, and mtime itself at least settle_time old (which also covers
// the first scan after a restart). Files are never processed on the scan that
// first sees them, so settle_time 0 still waits one scan interval.
func (in *Injector) settled(files []pending, now time.Time, force bool) []pending {
	settle := in.cfg.SettleTime.D()
	var ready []pending
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		seen[f.abs] = true
		prev, ok := in.observed[f.abs]
		if !ok || prev.size != f.fi.Size() || !prev.mtime.Equal(f.fi.ModTime()) {
			in.observed[f.abs] = observation{size: f.fi.Size(), mtime: f.fi.ModTime(), since: now}
			ok = false
			prev = in.observed[f.abs]
		}
		if force || (ok && now.Sub(prev.since) >= settle && now.Sub(f.fi.ModTime()) >= settle) {
			ready = append(ready, f)
		} else {
			in.log.Debug("waiting for file to settle", "file", f.rel, "size", f.fi.Size())
		}
	}
	for p := range in.observed {
		if !seen[p] {
			delete(in.observed, p)
		}
	}
	return ready
}

// list returns candidate files in the inject folder, skipping hidden files,
// the done/failed folders and unknown extensions.
func (in *Injector) list() ([]pending, error) {
	exts := make(map[string]bool, len(in.cfg.Extensions))
	for _, e := range in.cfg.Extensions {
		exts[e] = true
	}
	skipDirs := map[string]bool{in.cfg.Originals.Path: true, in.cfg.FailedPath: true}

	var out []pending
	err := filepath.WalkDir(in.cfg.Path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == in.cfg.Path {
				return err
			}
			in.log.Warn("skipping unreadable path", "path", p, "err", err)
			return nil
		}
		if p == in.cfg.Path {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || (d.IsDir() && skipDirs[p]) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if !exts[strings.ToLower(strings.TrimPrefix(filepath.Ext(p), "."))] {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(in.cfg.Path, p)
		out = append(out, pending{abs: p, rel: rel, fi: fi})
		return nil
	})
	return out, err
}

// process converts one file. It returns true if a file was added to the library.
func (in *Injector) process(ctx context.Context, f pending) bool {
	log := in.log.With("file", f.rel)
	start := in.now()

	p, err := probe(ctx, in.ffprobe, f.abs)
	if err != nil {
		in.fail(f, err, nil)
		return false
	}
	pl, err := buildPlan(p, in.cfg, in.aac)
	if err != nil {
		in.fail(f, err, nil)
		return false
	}

	folders, err := libraryFolders(in.library)
	if err != nil {
		log.Error("list library folders", "err", err)
		return false // leave the file for the next pass
	}
	h := hints{}
	if dir := filepath.Dir(filepath.ToSlash(f.rel)); dir != "." {
		h.injectDirs = strings.Split(dir, "/")
	}
	for _, t := range in.cfg.Routing.Tags {
		h.tags = append(h.tags, p.tag(t))
	}
	stem := strings.TrimSuffix(filepath.Base(f.abs), filepath.Ext(f.abs))
	if in.cfg.Routing.Filename {
		h.filename = stem
	}
	destRel, reason := route(folders, h)
	destDir := filepath.Join(in.library, filepath.FromSlash(destRel))

	// Write to a hidden temp name with a non-media extension so the library
	// scanner can never pick up a partial file, then rename into place.
	tmp := filepath.Join(destDir, "."+stem+pl.Ext+".podcaptain-partial")
	args := []string{"-hide_banner", "-nostdin", "-y", "-v", "error", "-i", f.abs}
	if in.cfg.Threads > 0 {
		args = append(args, "-threads", strconv.Itoa(in.cfg.Threads))
	}
	args = append(args, pl.Args...)
	args = append(args, "-f", pl.Format, tmp)

	log.Info("processing", "plan", pl.Summary, "destination", orDefault(destRel, "(top level)"), "routing", reason)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, in.ffmpeg, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		if ctx.Err() != nil {
			log.Info("interrupted; will retry on next start")
			return false
		}
		in.fail(f, fmt.Errorf("ffmpeg: %w", err), append([]string{in.ffmpeg}, args...), stderr.String())
		return false
	}
	// Keep the source's mtime: it is Pod Captain's last-resort episode date.
	_ = os.Chtimes(tmp, f.fi.ModTime(), f.fi.ModTime())

	dest, err := claimPath(filepath.Join(destDir, stem+pl.Ext))
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		os.Remove(tmp)
		log.Error("move output into library", "err", err)
		return false // source untouched; retried next pass
	}

	if err := in.disposeOriginal(f); err != nil {
		// The episode is published; leaving the source would re-inject it,
		// so say loudly that it needs attention.
		log.Error("output published but original could not be moved/deleted; remove it manually to avoid a duplicate",
			"err", err)
	}
	relDest, _ := filepath.Rel(in.library, dest)
	log.Info("injected", "output", relDest, "took", in.now().Sub(start).Round(time.Millisecond))
	return true
}

func (in *Injector) disposeOriginal(f pending) error {
	if in.cfg.Originals.Action == "delete" {
		return os.Remove(f.abs)
	}
	return moveFile(f.abs, filepath.Join(in.cfg.Originals.Path, f.rel))
}

// fail moves a file to the failed folder with a .log file explaining why,
// so it is not retried on every pass.
func (in *Injector) fail(f pending, cause error, cmd []string, stderr ...string) {
	in.log.Error("failed; moved to failed folder", "file", f.rel, "err", cause, "failed_path", in.cfg.FailedPath)
	dest := filepath.Join(in.cfg.FailedPath, f.rel)
	if err := moveFile(f.abs, dest); err != nil {
		in.log.Error("could not move failed file; it will be retried", "file", f.rel, "err", err)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "time: %s\nsource: %s\nerror: %v\n", in.now().Format(time.RFC3339), f.abs, cause)
	if len(cmd) > 0 {
		fmt.Fprintf(&b, "command: %s\n", strings.Join(cmd, " "))
	}
	for _, s := range stderr {
		if s = strings.TrimSpace(s); s != "" {
			if len(s) > 8192 {
				s = "…" + s[len(s)-8192:]
			}
			fmt.Fprintf(&b, "\nffmpeg output:\n%s\n", s)
		}
	}
	_ = os.WriteFile(dest+".log", []byte(b.String()), 0o644)
}

// claimPath returns path, or "name (2).ext", "name (3).ext"... if taken.
func claimPath(path string) (string, error) {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; i < 1000; i++ {
		p := path
		if i > 1 {
			p = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			return p, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no free file name for %s", path)
}

// moveFile renames src to dst (creating parents and avoiding overwrites),
// falling back to copy+delete across filesystems.
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	dst, err := claimPath(dst)
	if err != nil {
		return err
	}
	err = os.Rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyFile(src, dst); err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}
