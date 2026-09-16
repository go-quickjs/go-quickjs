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
	r.defToStringTag(j, "JSON")

	r.defMethod(j, "stringify", 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
		indent, err := rt.jsonIndent(arg(args, 2))
		if err != nil {
			return Undefined, err
		}
		enc := &jsonEncoder{rt: rt, indent: indent, seen: map[*Object]bool{}}
		if err := enc.setReplacer(arg(args, 1)); err != nil {
			return Undefined, err
		}
		// The value is presented to the replacer as a property of a wrapper
		// object under the empty key, which is what gives the top-level call a
		// holder to pass along.
		root := newObject(rt.proto.object, ClassObject)
		root.setOwnRaw(rt.atoms.intern(""), arg(args, 0), propDefault)
		v, err := enc.apply(Obj(root), Str(emptyString), arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		out, ok, err := enc.encode(v, "")
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
		reviver := arg(args, 1)
		if !isCallable(reviver) {
			return v, nil
		}
		// The reviver walks the result bottom-up, so a nested value is already
		// revived by the time its parent sees it.
		root := newObject(rt.proto.object, ClassObject)
		root.setOwnRaw(rt.atoms.intern(""), v, propDefault)
		return rt.reviveJSON(root, Str(emptyString), reviver)
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

	// replacer is the function form, called for every key.
	replacer Value
	// allowed is the array form: the property names to keep, in the order they
	// were listed, which becomes the order of the output.
	allowed    []Atom
	hasAllowed bool
}

// setReplacer resolves the second argument, which is either a function called
// for every key or a list of the keys to keep.
func (e *jsonEncoder) setReplacer(v Value) error {
	e.replacer = Undefined
	if isCallable(v) {
		e.replacer = v
		return nil
	}
	if !v.IsObject() || !v.Object().IsArray() {
		return nil
	}
	a, err := e.rt.viewArrayLike(v)
	if err != nil {
		return err
	}
	e.hasAllowed = true
	seen := map[Atom]bool{}
	for i := int64(0); i < a.n; i++ {
		item, err := a.get(e.rt, i)
		if err != nil {
			return err
		}
		// Only strings and numbers name a property here; a Number or String
		// wrapper counts too, which is why the check is on the unwrapped kind.
		if item.IsObject() {
			switch item.Object().class {
			case ClassStringWrapper, ClassNumberWrapper:
				item, err = e.rt.toPrimitive(item, hintString)
				if err != nil {
					return err
				}
			}
		}
		if !item.IsString() && !item.IsNumber() {
			continue
		}
		key, err := e.rt.toPropertyKey(item)
		if err != nil {
			return err
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		e.allowed = append(e.allowed, key)
	}
	return nil
}

// apply runs toJSON and then the replacer on one value, in that order.
func (e *jsonEncoder) apply(holder Value, key Value, v Value) (Value, error) {
	if v.IsObject() || v.IsString() {
		tj, err := e.rt.getValueProp(v, atomToJSON)
		if err != nil {
			return Undefined, err
		}
		if isCallable(tj) {
			v, err = e.rt.call(tj, v, []Value{key})
			if err != nil {
				return Undefined, err
			}
		}
	}
	if isCallable(e.replacer) {
		return e.rt.call(e.replacer, holder, []Value{key, v})
	}
	return v, nil
}

// reviveJSON walks a parsed value bottom-up, handing each entry to the reviver.
//
// A value the reviver returns undefined for is deleted, which is how a reviver
// prunes what it does not want.
func (r *Runtime) reviveJSON(holder *Object, key Value, reviver Value) (Value, error) {
	k, err := r.toPropertyKey(key)
	if err != nil {
		return Undefined, err
	}
	val, err := r.getProp(holder, k, Obj(holder))
	if err != nil {
		return Undefined, err
	}
	if val.IsObject() {
		o := val.Object()
		if o.IsArray() {
			a, err := r.viewArrayLike(val)
			if err != nil {
				return Undefined, err
			}
			for i := int64(0); i < a.n; i++ {
				el, err := r.reviveJSON(o, Float(float64(i)), reviver)
				if err != nil {
					return Undefined, err
				}
				if el.IsUndefined() {
					if err := a.remove(r, i); err != nil {
						return Undefined, err
					}
					continue
				}
				if err := a.set(r, i, el); err != nil {
					return Undefined, err
				}
			}
		} else {
			for _, pk := range o.ownKeys(false, r.atoms) {
				if !r.isEnumerable(o, pk) {
					continue
				}
				el, err := r.reviveJSON(o, r.keyToValue(pk), reviver)
				if err != nil {
					return Undefined, err
				}
				if el.IsUndefined() {
					o.deleteOwn(pk)
					continue
				}
				if err := r.defineOwnProp(o, pk, el, propDefault); err != nil {
					return Undefined, err
				}
			}
		}
	}
	return r.call(reviver, Obj(holder), []Value{key, val})
}

// encode renders one value, reporting false when it has no JSON form.
func (e *jsonEncoder) encode(v Value, prefix string) (string, bool, error) {
	// toJSON and the replacer have already run: apply does both, and does it
	// before the value is inspected, so that what they return is what gets
	// encoded.
	//
	// A wrapper object stands for its primitive, which is what makes
	// JSON.stringify(new Number(1)) produce 1 rather than {}.
	if v.IsObject() {
		switch v.Object().class {
		case ClassNumberWrapper:
			n, err := e.rt.toNumber(v)
			if err != nil {
				return "", false, err
			}
			v = Float(n)
		case ClassStringWrapper:
			sv, err := e.rt.toString(v)
			if err != nil {
				return "", false, err
			}
			v = Str(sv)
		case ClassBooleanWrapper:
			b, _ := v.Object().data.(bool)
			v = Bool(b)
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
		a, err := e.rt.viewArrayLike(v)
		if err != nil {
			return "", false, err
		}
		if a.n == 0 {
			return "[]", true, nil
		}
		parts := make([]string, 0, a.n)
		for i := int64(0); i < a.n; i++ {
			el, err := a.get(e.rt, i)
			if err != nil {
				return "", false, err
			}
			el, err = e.apply(v, Float(float64(i)), el)
			if err != nil {
				return "", false, err
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

	// An explicit key list fixes both which properties appear and their order;
	// otherwise the object's own enumerable string keys are used.
	keys := e.allowed
	if !e.hasAllowed {
		keys = nil
		var own []Atom
		if p := proxyOf(o); p != nil {
			// A proxy's keys come from its ownKeys trap, which the plain
			// property table would not reflect.
			var err error
			if own, err = e.rt.proxyOwnKeys(p); err != nil {
				return "", false, err
			}
		} else {
			own = o.ownKeys(false, e.rt.atoms)
		}
		for _, k := range own {
			if e.rt.atoms.symbol(k) != nil {
				continue
			}
			if !e.rt.isEnumerable(o, k) {
				continue
			}
			keys = append(keys, k)
		}
	}

	var parts []string
	for _, k := range keys {
		val, err := e.rt.getProp(o, k, v)
		if err != nil {
			return "", false, err
		}
		val, err = e.apply(v, e.rt.keyToValue(k), val)
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
