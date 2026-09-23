package inject

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/example/podcaptain/internal/config"
)

// stream is the subset of ffprobe stream info the planner needs.
type stream struct {
	Index      int               `json:"index"`
	CodecType  string            `json:"codec_type"`
	CodecName  string            `json:"codec_name"`
	PixFmt     string            `json:"pix_fmt"`
	Height     int               `json:"height"`
	Channels   int               `json:"channels"`
	SampleRate string            `json:"sample_rate"`
	Tags       map[string]string `json:"tags"`
	// Disposition.AttachedPic marks embedded cover art posing as a video stream.
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
		Default     int `json:"default"`
	} `json:"disposition"`
}

type probeResult struct {
	Streams []stream `json:"streams"`
	Format  struct {
		Tags map[string]string `json:"tags"`
	} `json:"format"`
}

// tag looks a tag up in the container, then in the audio stream (Ogg, Opus
// and FLAC-in-Ogg keep their tags on the stream).
func (p probeResult) tag(name string) string {
	if v := lookup(p.Format.Tags, name); v != "" {
		return v
	}
	if a, ok := p.audio(); ok {
		return lookup(a.Tags, name)
	}
	return ""
}

func lookup(m map[string]string, name string) string {
	for k, v := range m {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// metadataArgs copies tags to the output: container tags normally, or the
// audio stream's tags when the container has none (e.g. Ogg/Opus).
func (p probeResult) metadataArgs(a stream, hasAudio bool) []string {
	if hasAudio && lookup(p.Format.Tags, "title") == "" && lookup(a.Tags, "title") != "" {
		return []string{"-map_metadata", "0:s:" + strconv.Itoa(a.Index)}
	}
	return []string{"-map_metadata", "0"}
}

// firstStream returns the default stream matching pred, else the first match.
func (p probeResult) firstStream(pred func(stream) bool) (stream, bool) {
	var found *stream
	for i := range p.Streams {
		s := &p.Streams[i]
		if !pred(*s) {
			continue
		}
		if s.Disposition.Default == 1 {
			return *s, true
		}
		if found == nil {
			found = s
		}
	}
	if found == nil {
		return stream{}, false
	}
	return *found, true
}

func (p probeResult) audio() (stream, bool) {
	return p.firstStream(func(s stream) bool { return s.CodecType == "audio" })
}

func (p probeResult) video() (stream, bool) {
	return p.firstStream(func(s stream) bool { return s.CodecType == "video" && s.Disposition.AttachedPic == 0 })
}

func (p probeResult) cover() (stream, bool) {
	// Only formats the MP4 and MP3 muxers accept as cover art.
	return p.firstStream(func(s stream) bool {
		return s.CodecType == "video" && s.Disposition.AttachedPic == 1 && (s.CodecName == "mjpeg" || s.CodecName == "png")
	})
}

func probe(ctx context.Context, ffprobe, path string) (probeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobe,
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", "--", path).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return probeResult{}, fmt.Errorf("ffprobe: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return probeResult{}, fmt.Errorf("ffprobe: %w", err)
	}
	var p probeResult
	if err := json.Unmarshal(out, &p); err != nil {
		return probeResult{}, fmt.Errorf("decode ffprobe output: %w", err)
	}
	return p, nil
}

// plan describes one ffmpeg invocation.
type plan struct {
	Ext    string   // output extension including dot
	Format string   // ffmpeg muxer (-f)
	Args   []string // output options, placed between the input and output paths
	// Summary is a human-readable description for logs.
	Summary string
}

// encoders are the AAC encoders available in ffmpeg, best first.
var aacPreference = []string{"libfdk_aac", "aac_at", "aac"}

// buildPlan decides how to convert a probed file. aacEncoder is the resolved
// encoder name (never "auto").
func buildPlan(p probeResult, cfg config.Inject, aacEncoder string) (plan, error) {
	a, hasAudio := p.audio()
	v, hasVideo := p.video()
	passthrough := cfg.Compatible == "passthrough"

	if hasVideo && cfg.VideoMode == "keep" {
		pl := videoPlan(v, a, hasAudio, cfg, aacEncoder, passthrough)
		pl.Args = append(pl.Args, p.metadataArgs(a, hasAudio)...)
		return pl, nil
	}
	if !hasAudio {
		return plan{}, errors.New("no audio stream")
	}

	pl := plan{}
	args := []string{"-map", "0:" + strconv.Itoa(a.Index)}
	cover, hasCover := p.cover()
	if hasCover {
		args = append(args, "-map", "0:"+strconv.Itoa(cover.Index), "-c:v", "copy", "-disposition:v:0", "attached_pic")
	}

	switch {
	case passthrough && a.CodecName == "mp3" && !needsAudioChange(a, cfg):
		pl.Ext, pl.Format = ".mp3", "mp3"
		args = append(args, "-c:a", "copy", "-id3v2_version", "3")
		pl.Summary = "copy mp3 audio"
	case passthrough && a.CodecName == "aac" && !needsAudioChange(a, cfg):
		pl.Ext, pl.Format = ".m4a", "ipod"
		args = append(args, "-c:a", "copy", "-movflags", "+faststart")
		pl.Summary = "copy aac audio into m4a"
	default:
		pl.Ext, pl.Format = ".m4a", "ipod"
		enc, desc := audioEncodeArgs(a, cfg, aacEncoder)
		args = append(args, enc...)
		args = append(args, "-movflags", "+faststart")
		pl.Summary = "encode audio " + desc
	}
	if hasVideo {
		pl.Summary += " (video dropped)"
	}
	pl.Args = append(args, p.metadataArgs(a, true)...)
	return pl, nil
}

func videoPlan(v, a stream, hasAudio bool, cfg config.Inject, aacEncoder string, passthrough bool) plan {
	args := []string{"-map", "0:" + strconv.Itoa(v.Index)}
	var parts []string

	if passthrough && v.CodecName == "h264" && (v.PixFmt == "yuv420p" || v.PixFmt == "yuvj420p") {
		args = append(args, "-c:v", "copy")
		parts = append(parts, "copy h264 video")
	} else {
		args = append(args, "-c:v", cfg.Video.Encoder, "-preset", cfg.Video.Preset,
			"-crf", strconv.Itoa(cfg.Video.CRF), "-pix_fmt", "yuv420p")
		if cfg.Video.Encoder == "libx264" {
			args = append(args, "-profile:v", "high")
		}
		desc := fmt.Sprintf("encode video %s crf %d", cfg.Video.Encoder, cfg.Video.CRF)
		if cfg.Video.MaxHeight > 0 && v.Height > cfg.Video.MaxHeight {
			args = append(args, "-vf", "scale=-2:"+strconv.Itoa(cfg.Video.MaxHeight))
			desc += fmt.Sprintf(" scaled to %dp", cfg.Video.MaxHeight)
		}
		parts = append(parts, desc)
	}

	if hasAudio {
		args = append(args, "-map", "0:"+strconv.Itoa(a.Index))
		if passthrough && a.CodecName == "aac" && !needsAudioChange(a, cfg) {
			args = append(args, "-c:a", "copy")
			parts = append(parts, "copy aac audio")
		} else {
			enc, desc := audioEncodeArgs(a, cfg, aacEncoder)
			args = append(args, enc...)
			parts = append(parts, "encode audio "+desc)
		}
	}
	args = append(args, "-movflags", "+faststart")
	return plan{Ext: ".mp4", Format: "mp4", Args: args, Summary: strings.Join(parts, ", ")}
}

// needsAudioChange reports whether the configured channel layout or sample
// rate forces a re-encode even in passthrough mode.
func needsAudioChange(a stream, cfg config.Inject) bool {
	if cfg.Audio.Channels > 0 && a.Channels != cfg.Audio.Channels {
		return true
	}
	if a.Channels > 2 {
		return true
	}
	sr, _ := strconv.Atoi(a.SampleRate)
	if cfg.Audio.SampleRate > 0 {
		return sr != cfg.Audio.SampleRate
	}
	return sr != 0 && sr != 44100 && sr != 48000
}

func audioEncodeArgs(a stream, cfg config.Inject, encoder string) ([]string, string) {
	ch := a.Channels
	switch {
	case cfg.Audio.Channels > 0:
		ch = cfg.Audio.Channels
	case ch > 2 || ch <= 0:
		ch = 2
	}
	bitrate := cfg.Audio.BitrateStereo
	if ch == 1 {
		bitrate = cfg.Audio.BitrateMono
	}
	args := []string{"-c:a", encoder, "-b:a", bitrate, "-ac", strconv.Itoa(ch)}

	sr, _ := strconv.Atoi(a.SampleRate)
	switch {
	case cfg.Audio.SampleRate > 0:
		sr = cfg.Audio.SampleRate
		args = append(args, "-ar", strconv.Itoa(sr))
	case sr != 44100 && sr != 48000:
		sr = 48000
		args = append(args, "-ar", "48000")
	}
	desc := fmt.Sprintf("%s %s %dch", encoder, bitrate, ch)
	if cfg.Audio.Loudnorm {
		args = append(args, "-af", fmt.Sprintf("loudnorm=I=%g:TP=-1:LRA=11", cfg.Audio.LoudnessTarget))
		desc += fmt.Sprintf(" loudnorm %g LUFS", cfg.Audio.LoudnessTarget)
	}
	return args, desc
}

// resolveAACEncoder maps "auto" to the best AAC encoder this ffmpeg build has.
func resolveAACEncoder(ctx context.Context, ffmpeg, configured string) (string, error) {
	if configured != "auto" {
		return configured, nil
	}
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		return "", fmt.Errorf("list ffmpeg encoders: %w", err)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			have[f[1]] = true
		}
	}
	for _, e := range aacPreference {
		if have[e] {
			return e, nil
		}
	}
	return "", errors.New("ffmpeg has no AAC encoder")
}
