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
		// holder to pass along. Nothing else can observe that object, so it is
		// built only when there is a replacer to see it.
		holder := Undefined
		if isCallable(enc.replacer) {
			root := newObject(rt.proto.object, ClassObject)
			root.setOwnRaw(rt.atoms.intern(""), arg(args, 0), propDefault)
			holder = Obj(root)
		}
		v, err := enc.apply(holder, Str(emptyString), arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		buf, ok, err := enc.encode(make([]byte, 0, 64), v, "")
		if err != nil {
			return Undefined, err
		}
		if !ok {
			// A value with no JSON representation, such as undefined or a
			// function, stringifies to undefined rather than to a string.
			return Undefined, nil
		}
		return Str(NewString(string(buf))), nil
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
	// A Number or String object stands for the primitive it wraps, which is
	// what makes JSON.stringify(x, null, new Number(2)) indent by two.
	if v.IsObject() {
		switch v.Object().class {
		case ClassNumberWrapper:
			n, err := r.toNumber(v)
			if err != nil {
				return "", err
			}
			v = Float(n)
		case ClassStringWrapper:
			sv, err := r.toString(v)
			if err != nil {
				return "", err
			}
			v = Str(sv)
		}
	}
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
		// Ten code units, not ten bytes: a non-ASCII indent would otherwise be
		// cut in the middle of a character.
		s := v.String()
		if s.Len() > 10 {
			s = s.Substring(0, 10)
		}
		return s.Go(), nil
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

// needsApply reports whether apply could do anything to v, which saves
// building the key string for the values it would hand straight back: a
// number, a boolean or null has no toJSON to look up, so only a replacer can
// take an interest in it.
func (e *jsonEncoder) needsApply(v Value) bool {
	return v.IsObject() || v.IsString() || v.IsBigInt() || isCallable(e.replacer)
}

// apply runs toJSON and then the replacer on one value, in that order.
func (e *jsonEncoder) apply(holder Value, key Value, v Value) (Value, error) {
	// A BigInt has a toJSON hook like an object does, which is the only way to
	// serialize one at all: without it there is no JSON form and the attempt
	// is an error.
	if v.IsObject() || v.IsString() || v.IsBigInt() {
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
				el, err := r.reviveJSON(o, Str(NewString(strconv.FormatInt(i, 10))), reviver)
				if err != nil {
					return Undefined, err
				}
				if err := r.reviveWrite(o, r.atoms.indexAtom(uint32(i)), el); err != nil {
					return Undefined, err
				}
			}
		} else {
			// The keys are settled before the walk starts, so a property the
			// reviver deletes is still visited -- and then read through the
			// prototype chain, which is what Get does.
			pks, err := r.ownKeysOf(o, false)
			if err != nil {
				return Undefined, err
			}
			keys := make([]Atom, 0, len(pks))
			for _, pk := range pks {
				if r.atoms.symbol(pk) != nil {
					continue
				}
				enumerable, err := r.isEnumerable(o, pk)
				if err != nil {
					return Undefined, err
				}
				if enumerable {
					keys = append(keys, pk)
				}
			}
			for _, pk := range keys {
				el, err := r.reviveJSON(o, r.keyToValue(pk), reviver)
				if err != nil {
					return Undefined, err
				}
				if err := r.reviveWrite(o, pk, el); err != nil {
					return Undefined, err
				}
			}
		}
	}
	return r.call(reviver, Obj(holder), []Value{key, val})
}

// reviveWrite puts back what the reviver returned, or removes the property
// when it returned undefined -- which is how a reviver prunes what it does not
// want.
//
// A refusal is ignored: a property that cannot be redefined simply keeps its
// value. A trap that throws is not, since that is user code failing rather than
// the object declining.
func (r *Runtime) reviveWrite(o *Object, key Atom, v Value) error {
	if v.IsUndefined() {
		_, err := r.deleteProp(o, key, false)
		return err
	}
	_, err := r.defineProperty(o, key, &propDesc{
		value: v, hasValue: true,
		writable: true, hasWritable: true,
		enumerable: true, hasEnumerable: true,
		configurable: true, hasConfigurable: true,
	})
	return err
}

