package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const probeTimeout = 30 * time.Second

func resolveFFprobe(p string, log *slog.Logger) string {
	switch p {
	case "none":
		return ""
	case "":
		found, err := exec.LookPath("ffprobe")
		if err != nil {
			log.Warn("ffprobe not found in PATH; falling back to the built-in tag reader (no durations, fewer formats). Install ffmpeg for best results.")
			return ""
		}
		return found
	default:
		found, err := exec.LookPath(p)
		if err != nil {
			log.Warn("configured ffprobe not usable; falling back to the built-in tag reader", "ffprobe", p, "err", err)
			return ""
		}
		return found
	}
}

type ffprobeOutput struct {
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		CodecType string            `json:"codec_type"`
		Tags      map[string]string `json:"tags"`
	} `json:"streams"`
}

func probe(ctx context.Context, bin, path string) (embedded, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", "--", path)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return embedded{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return embedded{}, err
	}
	return parseProbe(out)
}

func parseProbe(out []byte) (embedded, error) {
	var p ffprobeOutput
	if err := json.Unmarshal(out, &p); err != nil {
		return embedded{}, fmt.Errorf("decode ffprobe output: %w", err)
	}
	tags := lowerKeys(p.Format.Tags)
	// Stream-level tags (e.g. creation_time on video tracks) fill gaps.
	for _, s := range p.Streams {
		for k, v := range lowerKeys(s.Tags) {
			if _, ok := tags[k]; !ok && k != "title" && k != "handler_name" {
				tags[k] = v
			}
		}
	}

	e := embedded{
		Title:       tags["title"],
		Description: first(tags, "synopsis", "description", "comment"),
		Author:      first(tags, "artist", "album_artist", "author", "composer"),
		Album:       tags["album"],
	}
	// Prefer an explicit release/recording date over the container's creation_time,
	// which is often just when the file was encoded.
	for _, k := range []string{"date", "release_date", "originaldate", "tdrl", "tdrc", "creation_time", "year"} {
		if d := parseDate(tags[k]); d.precision > e.Date.precision {
			e.Date = d
			if d.precision == precDay {
				break
			}
		}
	}
	if secs, err := strconv.ParseFloat(p.Format.Duration, 64); err == nil && secs > 0 {
		e.Duration = time.Duration(secs * float64(time.Second))
	}
	return e, nil
}

func lowerKeys(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

func first(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}
