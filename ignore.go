package main

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Ignore carries the patterns that keep junk out of a sync root.
type Ignore struct {
	rules []ignoreRule
}

type ignoreRule struct {
	pattern string
	negate  bool
	dirOnly bool
	// anchored means the pattern contains a slash and matches from the root.
	anchored bool
}

// defaultIgnores are always excluded: OS metadata, editor temp files and our
// own partial downloads. Users cannot accidentally sync these.
var defaultIgnores = []string{
	".DS_Store", "._*", ".Spotlight-V100", ".Trashes", ".fseventsd",
	"Thumbs.db", "desktop.ini", "$RECYCLE.BIN/", "System Volume Information/",
	".tessera-tmp-*", "*.tessera-part", "*.swp", "*.swx", "*~",
	".git/", ".svn/", ".hg/",
}

// newIgnore builds a matcher from built-ins plus the root's .tesseraignore and
// any per-root extra patterns from the sync config.
func newIgnore(localRoot string, extra []string) (*Ignore, error) {
	ig := &Ignore{}
	for _, p := range defaultIgnores {
		ig.add(p)
	}
	// A .tesseraignore at the root behaves like a .gitignore.
	if data, err := os.ReadFile(filepath.Join(localRoot, ".tesseraignore")); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		for sc.Scan() {
			ig.add(sc.Text())
		}
	}
	for _, p := range extra {
		ig.add(p)
	}
	return ig, nil
}

func (ig *Ignore) add(line string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	r := ignoreRule{}
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = strings.TrimPrefix(line, "!")
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	line = strings.TrimPrefix(line, "/")
	r.pattern = line
	r.anchored = strings.Contains(line, "/")
	ig.rules = append(ig.rules, r)
}

// Match reports whether a root-relative path should be ignored. isDir lets
// directory-only patterns behave correctly. Ignoring a directory implies
// ignoring everything beneath it.
func (ig *Ignore) Match(rel string, isDir bool) bool {
	ignored := false
	for _, r := range ig.rules {
		if r.matches(rel, isDir) {
			ignored = !r.negate
		}
	}
	return ignored
}

func (r ignoreRule) matches(rel string, isDir bool) bool {
	if r.anchored {
		// Match the whole path, or any ancestor so that "build/" also ignores
		// "build/sub/file". Equal-prefix matches require isDir for dirOnly
		// rules, which is what makes "build" a directory rule.
		if ok, _ := path.Match(r.pattern, rel); ok {
			if !r.dirOnly || isDir || len(rel) > len(r.pattern) {
				return true
			}
		}
		parts := strings.Split(rel, "/")
		for i := 1; i < len(parts); i++ {
			if ok, _ := path.Match(r.pattern, strings.Join(parts[:i], "/")); ok {
				return true
			}
		}
		return false
	}
	// Unanchored: match any single path component. A dirOnly rule matches a
	// component that has something after it, or the final component when it
	// really is a directory.
	parts := strings.Split(rel, "/")
	for i, seg := range parts {
		ok, _ := path.Match(r.pattern, seg)
		if !ok {
			continue
		}
		if !r.dirOnly || isDir || i < len(parts)-1 {
			return true
		}
	}
	return false
}