// encode renders one value, reporting false when it has no JSON form.
// encode appends v's JSON text to buf and reports whether v has any.
//
// Everything is written into the one buffer the top-level call starts, rather
// than each value building a string its parent then joins: the output of a
// nested structure is the same either way, but the intermediates are not
// allocated.
//
// A value with no JSON form -- undefined, a function, a symbol -- appends
// nothing and reports false, so a caller that has already written a key can
// take it back by truncating to the length it saw.
func (e *jsonEncoder) encode(buf []byte, v Value, prefix string) ([]byte, bool, error) {
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
				return buf, false, err
			}
			v = Float(n)
		case ClassStringWrapper:
			sv, err := e.rt.toString(v)
			if err != nil {
				return buf, false, err
			}
			v = Str(sv)
		case ClassBooleanWrapper:
			b, _ := v.Object().data.(bool)
			v = Bool(b)
		case ClassBigIntWrapper:
			// It stands for the BigInt it wraps, which has no JSON form -- so
			// the wrapper does not serialize as an empty object.
			bi, err := e.rt.toBigIntValue(v)
			if err != nil {
				return buf, false, err
			}
			v = bi
		}
	}

	switch v.Kind() {
	case KindNull:
		return append(buf, "null"...), true, nil
	case KindBool:
		if v.BoolValue() {
			return append(buf, "true"...), true, nil
		}
		return append(buf, "false"...), true, nil
	case KindNumber:
		n := v.Number()
		// A non-finite number has no JSON form and becomes null.
		if n != n || n > 1e308*1.7 || n < -1e308*1.7 {
			return append(buf, "null"...), true, nil
		}
		return jsnum.AppendFloat(buf, n), true, nil
	case KindString:
		return appendJSONString(buf, v.String().Go()), true, nil
	case KindUndefined, KindSymbol:
		return buf, false, nil
	case KindBigInt:
		return buf, false, e.rt.throwTypeError("a BigInt cannot be serialized to JSON")
	}

	o := v.Object()
	if o.IsCallable() {
		return buf, false, nil
	}
	if e.seen[o] {
		return buf, false, e.rt.throwTypeError("converting a circular structure to JSON")
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
			return buf, false, err
		}
		if a.n == 0 {
			return append(buf, "[]"...), true, nil
		}
		buf = append(buf, '[')
		buf = append(buf, open...)
		var keybuf []byte
		for i := int64(0); i < a.n; i++ {
			if i > 0 {
				buf = append(buf, sep...)
			}
			el, err := a.get(e.rt, i)
			if err != nil {
				return buf, false, err
			}
			if e.needsApply(el) {
				// The key reaches the replacer as a string, the way a property
				// name does: an array index is not a number here.
				keybuf = strconv.AppendInt(keybuf[:0], i, 10)
				el, err = e.apply(v, Str(NewString(string(keybuf))), el)
				if err != nil {
					return buf, false, err
				}
			}
			var ok bool
			buf, ok, err = e.encode(buf, el, inner)
			if err != nil {
				return buf, false, err
			}
			if !ok {
				// An element with no JSON form becomes null, unlike a
				// property, which is omitted.
				buf = append(buf, "null"...)
			}
		}
		buf = append(buf, close...)
		return append(buf, ']'), true, nil
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
				return buf, false, err
			}
		} else {
			e.rt.materializeFunctionProto(o)
			own = o.ownKeys(false, e.rt.atoms)
		}
		for _, k := range own {
			if e.rt.atoms.symbol(k) != nil {
				continue
			}
			enumerable, err := e.rt.isEnumerable(o, k)
			if err != nil {
				return buf, false, err
			}
			if !enumerable {
				continue
			}
			keys = append(keys, k)
		}
	}

	colon := ":"
	if e.indent != "" {
		colon = ": "
	}
	start := len(buf)
	buf = append(buf, '{')
	buf = append(buf, open...)
	written := 0
	for _, k := range keys {
		val, err := e.rt.getProp(o, k, v)
		if err != nil {
			return buf, false, err
		}
		if e.needsApply(val) {
			if val, err = e.apply(v, e.rt.keyToValue(k), val); err != nil {
				return buf, false, err
			}
		}
		// A property whose value has no JSON form is omitted, key and all, so
		// the separator and the key are written first and taken back when the
		// value turns out not to be there.
		mark := len(buf)
		if written > 0 {
			buf = append(buf, sep...)
		}
		buf = appendJSONString(buf, e.rt.atoms.name(k))
		buf = append(buf, colon...)
		var ok bool
		buf, ok, err = e.encode(buf, val, inner)
		if err != nil {
			return buf, false, err
		}
		if !ok {
			buf = buf[:mark]
			continue
		}
		written++
	}
	if written == 0 {
		return append(buf[:start], "{}"...), true, nil
	}
	buf = append(buf, close...)
	return append(buf, '}'), true, nil
}

