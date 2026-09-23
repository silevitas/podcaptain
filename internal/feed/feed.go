// Package feed renders a podcast RSS 2.0 feed with Apple Podcasts extensions.
package feed

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"time"

	"github.com/example/podcaptain/internal/config"
	"github.com/example/podcaptain/internal/library"
)

// URLs builds absolute URLs for resources referenced by the feed.
type URLs interface {
	Feed() string
	Media(e library.Episode) string
	Artwork(e library.Episode) string
	// ShowImage returns the show artwork URL, or "" if none is configured.
	ShowImage() string
}

type rss struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Itunes  string   `xml:"xmlns:itunes,attr"`
	Atom    string   `xml:"xmlns:atom,attr"`
	Channel channel  `xml:"channel"`
}

type channel struct {
	AtomLink      atomLink    `xml:"atom:link"`
	Title         string      `xml:"title"`
	Link          string      `xml:"link"`
	Description   string      `xml:"description"`
	Language      string      `xml:"language,omitempty"`
	Generator     string      `xml:"generator"`
	LastBuildDate string      `xml:"lastBuildDate"`
	Author        string      `xml:"itunes:author,omitempty"`
	Summary       string      `xml:"itunes:summary,omitempty"`
	Type          string      `xml:"itunes:type"`
	Explicit      string      `xml:"itunes:explicit"`
	Block         string      `xml:"itunes:block,omitempty"`
	Image         *itunesHref `xml:"itunes:image,omitempty"`
	RSSImage      *rssImage   `xml:"image,omitempty"`
	Category      *itunesCat  `xml:"itunes:category,omitempty"`
	Items         []item      `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type itunesHref struct {
	Href string `xml:"href,attr"`
}

type itunesCat struct {
	Text string `xml:"text,attr"`
}

type rssImage struct {
	URL   string `xml:"url"`
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

type item struct {
	Title       string      `xml:"title"`
	Description string      `xml:"description,omitempty"`
	Summary     string      `xml:"itunes:summary,omitempty"`
	GUID        guid        `xml:"guid"`
	PubDate     string      `xml:"pubDate"`
	Enclosure   enclosure   `xml:"enclosure"`
	Duration    string      `xml:"itunes:duration,omitempty"`
	Author      string      `xml:"itunes:author,omitempty"`
	Image       *itunesHref `xml:"itunes:image,omitempty"`
	Episode     int         `xml:"itunes:episode,omitempty"`
	EpisodeType string      `xml:"itunes:episodeType"`
}

type guid struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

// Render produces the feed XML for a snapshot.
func Render(cfg config.Feed, snap *library.Snapshot, u URLs, version string) ([]byte, error) {
	link := cfg.Link
	if link == "" {
		link = u.Feed()
	}
	ch := channel{
		AtomLink:    atomLink{Href: u.Feed(), Rel: "self", Type: "application/rss+xml"},
		Title:       cfg.Title,
		Link:        link,
		Description: cfg.Description,
		Language:    cfg.Language,
		Generator:   "Pod Captain " + version,
		Author:      cfg.Author,
		Summary:     cfg.Description,
		Type:        cfg.Type,
		Explicit:    strconv.FormatBool(cfg.Explicit),
	}
	if cfg.Block != nil && *cfg.Block {
		ch.Block = "Yes"
	}
	if cfg.Category != "" {
		ch.Category = &itunesCat{Text: cfg.Category}
	}
	if img := u.ShowImage(); img != "" {
		ch.Image = &itunesHref{Href: img}
		ch.RSSImage = &rssImage{URL: img, Title: cfg.Title, Link: link}
	}

	var episodes []library.Episode
	var built time.Time
	if snap != nil {
		episodes = snap.Episodes
		built = snap.ChangedAt
	}
	if built.IsZero() {
		built = time.Now()
	}
	ch.LastBuildDate = built.Format(time.RFC1123Z)

	// Episodes are chronological (oldest first). Number them in that order so
	// serial shows sort correctly in Apple Podcasts, which ignores XML order.
	items := make([]item, len(episodes))
	for i, e := range episodes {
		it := item{
			Title:       e.Info.Title,
			Description: e.Info.Description,
			Summary:     e.Info.Description,
			GUID:        guid{IsPermaLink: "false", Value: "podcaptain:" + e.ID},
			PubDate:     e.Info.Date.Format(time.RFC1123Z),
			Enclosure:   enclosure{URL: u.Media(e), Length: e.Size, Type: e.MIME},
			Author:      e.Info.Author,
			EpisodeType: "full",
		}
		if cfg.Type == "serial" {
			it.Episode = i + 1
		}
		if e.Info.Duration > 0 {
			it.Duration = strconv.Itoa(int(e.Info.Duration.Round(time.Second).Seconds()))
		}
		if e.Info.HasArtwork {
			it.Image = &itunesHref{Href: u.Artwork(e)}
		}
		items[i] = it
	}
	if cfg.Order == config.NewestFirst {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	ch.Items = items

	doc := rss{
		Version: "2.0",
		Itunes:  "http://www.itunes.com/dtds/podcast-1.0.dtd",
		Atom:    "http://www.w3.org/2005/Atom",
		Channel: ch,
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode feed: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}
