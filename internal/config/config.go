// Package config loads and validates Pod Captain's YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Order controls where the most recent episode appears.
type Order string

const (
	OldestFirst Order = "oldest_first" // most recent at the end (default)
	NewestFirst Order = "newest_first" // most recent at the beginning
)

// Config is the root configuration document.
type Config struct {
	Library  Library  `yaml:"library"`
	Server   Server   `yaml:"server"`
	Feed     Feed     `yaml:"feed"`
	Metadata Metadata `yaml:"metadata"`
	Inject   Inject   `yaml:"inject"`
	Log      Log      `yaml:"log"`
}

type Library struct {
	// Path is the folder to scan (recursively).
	Path string `yaml:"path"`
	// ScanInterval is how often the folder is rescanned.
	ScanInterval Duration `yaml:"scan_interval"`
	// Extensions lists the file extensions (without dot, case-insensitive) to include.
	Extensions []string `yaml:"extensions"`
	// SettleTime skips files modified more recently than this, so files that are
	// still being copied in are not published half-written.
	SettleTime Duration `yaml:"settle_time"`
	// IncludeHidden includes dotfiles and dot-directories.
	IncludeHidden bool `yaml:"include_hidden"`
}

type Server struct {
	// Listen is the address to bind, e.g. ":8080" or "127.0.0.1:8080".
	Listen string `yaml:"listen"`
	// BaseURL is the externally reachable URL of this server, used to build
	// absolute links in the feed. Apple Podcasts works reliably only with https.
	BaseURL string `yaml:"base_url"`
	// Token, if set, is a secret path prefix: all feed/media URLs become
	// /<token>/... and requests without it get 404.
	Token string `yaml:"token"`
	TLS   TLS    `yaml:"tls"`
}

type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

func (t TLS) Enabled() bool { return t.CertFile != "" || t.KeyFile != "" }

type Feed struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	Author      string `yaml:"author"`
	Language    string `yaml:"language"`
	Link        string `yaml:"link"`
	// Image is a local file path or an http(s) URL for the show artwork.
	Image    string `yaml:"image"`
	Category string `yaml:"category"`
	Explicit bool   `yaml:"explicit"`
	// Order is oldest_first (most recent at the end) or newest_first.
	Order Order `yaml:"order"`
	// Type is the itunes:type, "episodic" or "serial". Apple Podcasts ignores
	// item order in the XML and sorts by this instead, so when empty it is
	// derived from Order: oldest_first → serial, newest_first → episodic.
	Type string `yaml:"type"`
	// Block emits <itunes:block>Yes</itunes:block> so the feed is never listed
	// in the Apple Podcasts directory.
	Block *bool `yaml:"block"`
}

type Metadata struct {
	// FFprobe is the ffprobe binary. Empty means look it up in $PATH;
	// "none" disables it (the pure-Go tag reader is used instead).
	FFprobe string `yaml:"ffprobe"`
	// Workers is how many files are probed concurrently.
	Workers int `yaml:"workers"`
}

// Inject configures the file injector, which transcodes files dropped into an
// inject folder and moves the results into the library.
type Inject struct {
	Enabled bool `yaml:"enabled"`
	// Path is the inject folder. Defaults to "Inject" next to the library folder.
	Path string `yaml:"path"`
	// ScanInterval is how often the inject folder is checked.
	ScanInterval Duration `yaml:"scan_interval"`
	// SettleTime is how long a file's size and mtime must stay unchanged
	// before it is processed, so incomplete transfers are left alone.
	SettleTime Duration `yaml:"settle_time"`
	// Extensions lists the input file types to process; others are ignored.
	Extensions []string `yaml:"extensions"`
	// Originals controls what happens to a source file after success.
	Originals Originals `yaml:"originals"`
	// FailedPath receives source files that could not be processed, along
	// with a .log file explaining why. Defaults to <path>/failed.
	FailedPath string `yaml:"failed_path"`
	// FFmpeg is the ffmpeg binary; empty means look it up in $PATH.
	FFmpeg string `yaml:"ffmpeg"`
	// Threads passed to ffmpeg; 0 lets ffmpeg decide.
	Threads int `yaml:"threads"`
	// Compatible is "passthrough" (copy streams Apple Podcasts can already
	// play, no quality loss) or "transcode" (always re-encode, e.g. to
	// shrink files for metered data plans).
	Compatible string `yaml:"compatible"`
	// VideoMode is "keep" (output H.264/AAC MP4) or "audio_only" (output AAC M4A).
	VideoMode string      `yaml:"video_mode"`
	Audio     InjectAudio `yaml:"audio"`
	Video     InjectVideo `yaml:"video"`
	Routing   Routing     `yaml:"routing"`
}

type Originals struct {
	// Action is "move" or "delete".
	Action string `yaml:"action"`
	// Path is where originals are moved. Defaults to <inject path>/done.
	Path string `yaml:"path"`
}

