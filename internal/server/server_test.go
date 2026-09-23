package server

import (
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/podcaptain/internal/config"
	"github.com/example/podcaptain/internal/library"
	"github.com/example/podcaptain/internal/metadata"
)

type parsedFeed struct {
	Channel struct {
		Type  string `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd type"`
		Items []struct {
			Title     string `xml:"title"`
			Episode   int    `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd episode"`
			Enclosure struct {
				URL    string `xml:"url,attr"`
				Length int64  `xml:"length,attr"`
				Type   string `xml:"type,attr"`
			} `xml:"enclosure"`
		} `xml:"item"`
	} `xml:"channel"`
}

func setup(t *testing.T, order config.Order) (*httptest.Server, config.Config) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]time.Time{
		"2021-01-01 First.mp3":       {},
		"sub/2022-06-01 Second.m4a":  {},
		"Third.mp4":                  time.Date(2023, 1, 1, 0, 0, 0, 0, time.Local),
		"ignored.txt":                {},
		".hidden/2020-01-01 Hid.mp3": {},
	}
	old := time.Now().Add(-time.Hour)
	for name, mt := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("x", 1000)), 0o644); err != nil {
			t.Fatal(err)
		}
		if mt.IsZero() {
			mt = old
		}
		_ = os.Chtimes(p, mt, mt)
	}
	// A file still being written must be skipped.
	if err := os.WriteFile(filepath.Join(dir, "2024-01-01 Copying.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Parse([]byte("library:\n  path: "+dir+"\nserver:\n  token: tok\n  base_url: https://pod.example\nfeed:\n  order: "+string(order)+"\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lib := library.New(cfg.Library, metadata.NewExtractor("none", log), 2, log)
	srv := New(cfg, lib, log, "test")
	snap, err := lib.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	srv.Update(snap)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, cfg
}

func getFeed(t *testing.T, url string) parsedFeed {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("feed status %d", resp.StatusCode)
	}
	var f parsedFeed
	if err := xml.NewDecoder(resp.Body).Decode(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

func titles(f parsedFeed) string {
	var s []string
	for _, it := range f.Channel.Items {
		s = append(s, it.Title)
	}
	return strings.Join(s, ",")
}

func TestFeedOrder(t *testing.T) {
	ts, _ := setup(t, config.OldestFirst)
	f := getFeed(t, ts.URL+"/tok/feed.xml")
	if got := titles(f); got != "First,Second,Third" {
		t.Errorf("oldest_first order = %s", got)
	}
	if f.Channel.Type != "serial" || f.Channel.Items[0].Episode != 1 || f.Channel.Items[2].Episode != 3 {
		t.Errorf("serial numbering wrong: type=%s items=%+v", f.Channel.Type, f.Channel.Items)
	}
	it := f.Channel.Items[1]
	if it.Enclosure.Length != 1000 || it.Enclosure.Type != "audio/x-m4a" ||
		!strings.HasPrefix(it.Enclosure.URL, "https://pod.example/tok/media/") {
		t.Errorf("enclosure = %+v", it.Enclosure)
	}

	ts, _ = setup(t, config.NewestFirst)
	f = getFeed(t, ts.URL+"/tok/feed.xml")
	if got := titles(f); got != "Third,Second,First" {
		t.Errorf("newest_first order = %s", got)
	}
	if f.Channel.Type != "episodic" || f.Channel.Items[0].Episode != 0 {
		t.Errorf("episodic should not number episodes: %+v", f.Channel)
	}
}

func TestMediaServing(t *testing.T) {
	ts, _ := setup(t, config.OldestFirst)
	f := getFeed(t, ts.URL+"/tok/feed.xml")
	path := strings.TrimPrefix(f.Channel.Items[0].Enclosure.URL, "https://pod.example")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("Range", "bytes=10-19")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || len(body) != 10 ||
		resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Errorf("range: status=%d len=%d type=%s", resp.StatusCode, len(body), resp.Header.Get("Content-Type"))
	}

	resp, err = http.Head(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.ContentLength != 1000 {
		t.Errorf("head: status=%d len=%d", resp.StatusCode, resp.ContentLength)
	}

	for _, p := range []string{"/feed.xml", "/wrong/feed.xml", "/tok/media/nope/x.mp3"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("%s: status %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestASCIIName(t *testing.T) {
	tests := map[string]string{
		"2023-11-02 - Ünïcode Tïtle.m4a": "2023-11-02-n-code-T-tle.m4a",
		"plain.mp3":                      "plain.mp3",
		"日本語.mp3":                        "media.mp3",
		".mp3":                           "media.mp3",
	}
	for in, want := range tests {
		if got := asciiName(in); got != want {
			t.Errorf("asciiName(%q) = %q, want %q", in, got, want)
		}
	}
}
