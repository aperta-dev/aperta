package tool

import (
	"io/fs"
	"path"
	"strings"
)

var skipDirs = map[string]bool{".git": true, ".hg": true, ".svn": true, "node_modules": true}

func expandBraces(p string) []string {
	start := strings.IndexByte(p, '{')
	if start < 0 {
		return []string{p}
	}
	depth := 0
	for i := start; i < len(p); i++ {
		switch p[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				var out []string
				for _, alt := range splitTop(p[start+1 : i]) {
					out = append(out, expandBraces(p[:start]+alt+p[i+1:])...)
				}
				return out
			}
		}
	}
	return []string{p}
}

func splitTop(s string) []string {
	var parts []string
	depth, last := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[last:i])
				last = i + 1
			}
		}
	}
	return append(parts, s[last:])
}

type matcher struct {
	patterns [][]string
}

func newMatcher(pattern string) (*matcher, error) {
	m := &matcher{}
	for _, p := range expandBraces(pattern) {
		p = strings.TrimPrefix(path.Clean("/"+p), "/")
		segs := strings.Split(p, "/")
		for _, s := range segs {
			if s == "**" {
				continue
			}
			if _, err := path.Match(s, ""); err != nil {
				return nil, err
			}
		}
		m.patterns = append(m.patterns, segs)
	}
	return m, nil
}

func (m *matcher) Match(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, p := range m.patterns {
		if matchSegs(p, parts) {
			return true
		}
	}
	return false
}

func matchSegs(pat, parts []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for i := 0; i <= len(parts); i++ {
				if matchSegs(rest, parts[i:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], parts[0]); !ok {
			return false
		}
		pat, parts = pat[1:], parts[1:]
	}
	return len(parts) == 0
}

func skipDir(d fs.DirEntry, isRoot bool) bool {
	return !isRoot && skipDirs[d.Name()]
}