type InjectAudio struct {
	// Encoder is an ffmpeg AAC encoder, or "auto" to pick the best available
	// (libfdk_aac, then aac_at on macOS, then ffmpeg's built-in aac).
	Encoder       string `yaml:"encoder"`
	BitrateMono   string `yaml:"bitrate_mono"`
	BitrateStereo string `yaml:"bitrate_stereo"`
	// Channels: 0 keeps the source (downmixing anything above 2 to stereo),
	// 1 forces mono, 2 forces stereo.
	Channels int `yaml:"channels"`
	// SampleRate: 0 keeps 44.1/48 kHz sources and resamples others to 48 kHz.
	SampleRate int `yaml:"sample_rate"`
	// Loudnorm normalises loudness to LoudnessTarget LUFS when re-encoding.
	Loudnorm       bool    `yaml:"loudnorm"`
	LoudnessTarget float64 `yaml:"loudness_target"`
}

type InjectVideo struct {
	Encoder   string `yaml:"encoder"`
	CRF       int    `yaml:"crf"`
	Preset    string `yaml:"preset"`
	MaxHeight int    `yaml:"max_height"`
}

type Routing struct {
	// Tags are the embedded tags compared against library subfolder names, in priority order.
	Tags []string `yaml:"tags"`
	// Filename also matches subfolder names appearing as words in the file name.
	Filename bool `yaml:"filename"`
}

type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // text or json
}

// Duration wraps time.Duration so it can be written as "5m" in YAML.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

func (d Duration) D() time.Duration { return time.Duration(d) }

var validToken = regexp.MustCompile(`^[A-Za-z0-9_-]*$`)

// DefaultExtensions are the formats Apple Podcasts can play.
var DefaultExtensions = []string{"mp3", "m4a", "m4b", "mp4", "m4v", "mov"}

// Default returns a Config populated with defaults.
func Default() Config {
	return Config{
		Library: Library{
			ScanInterval: Duration(5 * time.Minute),
			Extensions:   append([]string(nil), DefaultExtensions...),
			SettleTime:   Duration(30 * time.Second),
		},
		Server: Server{Listen: ":8080"},
		Feed: Feed{
			Title:       "Pod Captain",
			Description: "Local media published by Pod Captain",
			Language:    "en",
			Order:       OldestFirst,
		},
		Metadata: Metadata{Workers: 4},
		Inject: Inject{
			ScanInterval: Duration(30 * time.Second),
			SettleTime:   Duration(60 * time.Second),
			Extensions: []string{
				"mp3", "m4a", "m4b", "aac", "wav", "aif", "aiff", "flac", "alac", "ogg", "oga", "opus",
				"wma", "caf", "amr", "mp4", "m4v", "mov", "mkv", "webm", "avi", "wmv", "flv", "ts",
				"mpg", "mpeg", "3gp",
			},
			Originals:  Originals{Action: "move"},
			Compatible: "passthrough",
			VideoMode:  "keep",
			Audio: InjectAudio{
				Encoder:        "auto",
				BitrateMono:    "96k",
				BitrateStereo:  "160k",
				LoudnessTarget: -16,
			},
			Video: InjectVideo{Encoder: "libx264", CRF: 23, Preset: "medium", MaxHeight: 1080},
			Routing: Routing{
				Tags:     []string{"show", "album", "album_artist", "artist", "grouping"},
				Filename: true,
			},
		},
		Log: Log{Level: "info", Format: "text"},
	}
}

// SearchPaths returns the locations tried, in order, when no -config flag is given.
func SearchPaths() []string {
	var paths []string
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		paths = append(paths, filepath.Join(x, "podcaptain", "config.yaml"))
	}
	if h, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(h, ".config", "podcaptain", "config.yaml"))
	}
	return append(paths, "/usr/local/etc/podcaptain/config.yaml", "/etc/podcaptain/config.yaml")
}

// Find returns the first existing config file from SearchPaths.
func Find() (string, error) {
	for _, p := range SearchPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no config file found (tried %s)", strings.Join(SearchPaths(), ", "))
}

// Load reads, parses and validates the config file at path.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(b, filepath.Dir(path))
}

