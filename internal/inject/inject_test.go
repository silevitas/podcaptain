package inject

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/silevitas/podcaptain/internal/config"
)

func defaults(t *testing.T) config.Inject {
	t.Helper()
	cfg, err := config.Parse([]byte("library:\n  path: /tmp/lib\ninject:\n  enabled: true\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Inject
}

func audioStream(codec string, ch int, sr string) stream {
	return stream{Index: 0, CodecType: "audio", CodecName: codec, Channels: ch, SampleRate: sr}
}

func has(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestBuildPlan(t *testing.T) {
	cover := stream{Index: 1, CodecType: "video", CodecName: "mjpeg"}
	cover.Disposition.AttachedPic = 1
	h264 := stream{Index: 0, CodecType: "video", CodecName: "h264", PixFmt: "yuv420p", Height: 720}
	hevc4k := stream{Index: 0, CodecType: "video", CodecName: "hevc", PixFmt: "yuv420p10le", Height: 2160}
	aac := audioStream("aac", 2, "48000")
	aac.Index = 1

	tests := []struct {
		name    string
		streams []stream
		mutate  func(*config.Inject)
		ext     string
		want    [][]string
		notWant [][]string
	}{
		{name: "mp3 passthrough keeps cover",
			streams: []stream{audioStream("mp3", 2, "44100"), cover}, ext: ".mp3",
			want: [][]string{{"-c:a", "copy"}, {"-map", "0:1"}}},
		{name: "mp3 transcode mode",
			streams: []stream{audioStream("mp3", 2, "44100")}, ext: ".m4a",
			mutate: func(c *config.Inject) { c.Compatible = "transcode" },
			want:   [][]string{{"-c:a", "aac_at"}, {"-b:a", "160k"}}},
		{name: "aac passthrough",
			streams: []stream{audioStream("aac", 1, "44100")}, ext: ".m4a",
			want: [][]string{{"-c:a", "copy"}}},
		{name: "flac mono uses mono bitrate",
			streams: []stream{audioStream("flac", 1, "44100")}, ext: ".m4a",
			want: [][]string{{"-b:a", "96k"}, {"-ac", "1"}}, notWant: [][]string{{"-ar"}}},
		{name: "5.1 at 96k is downmixed and resampled even in passthrough",
			streams: []stream{audioStream("aac", 6, "96000")}, ext: ".m4a",
			want: [][]string{{"-ac", "2"}, {"-ar", "48000"}, {"-b:a", "160k"}}},
		{name: "forced mono for metered data",
			streams: []stream{audioStream("mp3", 2, "44100")}, ext: ".m4a",
			mutate: func(c *config.Inject) { c.Audio.Channels = 1; c.Audio.BitrateMono = "64k" },
			want:   [][]string{{"-ac", "1"}, {"-b:a", "64k"}}},
		{name: "loudnorm",
			streams: []stream{audioStream("flac", 2, "48000")}, ext: ".m4a",
			mutate: func(c *config.Inject) { c.Audio.Loudnorm = true },
			want:   [][]string{{"-af", "loudnorm=I=-16:TP=-1:LRA=11"}}},
		{name: "h264+aac video is remuxed",
			streams: []stream{h264, aac}, ext: ".mp4",
			want: [][]string{{"-c:v", "copy"}, {"-c:a", "copy"}, {"-movflags", "+faststart"}}},
		{name: "hevc 4k is transcoded and scaled",
			streams: []stream{hevc4k, aac}, ext: ".mp4",
			want: [][]string{{"-c:v", "libx264"}, {"-vf", "scale=-2:1080"}, {"-pix_fmt", "yuv420p"}}},
		{name: "audio_only drops video",
			streams: []stream{h264, aac}, ext: ".m4a",
			mutate: func(c *config.Inject) { c.VideoMode = "audio_only" },
			want:   [][]string{{"-map", "0:1"}}, notWant: [][]string{{"-map", "0:0"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaults(t)
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			pl, err := buildPlan(probeResult{Streams: tt.streams}, cfg, "aac_at")
			if err != nil {
				t.Fatal(err)
			}
			if pl.Ext != tt.ext {
				t.Errorf("ext = %s, want %s", pl.Ext, tt.ext)
			}
			for _, w := range tt.want {
				if !has(pl.Args, w...) {
					t.Errorf("args %v missing %v", pl.Args, w)
				}
			}
			for _, w := range tt.notWant {
				if has(pl.Args, w...) {
					t.Errorf("args %v should not contain %v", pl.Args, w)
				}
			}
		})
	}

	if _, err := buildPlan(probeResult{Streams: []stream{cover}}, defaults(t), "aac"); err == nil {
		t.Error("expected error for file without audio")
	}
}

func TestRoute(t *testing.T) {
	folders := []folder{
		{rel: "My Show", words: words("My Show")},
		{rel: "My Show/Season 2", words: words("Season 2")},
		{rel: "Other/Season 2", words: words("Season 2")},
		{rel: "Daily", words: words("Daily")},
		{rel: "The Daily", words: words("The Daily")},
		{rel: "TV", words: words("TV")},
	}
	tests := []struct {
		name string
		h    hints
		want string
	}{
		{"inject subfolder path", hints{injectDirs: []string{"my show", "season 2"}}, "My Show/Season 2"},
		{"inject subfolder name", hints{injectDirs: []string{"MY-SHOW"}}, "My Show"},
		{"ambiguous inject name falls through", hints{injectDirs: []string{"Season 2"}}, ""},
		{"tag", hints{tags: []string{"", "My Show"}}, "My Show"},
		{"tag priority", hints{tags: []string{"Daily", "My Show"}}, "Daily"},
		{"filename longest match", hints{filename: "The Daily 2024-05-01"}, "The Daily"},
		{"filename whole words only", hints{filename: "Dailyness report"}, ""},
		{"short names ignored", hints{filename: "TV recap"}, ""},
		{"nothing", hints{filename: "random"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := route(folders, tt.h); got != tt.want {
				t.Errorf("route = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSettled(t *testing.T) {
	dir := t.TempDir()
	cfg := defaults(t)
	cfg.Path = dir
	cfg.SettleTime = config.Duration(time.Minute)
	in := &Injector{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), observed: map[string]observation{}}

	p := filepath.Join(dir, "a.mp3")
	t0 := time.Now()
	write := func(size int, mtime time.Time) []pending {
		if err := os.WriteFile(p, []byte(strings.Repeat("x", size)), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, mtime, mtime)
		files, err := in.list()
		if err != nil {
			t.Fatal(err)
		}
		return files
	}

	old := t0.Add(-time.Hour)
	if got := in.settled(write(10, old), t0, false); len(got) != 0 {
		t.Fatal("never ready on first sighting (e.g. just after a restart)")
	}
	if got := in.settled(write(10, old), t0.Add(30*time.Second), false); len(got) != 0 {
		t.Fatal("ready before settle_time elapsed")
	}
	// Still growing: the clock restarts.
	if got := in.settled(write(20, t0.Add(40*time.Second)), t0.Add(70*time.Second), false); len(got) != 0 {
		t.Fatal("ready while file is changing")
	}
	if got := in.settled(write(20, t0.Add(40*time.Second)), t0.Add(100*time.Second), false); len(got) != 0 {
		t.Fatal("ready before a minute of stability")
	}
	if got := in.settled(write(20, t0.Add(40*time.Second)), t0.Add(131*time.Second), false); len(got) != 1 {
		t.Fatal("not ready after settling")
	}
	if got := in.settled(write(20, time.Now()), time.Now(), true); len(got) != 1 {
		t.Fatal("force should skip settling")
	}
}

// TestEndToEnd runs real ffmpeg when available.
func TestEndToEnd(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	root := t.TempDir()
	lib := filepath.Join(root, "Library")
	injectDir := filepath.Join(root, "Inject")
	for _, d := range []string{filepath.Join(lib, "My Show"), injectDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(injectDir, "episode.wav")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=d=1",
		"-metadata", "album=My Show", src).CombinedOutput(); err != nil {
		t.Fatalf("make fixture: %v %s", err, out)
	}
	mtime := time.Date(2024, 5, 1, 9, 0, 0, 0, time.Local)
	_ = os.Chtimes(src, mtime, mtime)
	if err := os.WriteFile(filepath.Join(injectDir, "junk.mp3"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Parse([]byte("library:\n  path: "+lib+"\ninject:\n  enabled: true\n  audio:\n    encoder: aac\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	in, err := New(context.Background(), cfg.Inject, lib, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if n := in.Pass(context.Background(), true); n != 1 {
		t.Fatalf("injected %d, want 1", n)
	}

	out := filepath.Join(lib, "My Show", "episode.m4a")
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("output missing: %v", err)
	}
	if !fi.ModTime().Equal(mtime) {
		t.Errorf("output mtime = %v, want source mtime %v", fi.ModTime(), mtime)
	}
	for _, p := range []string{
		filepath.Join(injectDir, "done", "episode.wav"),
		filepath.Join(injectDir, "failed", "junk.mp3"),
		filepath.Join(injectDir, "failed", "junk.mp3.log"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(lib, "My Show"))
	if len(entries) != 1 {
		t.Errorf("stray files in library: %v", entries)
	}
}
