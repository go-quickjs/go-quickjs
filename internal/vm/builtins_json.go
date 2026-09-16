package vm

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// JSON.stringify and JSON.parse.
//
// These are written against the engine's own value model rather than Go's
// encoding/json, because the two disagree in ways that matter: JavaScript
// distinguishes undefined from null, drops undefined-valued properties,
// preserves property insertion order, and escapes lone surrogates rather than
// replacing them.

func (r *Runtime) initJSONBuiltins() {
	j := newObject(r.proto.object, ClassJSONObject)
	r.defValue(r.global, "JSON", Obj(j))
	r.defConst(j, "[Symbol.toStringTag]", Str(NewString("JSON")))

	r.defMethod(j, "stringify", 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
		indent, err := rt.jsonIndent(arg(args, 2))
		if err != nil {
			return Undefined, err
		}
		enc := &jsonEncoder{rt: rt, indent: indent, seen: map[*Object]bool{}}
		out, ok, err := enc.encode(arg(args, 0), "")
		if err != nil {
			return Undefined, err
		}
		if !ok {
			// A value with no JSON representation, such as undefined or a
			// function, stringifies to undefined rather than to a string.
			return Undefined, nil
		}
		return Str(NewString(out)), nil
	})

	r.defMethod(j, "parse", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		p := &jsonParser{rt: rt, src: s.Go()}
		p.skipSpace()
		v, err := p.parseValue()
		if err != nil {
			return Undefined, err
		}
		p.skipSpace()
		if p.pos != len(p.src) {
			return Undefined, rt.throwSyntaxError("unexpected trailing content in JSON at position %d", p.pos)
		}
		return v, nil
	})
}

// jsonIndent resolves the space argument into the indent string it denotes.
func (r *Runtime) jsonIndent(v Value) (string, error) {
	switch {
	case v.IsNumber():
		n, err := r.toInteger(v)
		if err != nil {
			return "", err
		}
		if n > 10 {
			n = 10
		}
		if n <= 0 {
			return "", nil
		}
		return strings.Repeat(" ", int(n)), nil
	case v.IsString():
		s := v.String().Go()
		if len(s) > 10 {
			s = s[:10]
		}
		return s, nil
	}
	return "", nil
}

type jsonEncoder struct {
	rt     *Runtime
	indent string
	// seen detects the cycles that would otherwise recurse forever.
	seen map[*Object]bool
}

// encode renders one value, reporting false when it has no JSON form.
func (e *jsonEncoder) encode(v Value, prefix string) (string, bool, error) {
	// A toJSON method replaces the value entirely.
	if v.IsObject() || v.IsString() {
		tj, err := e.rt.getValueProp(v, atomToJSON)
		if err != nil {
			return "", false, err
		}
		if isCallable(tj) {
			res, err := e.rt.call(tj, v, nil)
			if err != nil {
				return "", false, err
			}
			v = res
		}
	}

	switch v.Kind() {
	case KindNull:
		return "null", true, nil
	case KindBool:
		if v.BoolValue() {
			return "true", true, nil
		}
		return "false", true, nil
	case KindNumber:
		n := v.Number()
		// A non-finite number has no JSON form and becomes null.
		if n != n || n > 1e308*1.7 || n < -1e308*1.7 {
			return "null", true, nil
		}
		return jsnum.FormatFloat(n), true, nil
	case KindString:
		return encodeJSONString(v.String().Go()), true, nil
	case KindUndefined, KindSymbol:
		return "", false, nil
	case KindBigInt:
		return "", false, e.rt.throwTypeError("a BigInt cannot be serialized to JSON")
	}

	o := v.Object()
	if o.IsCallable() {
		return "", false, nil
	}
	if e.seen[o] {
		return "", false, e.rt.throwTypeError("converting a circular structure to JSON")
	}
	e.seen[o] = true
	defer delete(e.seen, o)

	inner := prefix + e.indent
	sep, open, close := ",", "", ""
	if e.indent != "" {
		sep = ",\n" + inner
		open = "\n" + inner
		close = "\n" + prefix
	}

	if o.IsArray() {
		if len(o.elems) == 0 {
			return "[]", true, nil
		}
		parts := make([]string, 0, len(o.elems))
		for _, el := range o.elems {
			if isHole(el) {
				parts = append(parts, "null")
				continue
			}
			s, ok, err := e.encode(el, inner)
			if err != nil {
				return "", false, err
			}
			if !ok {
				// An element with no JSON form becomes null, unlike a
				// property, which is omitted.
				s = "null"
			}
			parts = append(parts, s)
		}
		return "[" + open + strings.Join(parts, sep) + close + "]", true, nil
	}

	var parts []string
	for _, k := range o.ownKeys(false, e.rt.atoms) {
		if !e.rt.isEnumerable(o, k) {
			continue
		}
		val, err := e.rt.getProp(o, k, v)
		if err != nil {
			return "", false, err
		}
		s, ok, err := e.encode(val, inner)
		if err != nil {
			return "", false, err
		}
		if !ok {
			continue
		}
		colon := ":"
		if e.indent != "" {
			colon = ": "
		}
		parts = append(parts, encodeJSONString(e.rt.atoms.name(k))+colon+s)
	}
	if len(parts) == 0 {
		return "{}", true, nil
	}
	return "{" + open + strings.Join(parts, sep) + close + "}", true, nil
}

