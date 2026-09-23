package metadata

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type filenameInfo struct {
	Title string
	Date  date
}

// Matches YYYY-MM-DD, YYYY_MM_DD, YYYY.MM.DD, YYYY MM DD or YYYYMMDD not
// embedded in a longer run of digits. The separator must be consistent.
var filenameDate = regexp.MustCompile(
	`(?:^|[^0-9])((?:19|20)\d{2})([-_. ]?)(0[1-9]|1[0-2])([-_. ]?)(0[1-9]|[12]\d|3[01])(?:[^0-9]|$)`)

const titleTrim = " -_.,:;|()[]{}"

// parseFilename derives a title and optional date from a file name.
func parseFilename(name string) filenameInfo {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	var fi filenameInfo

	rest := stem
	for _, m := range filenameDate.FindAllStringSubmatchIndex(stem, -1) {
		sep1, sep2 := stem[m[4]:m[5]], stem[m[8]:m[9]]
		if sep1 != sep2 {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02",
			stem[m[2]:m[3]]+"-"+stem[m[6]:m[7]]+"-"+stem[m[10]:m[11]], time.Local)
		if err != nil { // e.g. 2023-02-30
			continue
		}
		fi.Date = date{t: t, precision: precDay}
		rest = stem[:m[2]] + " " + stem[m[11]:]
		break
	}

	title := strings.NewReplacer("_", " ").Replace(rest)
	title = strings.Join(strings.Fields(title), " ")
	title = strings.Trim(title, titleTrim)
	// Tidy a separator left dangling in the middle, e.g. "Show -  - Topic".
	title = strings.ReplaceAll(title, " - - ", " - ")
	if title == "" {
		title = stem
	}
	fi.Title = title
	return fi
}
