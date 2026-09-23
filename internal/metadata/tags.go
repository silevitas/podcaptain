package metadata

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/dhowden/tag"
)

// Raw tag keys that may hold a full date (ID3v2.4, ID3v2.3, MP4, Vorbis).
var rawDateKeys = []string{"TDRL", "TDRC", "TYER", "TYE", "\xa9day", "DATE", "date"}

func readTags(path string) (embedded, error) {
	f, err := os.Open(path)
	if err != nil {
		return embedded{}, err
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return embedded{}, err
	}

	e := embedded{
		Title:       m.Title(),
		Description: m.Comment(),
		Author:      m.Artist(),
		Album:       m.Album(),
	}
	if p := m.Picture(); p != nil && len(p.Data) > 0 {
		e.HasArtwork = true
		e.ArtworkExt = ".jpg"
		if p.MIMEType == "image/png" || strings.EqualFold(p.Ext, "png") {
			e.ArtworkExt = ".png"
		}
	}
	if e.Author == "" {
		e.Author = m.AlbumArtist()
	}
	raw := m.Raw()
	for _, k := range rawDateKeys {
		if s, ok := raw[k].(string); ok {
			if d := parseDate(s); d.precision > e.Date.precision {
				e.Date = d
			}
		}
	}
	if e.Date.precision == precNone && m.Year() > 0 {
		e.Date = parseDate(fmt.Sprintf("%04d", m.Year()))
	}
	return e, nil
}

// ErrNoArtwork is returned by Artwork when the file has no embedded picture.
var ErrNoArtwork = errors.New("no embedded artwork")

// Artwork returns the embedded cover image of a media file and its MIME type.
func Artwork(path string) ([]byte, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return nil, "", err
	}
	p := m.Picture()
	if p == nil || len(p.Data) == 0 {
		return nil, "", ErrNoArtwork
	}
	mime := p.MIMEType
	if mime == "" {
		mime = "image/jpeg"
	}
	return p.Data, mime, nil
}
