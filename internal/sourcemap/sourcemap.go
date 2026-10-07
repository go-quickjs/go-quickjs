// Package sourcemap reads source maps (version 3, ECMA-426) as Node does
// for --enable-source-maps: the positions in a compiled script mapped to
// the source it was compiled from, and the names it had there.
package sourcemap

import (
	"encoding/base64"
	"errors"
	"net/url"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Map is a decoded source map.
type Map struct {
	entries []entry
}

// entry is one mapping: a place in the compiled script, and, but for a
// mapping of a place to nothing, where in which source it came from and the
// name it had there.
type entry struct {
	line, col       int
	source          string
	srcLine, srcCol int
	name            string
	mapped          bool
}

// Entry is what a position in the compiled script maps to.
type Entry struct {
	// Source is the original source's URL, made absolute.
	Source string
	// Line and Column are counted from 0.
	Line, Column int
	// Name is the name the mapping gives the place, or empty.
	Name string
}

// payload is a source map's fields.
type payload struct {
	Version    int
	Sources    []*string
	SourceRoot string
	Names      []string
	Mappings   string
	// Sections are an index map's, nil for any other.
	Sections []section
}

type section struct {
	Offset struct{ Line, Column int }
	Map    *payload
}

// Parse decodes a source map whose own URL is base, against which -- after
// its sourceRoot -- the sources it names are made absolute, as Node makes
// them: a source that is an absolute path becomes a file URL.
func Parse(data []byte, base string) (*Map, error) {
	v, err := parseJSON(data)
	if err != nil {
		return nil, err
	}
	pp, err := payloadOf(v)
	if err != nil {
		return nil, err
	}
	p := *pp
	m := &Map{}
	if p.Sections != nil {
		for _, s := range p.Sections {
			if s.Map == nil {
				continue
			}
			if err := m.parse(s.Map, base, s.Offset.Line, s.Offset.Column); err != nil {
				return nil, err
			}
		}
	} else if err := m.parse(&p, base, 0, 0); err != nil {
		return nil, err
	}
	sort.SliceStable(m.entries, func(i, j int) bool {
		a, b := m.entries[i], m.entries[j]
		return a.line < b.line || a.line == b.line && a.col < b.col
	})
	return m, nil
}

// parse decodes one map's mappings, whose lines start at line and the first
// of them at col.
func (m *Map) parse(p *payload, base string, line, col int) error {
	sources := make([]string, len(p.Sources))
	for i, s := range p.Sources {
		if s != nil {
			sources[i] = absolute(p.SourceRoot+*s, base)
		}
	}
	src, srcLine, srcCol, name := 0, 0, 0, 0
	r := strings.NewReader(p.Mappings)
	for r.Len() > 0 {
		c, _ := r.ReadByte()
		switch c {
		case ';':
			line++
			col = 0
			continue
		case ',':
			continue
		}
		r.UnreadByte()
		var fields [5]int
		n := 0
		for n < 5 && r.Len() > 0 {
			if c, _ := r.ReadByte(); c == ',' || c == ';' {
				r.UnreadByte()
				break
			}
			r.UnreadByte()
			v, err := vlq(r)
			if err != nil {
				return err
			}
			fields[n] = v
			n++
		}
		col += fields[0]
		if n < 4 {
			m.entries = append(m.entries, entry{line: line, col: col})
			continue
		}
		src += fields[1]
		srcLine += fields[2]
		srcCol += fields[3]
		e := entry{line: line, col: col, srcLine: srcLine, srcCol: srcCol, mapped: true}
		if src >= 0 && src < len(sources) {
			e.source = sources[src]
		}
		if n == 5 {
			name += fields[4]
			if name >= 0 && name < len(p.Names) {
				e.name = p.Names[name]
			}
		}
		m.entries = append(m.entries, e)
	}
	return nil
}

const base64Digits = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// vlq reads one base64 VLQ value.
func vlq(r *strings.Reader) (int, error) {
	result, shift := 0, 0
	for {
		c, err := r.ReadByte()
		if err != nil {
			return 0, errors.New("sourcemap: a value ends early")
		}
		d := strings.IndexByte(base64Digits, c)
		if d < 0 {
			return 0, errors.New("sourcemap: a mapping holds a character that is no base64 digit")
		}
		if shift > 60 {
			return 0, errors.New("sourcemap: a value is too large")
		}
		result += (d & 31) << shift
		shift += 5
		if d&32 == 0 {
			break
		}
	}
	if result&1 != 0 {
		return -(result >> 1), nil
	}
	return result >> 1, nil
}

// Find is the mapping a position in the compiled script, its line and
// column counted from 0, falls in: the last one at or before it, as Node's
// findEntry has it -- false where there is none, or it maps the place to
// nothing.
func (m *Map) Find(line, col int) (Entry, bool) {
	es := m.entries
	i := sort.Search(len(es), func(i int) bool {
		return es[i].line > line || es[i].line == line && es[i].col > col
	})
	if i == 0 {
		return Entry{}, false
	}
	e := es[i-1]
	if !e.mapped || e.source == "" {
		return Entry{}, false
	}
	return Entry{Source: e.source, Line: e.srcLine, Column: e.srcCol, Name: e.name}, true
}

// absolute is a source's URL, made absolute against base.
func absolute(source, base string) string {
	if filepath.IsAbs(source) || isWindowsPath(source) {
		return FileURL(source)
	}
	b, err := url.Parse(base)
	if err != nil {
		return source
	}
	s, err := url.Parse(source)
	if err != nil {
		return source
	}
	return b.ResolveReference(s).String()
}

// isWindowsPath reports a drive-letter path, which is absolute on Windows
// whatever the platform reading it.
func isWindowsPath(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}

// FileURL is a file's absolute path as a file URL.
func FileURL(path string) string {
	p := filepath.ToSlash(path)
	if isWindowsPath(path) {
		p = strings.ReplaceAll(path, "\\", "/")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// Path is a file URL's path, as the platform writes it, or the URL itself
// where it is no file URL.
func Path(u string) string {
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme != "file" {
		return u
	}
	p := parsed.Path
	if runtime.GOOS == "windows" {
		p = strings.TrimPrefix(p, "/")
		return filepath.FromSlash(p)
	}
	return p
}

// URL is the URL a script is at, from the name it was run under: a file
// URL for an absolute path, and the name as it is otherwise.
func URL(name string) string {
	if filepath.IsAbs(name) || isWindowsPath(name) {
		return FileURL(name)
	}
	return name
}

// DataURL is the JSON a data: URL holds, if it is one of application/json,
// base64 or not.
func DataURL(u string) ([]byte, bool) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return nil, false
	}
	format, data, ok := strings.Cut(rest, ",")
	if !ok {
		return nil, false
	}
	parts := strings.Split(format, ";")
	if parts[0] != "application/json" {
		return nil, false
	}
	if parts[len(parts)-1] == "base64" {
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, false
		}
		return b, true
	}
	s, err := url.PathUnescape(data)
	if err != nil {
		return nil, false
	}
	return []byte(s), true
}
