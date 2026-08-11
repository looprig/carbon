package input

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/looprig/tui/components"
)

// maxFileCompletions caps the @path picker so the panel never dominates the surface.
const maxFileCompletions = 8

// ActiveAtToken reports the @path partial currently being typed at the END of v, if
// any: it is the last whitespace-delimited token when that token starts with '@' and v
// does not end in whitespace (a trailing space means the token is finished). The
// returned partial is the token without its leading '@' (possibly "").
func ActiveAtToken(v string) (partial string, ok bool) {
	if v == "" {
		return "", false
	}
	switch v[len(v)-1] {
	case ' ', '\t', '\n':
		return "", false
	}
	tok := LastField(v)
	if !strings.HasPrefix(tok, "@") {
		return "", false
	}
	return tok[1:], true
}

// LastField returns the last whitespace-delimited field of v (or "").
func LastField(v string) string {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// ListFiles returns up to maxFileCompletions completion candidates for the @path
// partial: entries of the partial's directory whose name has the partial's base as a
// (case-insensitive) prefix. Dotfiles are hidden unless the prefix itself starts with
// '.'. A read error (missing/inaccessible directory) yields no candidates — the panel
// simply stays hidden. It only reads a directory listing; it opens no file.
func ListFiles(partial string) []components.FileItem {
	dir, prefix := splitPartial(partial)
	entries, err := os.ReadDir(filepath.Clean(dir))
	if err != nil {
		return nil
	}
	includeHidden := strings.HasPrefix(prefix, ".")
	lowPrefix := strings.ToLower(prefix)

	var items []components.FileItem
	for _, e := range entries {
		name := e.Name()
		if !includeHidden && strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(name), lowPrefix) {
			continue
		}
		path := name
		if dir != "." {
			path = filepath.Join(dir, name)
		}
		items = append(items, components.FileItem{Path: path, IsDir: e.IsDir()})
		if len(items) >= maxFileCompletions {
			break
		}
	}
	return items
}

// splitPartial splits an @path partial into the directory to list and the base-name
// prefix to filter by. A trailing "/" (or empty partial) lists the whole directory.
func splitPartial(partial string) (dir, prefix string) {
	switch {
	case partial == "":
		return ".", ""
	case strings.HasSuffix(partial, "/"):
		dir = strings.TrimSuffix(partial, "/")
		if dir == "" {
			return "/", "" // partial was "/" — the filesystem root
		}
		return filepath.Clean(dir), ""
	default:
		return filepath.Dir(partial), filepath.Base(partial)
	}
}
