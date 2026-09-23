package inject

import (
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// folder is a candidate destination directory inside the library.
type folder struct {
	rel   string   // relative to the library root, slash-separated
	words []string // normalized words of the folder's base name
}

// words splits s into lowercase alphanumeric words.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func key(ws []string) string { return strings.Join(ws, " ") }

// libraryFolders lists every non-hidden subdirectory of the library.
func libraryFolders(root string) ([]folder, error) {
	var out []folder
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if !d.IsDir() || p == root {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		if ws := words(d.Name()); len(ws) > 0 {
			out = append(out, folder{rel: filepath.ToSlash(rel), words: ws})
		}
		return nil
	})
	return out, err
}

// hints are the clues a file offers about where it belongs, strongest first.
type hints struct {
	// injectDirs are the subfolders the file sat in within the inject folder,
	// outermost first (e.g. inject/My Show/Season 2/ep.mp3 → [My Show, Season 2]).
	injectDirs []string
	// tags are embedded tag values in configured priority order.
	tags []string
	// filename is the file name without extension, or "" to skip filename matching.
	filename string
}

// route picks a library subfolder for a file, returning "" for the top level.
// The reason explains the choice, for logging.
func route(folders []folder, h hints) (rel, reason string) {
	byName := map[string][]folder{}
	for _, f := range folders {
		k := key(f.words)
		byName[k] = append(byName[k], f)
	}
	// unique returns the single folder with the given normalized name.
	unique := func(name string) (folder, bool) {
		fs := byName[key(words(name))]
		if len(fs) == 1 {
			return fs[0], true
		}
		return folder{}, false
	}

	// 1. The inject subfolder path, matched as a full relative path first
	//    (inject/A/B → library/A/B), then by name from the innermost folder out.
	if len(h.injectDirs) > 0 {
		var want []string
		for _, d := range h.injectDirs {
			want = append(want, key(words(d)))
		}
		for _, f := range folders {
			parts := strings.Split(f.rel, "/")
			if len(parts) != len(want) {
				continue
			}
			match := true
			for i, p := range parts {
				if key(words(p)) != want[i] {
					match = false
					break
				}
			}
			if match {
				return f.rel, "inject subfolder " + strings.Join(h.injectDirs, "/")
			}
		}
		for i := len(h.injectDirs) - 1; i >= 0; i-- {
			if f, ok := unique(h.injectDirs[i]); ok {
				return f.rel, "inject subfolder " + h.injectDirs[i]
			}
		}
	}

	// 2. Embedded tags equal to a folder name.
	for _, t := range h.tags {
		if t == "" {
			continue
		}
		if f, ok := unique(t); ok {
			return f.rel, "tag " + t
		}
	}

	// 3. A folder name appearing as a run of whole words in the file name.
	//    The longest match wins; a tie between different folders is ambiguous.
	if h.filename != "" {
		fw := words(h.filename)
		var best []folder
		bestLen := 0
		for _, f := range folders {
			if len(key(f.words)) < 3 || !containsRun(fw, f.words) {
				continue // skip very short names like "tv" to avoid false hits
			}
			n := len(key(f.words))
			switch {
			case n > bestLen:
				best, bestLen = []folder{f}, n
			case n == bestLen:
				best = append(best, f)
			}
		}
		if len(best) == 1 {
			return best[0].rel, "filename contains " + best[0].rel
		}
	}
	return "", "no matching subfolder"
}

func containsRun(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}