// Parse parses YAML config. Relative paths in the config are resolved against baseDir.
func Parse(b []byte, baseDir string) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.normalize(baseDir); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) normalize(baseDir string) error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Library.Path == "" {
		add("library.path is required")
	} else {
		c.Library.Path = resolvePath(c.Library.Path, baseDir)
	}
	if c.Library.ScanInterval.D() < time.Second {
		add("library.scan_interval must be at least 1s")
	}
	if c.Library.SettleTime.D() < 0 {
		add("library.settle_time must not be negative")
	}
	if len(c.Library.Extensions) == 0 {
		add("library.extensions must not be empty")
	}
	for i, e := range c.Library.Extensions {
		c.Library.Extensions[i] = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
	}

	if c.Server.Listen == "" {
		add("server.listen is required")
	}
	if c.Server.BaseURL != "" {
		u, err := url.Parse(c.Server.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("server.base_url must be an absolute http(s) URL, got %q", c.Server.BaseURL)
		}
		c.Server.BaseURL = strings.TrimRight(c.Server.BaseURL, "/")
	}
	if !validToken.MatchString(c.Server.Token) {
		add("server.token may only contain letters, digits, - and _")
	}
	if c.Server.TLS.Enabled() {
		if c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "" {
			add("server.tls requires both cert_file and key_file")
		}
		c.Server.TLS.CertFile = resolvePath(c.Server.TLS.CertFile, baseDir)
		c.Server.TLS.KeyFile = resolvePath(c.Server.TLS.KeyFile, baseDir)
	}

	switch c.Feed.Order {
	case OldestFirst, NewestFirst:
	default:
		add("feed.order must be %q or %q, got %q", OldestFirst, NewestFirst, c.Feed.Order)
	}
	switch c.Feed.Type {
	case "":
		if c.Feed.Order == NewestFirst {
			c.Feed.Type = "episodic"
		} else {
			c.Feed.Type = "serial"
		}
	case "episodic", "serial":
	default:
		add("feed.type must be \"episodic\" or \"serial\", got %q", c.Feed.Type)
	}
	if c.Feed.Block == nil {
		t := true
		c.Feed.Block = &t
	}
	if c.Feed.Image != "" && !isURL(c.Feed.Image) {
		c.Feed.Image = resolvePath(c.Feed.Image, baseDir)
	}

	if c.Metadata.Workers < 1 {
		c.Metadata.Workers = 1
	}
	if c.Metadata.FFprobe != "" && c.Metadata.FFprobe != "none" {
		c.Metadata.FFprobe = expandHome(c.Metadata.FFprobe)
	}

	if c.Inject.Enabled {
		errs = append(errs, c.normalizeInject(baseDir)...)
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level must be debug, info, warn or error")
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		add("log.format must be text or json")
	}
	return errors.Join(errs...)
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

func resolvePath(p, baseDir string) string {
	p = expandHome(os.ExpandEnv(p))
	if !filepath.IsAbs(p) && baseDir != "" {
		p = filepath.Join(baseDir, p)
	}
	return filepath.Clean(p)
}

func (c *Config) normalizeInject(baseDir string) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	in := &c.Inject

	if in.Path == "" {
		if c.Library.Path == "" {
			return nil // already reported
		}
		in.Path = filepath.Join(filepath.Dir(c.Library.Path), "Inject")
	} else {
		in.Path = resolvePath(in.Path, baseDir)
	}
	if in.Originals.Path == "" {
		in.Originals.Path = filepath.Join(in.Path, "done")
	} else {
		in.Originals.Path = resolvePath(in.Originals.Path, baseDir)
	}
	if in.FailedPath == "" {
		in.FailedPath = filepath.Join(in.Path, "failed")
	} else {
		in.FailedPath = resolvePath(in.FailedPath, baseDir)
	}
	if in.FFmpeg != "" {
		in.FFmpeg = expandHome(in.FFmpeg)
	}

	// Nothing the injector reads from or parks files in may be inside the
	// library, or originals and half-written files would show up in the feed.
	if c.Library.Path != "" {
		for name, p := range map[string]string{
			"inject.path": in.Path, "inject.originals.path": in.Originals.Path, "inject.failed_path": in.FailedPath,
		} {
			if within(p, c.Library.Path) {
				add("%s (%s) must not be inside library.path", name, p)
			}
		}
		if within(c.Library.Path, in.Path) {
			add("library.path must not be inside inject.path")
		}
	}

	if in.ScanInterval.D() < time.Second {
		add("inject.scan_interval must be at least 1s")
	}
	if in.SettleTime.D() < 0 {
		add("inject.settle_time must not be negative")
	}
	if len(in.Extensions) == 0 {
		add("inject.extensions must not be empty")
	}
	for i, e := range in.Extensions {
		in.Extensions[i] = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
	}
	switch in.Originals.Action {
	case "move", "delete":
	default:
		add("inject.originals.action must be \"move\" or \"delete\", got %q", in.Originals.Action)
	}
	switch in.Compatible {
	case "passthrough", "transcode":
	default:
		add("inject.compatible must be \"passthrough\" or \"transcode\", got %q", in.Compatible)
	}
	switch in.VideoMode {
	case "keep", "audio_only":
	default:
		add("inject.video_mode must be \"keep\" or \"audio_only\", got %q", in.VideoMode)
	}
	if in.Audio.Channels < 0 || in.Audio.Channels > 2 {
		add("inject.audio.channels must be 0, 1 or 2")
	}
	if in.Audio.SampleRate < 0 {
		add("inject.audio.sample_rate must not be negative")
	}
	if in.Audio.BitrateMono == "" || in.Audio.BitrateStereo == "" {
		add("inject.audio.bitrate_mono and bitrate_stereo are required")
	}
	if in.Audio.Encoder == "" {
		in.Audio.Encoder = "auto"
	}
	if in.Video.CRF < 0 || in.Video.CRF > 51 {
		add("inject.video.crf must be between 0 and 51")
	}
	if in.Video.MaxHeight < 0 {
		add("inject.video.max_height must not be negative")
	}
	if in.Threads < 0 {
		add("inject.threads must not be negative")
	}
	for i, t := range in.Routing.Tags {
		in.Routing.Tags[i] = strings.ToLower(strings.TrimSpace(t))
	}
	return errs
}

// within reports whether path p is dir or inside it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