// encodeJSONString quotes a string, escaping the characters JSON requires plus
// any lone surrogate, which must be written as an escape because it has no
// valid UTF-8 form.
func encodeJSONString(s string) string {
	var sb strings.Builder
	sb.Grow(len(s) + 2)
	sb.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				sb.WriteString(`\"`)
			case '\\':
				sb.WriteString(`\\`)
			case '\n':
				sb.WriteString(`\n`)
			case '\r':
				sb.WriteString(`\r`)
			case '\t':
				sb.WriteString(`\t`)
			case '\b':
				sb.WriteString(`\b`)
			case '\f':
				sb.WriteString(`\f`)
			default:
				if c < 0x20 {
					sb.WriteString(`\u00`)
					const hex = "0123456789abcdef"
					sb.WriteByte(hex[c>>4])
					sb.WriteByte(hex[c&0xF])
				} else {
					sb.WriteByte(c)
				}
			}
			i++
			continue
		}
		if r, ok := wtf8.DecodeSurrogateAt(s, i); ok {
			// A lone surrogate is emitted as an escape, which is what the
			// specification's well-formed stringify requires.
			sb.WriteString(`\u`)
			sb.WriteString(strconv.FormatUint(uint64(r), 16))
			i += 3
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		sb.WriteString(s[i : i+size])
		i += size
	}
	sb.WriteByte('"')
	return sb.String()
}

// jsonParser is a recursive-descent parser for JSON text.
type jsonParser struct {
	rt  *Runtime
	src string
	pos int
}

func (p *jsonParser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) parseValue() (Value, error) {
	if p.pos >= len(p.src) {
		return Undefined, p.rt.throwSyntaxError("unexpected end of JSON input")
	}
	switch c := p.src[p.pos]; {
	case c == '{':
		return p.parseObject()
	case c == '[':
		return p.parseArray()
	case c == '"':
		s, err := p.parseString()
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(s)), nil
	case strings.HasPrefix(p.src[p.pos:], "true"):
		p.pos += 4
		return True, nil
	case strings.HasPrefix(p.src[p.pos:], "false"):
		p.pos += 5
		return False, nil
	case strings.HasPrefix(p.src[p.pos:], "null"):
		p.pos += 4
		return Null, nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.parseNumber()
	}
	return Undefined, p.rt.throwSyntaxError(
		"unexpected token %q in JSON at position %d", p.src[p.pos], p.pos)
}

func (p *jsonParser) parseObject() (Value, error) {
	o := newObject(p.rt.proto.object, ClassObject)
	p.pos++ // consume '{'
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		return Obj(o), nil
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != '"' {
			return Undefined, p.rt.throwSyntaxError(
				"expected a property name in JSON at position %d", p.pos)
		}
		key, err := p.parseString()
		if err != nil {
			return Undefined, err
		}
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return Undefined, p.rt.throwSyntaxError("expected ':' in JSON at position %d", p.pos)
		}
		p.pos++
		p.skipSpace()
		v, err := p.parseValue()
		if err != nil {
			return Undefined, err
		}
		if err := p.rt.defineOwnProp(o, p.rt.atoms.intern(key), v, propDefault); err != nil {
			return Undefined, err
		}
		p.skipSpace()
		if p.pos >= len(p.src) {
			return Undefined, p.rt.throwSyntaxError("unexpected end of JSON input")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return Obj(o), nil
		default:
			return Undefined, p.rt.throwSyntaxError(
				"expected ',' or '}' in JSON at position %d", p.pos)
		}
	}
}