// appendJSONString appends a quoted string, escaping the characters JSON
// requires plus any lone surrogate, which must be written as an escape because
// it has no valid UTF-8 form.
//
// A string with nothing to escape -- which most are -- is copied in one go
// rather than a byte at a time.
func appendJSONString(buf []byte, s string) []byte {
	buf = append(buf, '"')
	plain := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			buf = append(buf, s[plain:i]...)
			switch c {
			case '"':
				buf = append(buf, `\"`...)
			case '\\':
				buf = append(buf, `\\`...)
			case '\n':
				buf = append(buf, `\n`...)
			case '\r':
				buf = append(buf, `\r`...)
			case '\t':
				buf = append(buf, `\t`...)
			case '\b':
				buf = append(buf, `\b`...)
			case '\f':
				buf = append(buf, `\f`...)
			default:
				const hex = "0123456789abcdef"
				buf = append(buf, `\u00`...)
				buf = append(buf, hex[c>>4], hex[c&0xF])
			}
			i++
			plain = i
			continue
		}
		if r, ok := wtf8.DecodeSurrogateAt(s, i); ok {
			// A lone surrogate is emitted as an escape, which is what the
			// specification's well-formed stringify requires.
			buf = append(buf, s[plain:i]...)
			buf = append(buf, `\u`...)
			buf = strconv.AppendUint(buf, uint64(r), 16)
			i += 3
			plain = i
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	buf = append(buf, s[plain:]...)
	return append(buf, '"')
}

// jsonParser is a recursive-descent parser for JSON text.
type jsonParser struct {
	rt  *Runtime
	src string
	pos int
	// scratch collects the elements of the arrays being parsed, one array's
	// above its parent's. Each is copied out at exactly its own length when
	// its closing bracket arrives, so growing this one buffer replaces growing
	// a slice per array.
	scratch []Value
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
	// How many properties there are is not known without parsing them, and
	// growing the table from nothing costs an allocation per doubling. Room
	// for a few, in the object's own allocation, pays for itself by the second
	// key.
	o := newLiteralObject(p.rt.proto.object, ClassObject, 0)
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
	p.pos++ // consume '['
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		return Obj(p.rt.newArrayFrom(nil)), nil
	}
	mark := len(p.scratch)
	for {
		p.skipSpace()
		v, err := p.parseValue()
		if err != nil {
			return Undefined, err
		}
		p.scratch = append(p.scratch, v)
		p.skipSpace()
		if p.pos >= len(p.src) {
			return Undefined, p.rt.throwSyntaxError("unexpected end of JSON input")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			elems := make([]Value, len(p.scratch)-mark)
			copy(elems, p.scratch[mark:])
			p.scratch = p.scratch[:mark]
			return Obj(p.rt.newArrayFrom(elems)), nil
		default:
			return Undefined, p.rt.throwSyntaxError(
				"expected ',' or ']' in JSON at position %d", p.pos)
		}
	}
}

func (p *jsonParser) parseString() (string, error) {
	p.pos++ // consume '"'

	// A string with no escape in it -- which nearly every string in real JSON
	// is -- is a slice of the source text, costing neither a copy nor an
	// allocation. Sharing the bytes is what the engine's own substrings do.
	start := p.pos
	i := p.pos
	for i < len(p.src) {
		c := p.src[i]
		if c == '"' {
			p.pos = i + 1
			return p.src[start:i], nil
		}
		if c == '\\' {
			break
		}
		if c < 0x20 {
			p.pos = i
			return "", p.rt.throwSyntaxError(
				"a control character is not allowed in a JSON string at position %d", i)
		}
		i++
	}
	if i >= len(p.src) {
		p.pos = i
		return "", p.rt.throwSyntaxError("unterminated string in JSON")
	}

	// From the first escape on, the string has to be built.
	var sb strings.Builder
	sb.WriteString(p.src[start:i])
	p.pos = i
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
