package metadata

import (
	"strings"
	"time"
)

type precision int

const (
	precNone precision = iota
	precYear
	precMonth
	precDay
)

type date struct {
	t         time.Time
	precision precision
}

// contains reports whether t falls inside the period d describes.
func (d date) contains(t time.Time) bool {
	t = t.In(d.t.Location())
	switch d.precision {
	case precYear:
		return t.Year() == d.t.Year()
	case precMonth:
		return t.Year() == d.t.Year() && t.Month() == d.t.Month()
	case precDay:
		y1, m1, d1 := t.Date()
		y2, m2, d2 := d.t.Date()
		return y1 == y2 && m1 == m2 && d1 == d2
	}
	return false
}

var dateLayouts = []struct {
	layout string
	prec   precision
}{
	{time.RFC3339Nano, precDay},
	{"2006-01-02T15:04:05.999999999", precDay},
	{"2006-01-02T15:04:05", precDay},
	{"2006-01-02T15:04", precDay},
	{"2006-01-02 15:04:05", precDay},
	{"2006-01-02 15:04", precDay},
	{"2006-01-02", precDay},
	{"2006/01/02", precDay},
	{"2006.01.02", precDay},
	{"20060102", precDay},
	{"2006-01", precMonth},
	{"2006", precYear},
}

// parseDate parses the date formats commonly found in media tags. Values
// without a zone are interpreted in local time.
func parseDate(s string) date {
	s = strings.TrimSpace(s)
	if s == "" {
		return date{}
	}
	for _, l := range dateLayouts {
		if t, err := time.ParseInLocation(l.layout, s, time.Local); err == nil {
			if isUnsetDate(t) {
				return date{}
			}
			return date{t: t, precision: l.prec}
		}
	}
	return date{}
}

// isUnsetDate reports placeholder dates written by tools when no date is
// known: year 0, and the MP4 (1904) and Unix (1970) epochs.
func isUnsetDate(t time.Time) bool {
	if t.Year() < 1900 {
		return true
	}
	u := t.UTC()
	return u.Month() == time.January && u.Day() == 1 && u.Hour() == 0 && u.Minute() == 0 && u.Second() == 0 &&
		(u.Year() == 1904 || u.Year() == 1970)
}