func (p *jsonParser) parseArray() (Value, error) {
	var elems []Value
	p.pos++ // consume '['
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		return Obj(p.rt.newArrayFrom(nil)), nil
	}
	for {
		p.skipSpace()
		v, err := p.parseValue()
		if err != nil {
			return Undefined, err
		}
		elems = append(elems, v)
		p.skipSpace()
		if p.pos >= len(p.src) {
			return Undefined, p.rt.throwSyntaxError("unexpected end of JSON input")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return Obj(p.rt.newArrayFrom(elems)), nil
		default:
			return Undefined, p.rt.throwSyntaxError(
				"expected ',' or ']' in JSON at position %d", p.pos)
		}
	}
}

func (p *jsonParser) parseString() (string, error) {
	p.pos++ // consume '"'
	var sb strings.Builder
	for {
		if p.pos >= len(p.src) {
			return "", p.rt.throwSyntaxError("unterminated string in JSON")
		}
		c := p.src[p.pos]
		switch c {
		case '"':
			p.pos++
			return sb.String(), nil
		case '\\':
			p.pos++
			if p.pos >= len(p.src) {
				return "", p.rt.throwSyntaxError("unterminated escape in JSON")
			}
			switch p.src[p.pos] {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'u':
				r, err := p.parseUnicodeEscape()
				if err != nil {
					return "", err
				}
				// A lone surrogate is preserved rather than replaced.
				var buf [4]byte
				sb.Write(wtf8.AppendRune(buf[:0], r))
				continue
			default:
				return "", p.rt.throwSyntaxError(
					"invalid escape %q in JSON at position %d", p.src[p.pos], p.pos)
			}
			p.pos++
		default:
			if c < 0x20 {
				return "", p.rt.throwSyntaxError(
					"a control character is not allowed in a JSON string at position %d", p.pos)
			}
			sb.WriteByte(c)
			p.pos++
		}
	}
}

// parseUnicodeEscape reads \uXXXX, combining a surrogate pair when the next
// escape completes one.
func (p *jsonParser) parseUnicodeEscape() (rune, error) {
	if p.pos+4 >= len(p.src) {
		return 0, p.rt.throwSyntaxError("truncated \\u escape in JSON")
	}
	v, err := strconv.ParseUint(p.src[p.pos+1:p.pos+5], 16, 32)
	if err != nil {
		return 0, p.rt.throwSyntaxError("invalid \\u escape in JSON at position %d", p.pos)
	}
	p.pos += 5
	r := rune(v)
	if r >= 0xD800 && r <= 0xDBFF && strings.HasPrefix(p.src[p.pos:], `\u`) && p.pos+6 <= len(p.src) {
		if lo, err := strconv.ParseUint(p.src[p.pos+2:p.pos+6], 16, 32); err == nil {
			if lo >= 0xDC00 && lo <= 0xDFFF {
				p.pos += 6
				return 0x10000 + (r-0xD800)<<10 + (rune(lo) - 0xDC00), nil
			}
		}
	}
	return r, nil
}

// parseNumber reads a JSON number, which is a strict subset of the JavaScript
// numeric grammar: no leading plus, no leading zeros, no hexadecimal, and a
// digit required on both sides of a decimal point.
func (p *jsonParser) parseNumber() (Value, error) {
	start := p.pos
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		p.pos++
	}
	digitsStart := p.pos
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}
	if p.pos == digitsStart {
		return Undefined, p.rt.throwSyntaxError("invalid number in JSON at position %d", start)
	}
	// A leading zero may not be followed by another digit.
	if p.src[digitsStart] == '0' && p.pos-digitsStart > 1 {
		return Undefined, p.rt.throwSyntaxError("invalid number in JSON at position %d", start)
	}
	if p.pos < len(p.src) && p.src[p.pos] == '.' {
		p.pos++
		fracStart := p.pos
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
		if p.pos == fracStart {
			return Undefined, p.rt.throwSyntaxError("invalid number in JSON at position %d", start)
		}
	}
	if p.pos < len(p.src) && (p.src[p.pos] == 'e' || p.src[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.src) && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
			p.pos++
		}
		expStart := p.pos
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
		if p.pos == expStart {
			return Undefined, p.rt.throwSyntaxError("invalid number in JSON at position %d", start)
		}
	}
	f, err := strconv.ParseFloat(p.src[start:p.pos], 64)
	if err != nil {
		// Overflow yields infinity, which is the same result JavaScript gives.
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return Float(f), nil
		}
		return Undefined, p.rt.throwSyntaxError("invalid number in JSON at position %d", start)
	}
	return Float(f), nil
}
