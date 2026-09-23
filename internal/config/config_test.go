package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte("library:\n  path: media\n"), "/etc/podcaptain")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Library.Path != "/etc/podcaptain/media" {
		t.Errorf("relative path not resolved: %s", cfg.Library.Path)
	}
	if cfg.Library.ScanInterval.D() != 5*time.Minute {
		t.Errorf("scan_interval = %v", cfg.Library.ScanInterval.D())
	}
	if cfg.Feed.Order != OldestFirst || cfg.Feed.Type != "serial" {
		t.Errorf("order/type = %s/%s", cfg.Feed.Order, cfg.Feed.Type)
	}
	if cfg.Feed.Block == nil || !*cfg.Feed.Block {
		t.Error("block should default to true")
	}
}

func TestParseOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
library:
  path: /media
  scan_interval: 30s
  extensions: [".MP3", m4a]
server:
  base_url: https://example.com/
feed:
  order: newest_first
`), "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Library.ScanInterval.D() != 30*time.Second {
		t.Errorf("scan_interval = %v", cfg.Library.ScanInterval.D())
	}
	if strings.Join(cfg.Library.Extensions, ",") != "mp3,m4a" {
		t.Errorf("extensions = %v", cfg.Library.Extensions)
	}
	if cfg.Server.BaseURL != "https://example.com" {
		t.Errorf("base_url = %s", cfg.Server.BaseURL)
	}
	if cfg.Feed.Type != "episodic" {
		t.Errorf("type = %s, want episodic for newest_first", cfg.Feed.Type)
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]string{
		"missing path":  "server:\n  listen: :80\n",
		"unknown field": "library:\n  path: /x\n  bogus: 1\n",
		"bad duration":  "library:\n  path: /x\n  scan_interval: soon\n",
		"bad order":     "library:\n  path: /x\nfeed:\n  order: random\n",
		"bad base_url":  "library:\n  path: /x\nserver:\n  base_url: example.com\n",
		"half tls":      "library:\n  path: /x\nserver:\n  tls:\n    cert_file: a.pem\n",
		"bad token":     "library:\n  path: /x\nserver:\n  token: a/b\n",
		"token w/ dot":  "library:\n  path: /x\nserver:\n  token: a.b\n",
	}
	for name, y := range tests {
		if _, err := Parse([]byte(y), ""); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestInjectConfig(t *testing.T) {
	cfg, err := Parse([]byte("library:\n  path: /media/Podcasts\ninject:\n  enabled: true\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.Inject
	if in.Path != "/media/Inject" || in.Originals.Path != "/media/Inject/done" || in.FailedPath != "/media/Inject/failed" {
		t.Errorf("default paths: %s %s %s", in.Path, in.Originals.Path, in.FailedPath)
	}
	if in.Originals.Action != "move" || in.Compatible != "passthrough" || in.VideoMode != "keep" {
		t.Errorf("default modes: %+v", in)
	}

	bad := map[string]string{
		"inject inside library":    "library:\n  path: /m\ninject:\n  enabled: true\n  path: /m/inject\n",
		"originals inside library": "library:\n  path: /m\ninject:\n  enabled: true\n  originals:\n    path: /m/done\n",
		"library inside inject":    "library:\n  path: /i/lib\ninject:\n  enabled: true\n  path: /i\n",
		"bad action":               "library:\n  path: /m\ninject:\n  enabled: true\n  originals:\n    action: shred\n",
		"bad video mode":           "library:\n  path: /m\ninject:\n  enabled: true\n  video_mode: gif\n",
		"bad channels":             "library:\n  path: /m\ninject:\n  enabled: true\n  audio:\n    channels: 6\n",
	}
	for name, y := range bad {
		if _, err := Parse([]byte(y), ""); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// A sibling whose name merely starts with the library's is fine.
	if _, err := Parse([]byte("library:\n  path: /m/pod\ninject:\n  enabled: true\n  path: /m/podinject\n"), ""); err != nil {
		t.Errorf("sibling prefix rejected: %v", err)
	}
}
