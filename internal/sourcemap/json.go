package sourcemap

import (
	"errors"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// A source map is JSON, read here rather than with encoding/json, which a
// program embedding the engine would otherwise link -- several hundred
// kilobytes -- for the stack traces of a host that maps them.

// value is a JSON value as read: nil, bool, float64, string, []any or
// map[string]any.
type value = any

var errJSON = errors.New("sourcemap: the map is not JSON")

// parseJSON reads one JSON value, the whole of data.
func parseJSON(data []byte) (value, error) {
	p := &jsonParser{s: data}
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i != len(p.s) {
		return nil, errJSON
	}
	return v, nil
}

type jsonParser struct {
	s []byte
	i int
}

func (p *jsonParser) space() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

// value reads a value, depth objects and arrays in.
func (p *jsonParser) value(depth int) (value, error) {
	if depth > 64 {
		return nil, errJSON
	}
	p.space()
	if p.i >= len(p.s) {
		return nil, errJSON
	}
	switch c := p.s[p.i]; {
	case c == '{':
		p.i++
		obj := map[string]any{}
		p.space()
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return obj, nil
		}
		for {
			p.space()
			k, err := p.string()
			if err != nil {
				return nil, err
			}
			p.space()
			if p.i >= len(p.s) || p.s[p.i] != ':' {
				return nil, errJSON
			}
			p.i++
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			obj[k] = v
			p.space()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.i < len(p.s) && p.s[p.i] == '}' {
				p.i++
				return obj, nil
			}
			return nil, errJSON
		}
	case c == '[':
		p.i++
		arr := []any{}
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return arr, nil
		}
		for {
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
			p.space()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.i < len(p.s) && p.s[p.i] == ']' {
				p.i++
				return arr, nil
			}
			return nil, errJSON
		}
	case c == '"':
		return p.string()
	case c == 't':
		return p.word("true", true)
	case c == 'f':
		return p.word("false", false)
	case c == 'n':
		return p.word("null", nil)
	case c == '-' || c >= '0' && c <= '9':
		start := p.i
		for p.i < len(p.s) && (p.s[p.i] == '-' || p.s[p.i] == '+' || p.s[p.i] == '.' ||
			p.s[p.i] == 'e' || p.s[p.i] == 'E' || p.s[p.i] >= '0' && p.s[p.i] <= '9') {
			p.i++
		}
		f, err := strconv.ParseFloat(string(p.s[start:p.i]), 64)
		if err != nil {
			return nil, errJSON
		}
		return f, nil
	}
	return nil, errJSON
}

func (p *jsonParser) word(w string, v value) (value, error) {
	if len(p.s)-p.i < len(w) || string(p.s[p.i:p.i+len(w)]) != w {
		return nil, errJSON
	}
	p.i += len(w)
	return v, nil
}

// string reads a string, its escapes undone.
func (p *jsonParser) string() (string, error) {
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		return "", errJSON
	}
	p.i++
	var out []byte
	for start := p.i; ; {
		if p.i >= len(p.s) {
			return "", errJSON
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			out = append(out, p.s[start:p.i]...)
			p.i++
			return string(out), nil
		case c < 0x20:
			return "", errJSON
		case c != '\\':
			p.i++
			continue
		}
		out = append(out, p.s[start:p.i]...)
		if p.i+1 >= len(p.s) {
			return "", errJSON
		}
		e := p.s[p.i+1]
		p.i += 2
		switch e {
		case '"', '\\', '/':
			out = append(out, e)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return "", errJSON
			}
			if utf16.IsSurrogate(r) && p.i+1 < len(p.s) && p.s[p.i] == '\\' && p.s[p.i+1] == 'u' {
				save := p.i
				p.i += 2
				if lo, ok := p.hex4(); ok && utf16.DecodeRune(r, lo) != utf8.RuneError {
					r = utf16.DecodeRune(r, lo)
				} else {
					p.i = save
				}
			}
			out = utf8.AppendRune(out, r)
		default:
			return "", errJSON
		}
		start = p.i
	}
}

func (p *jsonParser) hex4() (rune, bool) {
	if len(p.s)-p.i < 4 {
		return 0, false
	}
	n, err := strconv.ParseUint(string(p.s[p.i:p.i+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	p.i += 4
	return rune(n), true
}

// Reading the fields a map has.

func str(v value) string {
	s, _ := v.(string)
	return s
}

func num(v value) int {
	f, _ := v.(float64)
	return int(f)
}

func stringsOf(v value) []*string {
	arr, _ := v.([]any)
	out := make([]*string, len(arr))
	for i, e := range arr {
		if s, ok := e.(string); ok {
			out[i] = &s
		}
	}
	return out
}

// payloadOf is a map's fields, from its JSON.
func payloadOf(v value) (*payload, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errJSON
	}
	p := &payload{Version: num(obj["version"]), SourceRoot: str(obj["sourceRoot"]),
		Mappings: str(obj["mappings"]), Sources: stringsOf(obj["sources"])}
	for _, n := range stringsOf(obj["names"]) {
		name := ""
		if n != nil {
			name = *n
		}
		p.Names = append(p.Names, name)
	}
	if sections, ok := obj["sections"].([]any); ok {
		for _, sv := range sections {
			so, ok := sv.(map[string]any)
			if !ok {
				return nil, errJSON
			}
			var s section
			if off, ok := so["offset"].(map[string]any); ok {
				s.Offset.Line, s.Offset.Column = num(off["line"]), num(off["column"])
			}
			if mv, ok := so["map"]; ok && mv != nil {
				m, err := payloadOf(mv)
				if err != nil {
					return nil, err
				}
				s.Map = m
			}
			p.Sections = append(p.Sections, s)
		}
		if p.Sections == nil {
			p.Sections = []section{}
		}
	}
	return p, nil
}
