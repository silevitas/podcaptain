package metadata

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseFilename(t *testing.T) {
	tests := []struct {
		name, title, date string
	}{
		{"2024-05-01 - My Episode.mp3", "My Episode", "2024-05-01"},
		{"20240501_My_Episode.m4a", "My Episode", "2024-05-01"},
		{"Show - 2023.12.31 - Topic.mp3", "Show - Topic", "2023-12-31"},
		{"Talk (2022_06_15).mp4", "Talk", "2022-06-15"},
		{"Plain Title.mp3", "Plain Title", ""},
		{"Episode 20240501123.mp3", "Episode 20240501123", ""}, // digits run too long
		{"2024-0501 mixed.mp3", "2024-0501 mixed", ""},         // inconsistent separators
		{"2023-02-30 invalid.mp3", "2023-02-30 invalid", ""},
		{"2024-05-01.mp3", "2024-05-01", "2024-05-01"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFilename(tt.name)
			if got.Title != tt.title {
				t.Errorf("title = %q, want %q", got.Title, tt.title)
			}
			gotDate := ""
			if got.Date.precision == precDay {
				gotDate = got.Date.t.Format("2006-01-02")
			}
			if gotDate != tt.date {
				t.Errorf("date = %q, want %q", gotDate, tt.date)
			}
		})
	}
}

func TestParseDate(t *testing.T) {
	tests := []struct {
		in   string
		prec precision
		want string
	}{
		{"2024-03-15T10:20:30.000000Z", precDay, "2024-03-15"},
		{"2024-03-15", precDay, "2024-03-15"},
		{"20240315", precDay, "2024-03-15"},
		{"2024-03", precMonth, "2024-03-01"},
		{"2024", precYear, "2024-01-01"},
		{"1970-01-01T00:00:00.000000Z", precNone, ""},
		{"1904-01-01T00:00:00.000000Z", precNone, ""},
		{"0000", precNone, ""},
		{"garbage", precNone, ""},
		{"", precNone, ""},
	}
	for _, tt := range tests {
		d := parseDate(tt.in)
		if d.precision != tt.prec {
			t.Errorf("%q: precision = %d, want %d", tt.in, d.precision, tt.prec)
			continue
		}
		if tt.want != "" && d.t.Format("2006-01-02") != tt.want {
			t.Errorf("%q: date = %s, want %s", tt.in, d.t.Format("2006-01-02"), tt.want)
		}
	}
}

func TestParseProbe(t *testing.T) {
	out := []byte(`{
	  "streams": [{"codec_type":"video","tags":{"creation_time":"2021-01-01T00:00:00.000000Z","title":"track title"}}],
	  "format": {"duration":"125.5","tags":{"TITLE":"Hello","ARTIST":"Host","date":"2024","synopsis":"Long","comment":"short"}}
	}`)
	e, err := parseProbe(out)
	if err != nil {
		t.Fatal(err)
	}
	if e.Title != "Hello" || e.Author != "Host" || e.Description != "Long" {
		t.Errorf("unexpected fields: %+v", e)
	}
	if e.Duration != 125500*time.Millisecond {
		t.Errorf("duration = %v", e.Duration)
	}
	// A full creation_time beats a bare year.
	if e.Date.precision != precDay || e.Date.t.Year() != 2021 {
		t.Errorf("date = %v (prec %d)", e.Date.t, e.Date.precision)
	}
}

func TestExtractFallsBackToFilenameAndMtime(t *testing.T) {
	dir := t.TempDir()
	x := NewExtractor("none", slog.New(slog.NewTextHandler(io.Discard, nil)))
	mtime := time.Date(2020, 6, 1, 12, 0, 0, 0, time.Local)

	write := func(name string) (string, os.FileInfo) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("not really audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(p)
		return p, fi
	}

	p, fi := write("2019-02-03 Dated.mp3")
	info := x.Extract(context.Background(), p, fi)
	if info.Title != "Dated" || info.DateSource != "filename" || info.Date.Format("2006-01-02") != "2019-02-03" {
		t.Errorf("filename fallback: %+v", info)
	}

	p, fi = write("Undated.mp3")
	info = x.Extract(context.Background(), p, fi)
	if info.Title != "Undated" || info.DateSource != "mtime" || !info.Date.Equal(mtime) {
		t.Errorf("mtime fallback: %+v", info)
	}
}
