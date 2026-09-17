package vm

import (
	"strings"
	"unicode/utf16"

	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// The observable protocol the RegExp symbol methods follow.
//
// Each of them is written against the receiver's properties rather than
// against a compiled pattern: the match comes from whatever `exec` the
// receiver has, the flags from its `flags` getter, the position from its
// `lastIndex`, and the pieces of a result from that result's own `index`,
// `length` and numbered properties. That is a good deal slower than reaching
// into the compiled pattern, and it is the only way a subclass that overrides
// any of those can be honoured -- which is the entire reason these live on
// RegExp.prototype rather than inside the String methods.
//
// The fast path is still there: an unmodified RegExp reaches the built-in exec
// below, which does the matching directly.

// regExpExec runs a pattern through whatever exec the receiver has.
func (r *Runtime) regExpExec(rx Value, s *String) (Value, error) {
	exec, err := r.getValueProp(rx, atomExec)
	if err != nil {
		return Undefined, err
	}
	if isCallable(exec) {
		res, err := r.call(exec, rx, []Value{Str(s)})
		if err != nil {
			return Undefined, err
		}
		// A result that is neither a match nor the absence of one leaves
		// everything downstream with nothing to read.
		if !res.IsObject() && !res.IsNull() {
			return Undefined, r.throwTypeError("exec must return an object or null")
		}
		return res, nil
	}
	return r.regexpExec(rx, s)
}

// regExpFlagsOf reads the receiver's flags as the string a script would see.
func (r *Runtime) regExpFlagsOf(rx Value) (string, error) {
	v, err := r.getValueProp(rx, atomFlags)
	if err != nil {
		return "", err
	}
	s, err := r.toString(v)
	if err != nil {
		return "", err
	}
	return s.Go(), nil
}

// advanceStringIndex steps past one position.
//
// Under u or v a surrogate pair is one character, so an empty match must step
// over both halves rather than land between them.
func advanceStringIndex(units []uint16, index int, fullUnicode bool) int {
	if !fullUnicode || index+1 >= len(units) {
		return index + 1
	}
	if utf16.IsSurrogate(rune(units[index])) &&
		utf16.DecodeRune(rune(units[index]), rune(units[index+1])) != 0xFFFD {
		return index + 2
	}
	return index + 1
}

// matchLastIndex reads lastIndex and writes back the position after an empty
// match, which is what stops a global pattern looping on one.
func (r *Runtime) advanceAfterEmptyMatch(rx Value, units []uint16, fullUnicode bool) error {
	li, err := r.getValueProp(rx, atomLastIndex)
	if err != nil {
		return err
	}
	n, err := r.toLength(li)
	if err != nil {
		return err
	}
	return r.setValueProp(rx, atomLastIndex,
		Int(advanceStringIndex(units, int(n), fullUnicode)), true)
}

// resultMatchString reads a result's element 0, which every method needs as a
// string whatever else it takes from the result.
func (r *Runtime) resultMatchString(result Value) (*String, error) {
	v, err := r.getValueProp(result, r.indexKey(0))
	if err != nil {
		return nil, err
	}
	s, err := r.toString(v)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// regExpSymbolMatch implements RegExp.prototype[Symbol.match].
func (r *Runtime) regExpSymbolMatch(rx Value, args []Value) (Value, error) {
	if !rx.IsObject() {
		return Undefined, r.throwTypeError("RegExp.prototype[Symbol.match] called on a non-object")
	}
	s, err := r.toString(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	flags, err := r.regExpFlagsOf(rx)
	if err != nil {
		return Undefined, err
	}
	// Without the global flag, match is exec: one result with its groups.
	if !strings.ContainsRune(flags, 'g') {
		return r.regExpExec(rx, s)
	}
	// With it, match returns every matched substring and no group information,
	// which is a different shape entirely.
	fullUnicode := strings.ContainsAny(flags, "uv")
	if err := r.setValueProp(rx, atomLastIndex, Int(0), true); err != nil {
		return Undefined, err
	}
	units := s.codeUnits()
	var out []Value
	for {
		if err := r.tick(); err != nil {
			return Undefined, err
		}
		result, err := r.regExpExec(rx, s)
		if err != nil {
			return Undefined, err
		}
		if result.IsNull() {
			if len(out) == 0 {
				return Null, nil
			}
			return Obj(r.newArrayFrom(out)), nil
		}
		matched, err := r.resultMatchString(result)
		if err != nil {
			return Undefined, err
		}
		out = append(out, Str(matched))
		if matched.Len() == 0 {
			if err := r.advanceAfterEmptyMatch(rx, units, fullUnicode); err != nil {
				return Undefined, err
			}
		}
	}
}

// regExpSymbolSearch implements RegExp.prototype[Symbol.search].
//
// The search starts at zero whatever lastIndex said, and puts it back
// afterwards: finding where a pattern occurs is not meant to move a global
// pattern along.
func (r *Runtime) regExpSymbolSearch(rx Value, args []Value) (Value, error) {
	if !rx.IsObject() {
		return Undefined, r.throwTypeError("RegExp.prototype[Symbol.search] called on a non-object")
	}
	s, err := r.toString(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	previous, err := r.getValueProp(rx, atomLastIndex)
	if err != nil {
		return Undefined, err
	}
	if !previous.SameValue(Int(0)) {
		if err := r.setValueProp(rx, atomLastIndex, Int(0), true); err != nil {
			return Undefined, err
		}
	}
	result, err := r.regExpExec(rx, s)
	if err != nil {
		// Nothing is put back: the search never finished, and what it left
		// behind is the zero it set on the way in.
		return Undefined, err
	}
	// The restore happens whether or not the match succeeded -- finding where a
	// pattern occurs is not meant to move a global pattern along.
	current, err := r.getValueProp(rx, atomLastIndex)
	if err != nil {
		return Undefined, err
	}
	if !current.SameValue(previous) {
		if err := r.setValueProp(rx, atomLastIndex, previous, true); err != nil {
			return Undefined, err
		}
	}
	if result.IsNull() {
		return Int(-1), nil
	}
	return r.getValueProp(result, atomIndex)
}

// regExpSymbolSplit implements RegExp.prototype[Symbol.split].
//
// The splitting is done with a fresh sticky copy of the receiver rather than
// the receiver itself, so that a pattern's own lastIndex is left alone and a
// non-sticky pattern cannot skip past text it should have split on.
func (r *Runtime) regExpSymbolSplit(rx Value, args []Value) (Value, error) {
	if !rx.IsObject() {
		return Undefined, r.throwTypeError("RegExp.prototype[Symbol.split] called on a non-object")
	}
	s, err := r.toString(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	ctor, err := r.speciesConstructor(rx.Object(), r.proto.regexpCtor)
	if err != nil {
		return Undefined, err
	}
	flags, err := r.regExpFlagsOf(rx)
	if err != nil {
		return Undefined, err
	}
	fullUnicode := strings.ContainsAny(flags, "uv")
	newFlags := flags
	if !strings.ContainsRune(flags, 'y') {
		newFlags += "y"
	}
	splitter, err := r.construct(ctor, []Value{rx, Str(NewString(newFlags))})
	if err != nil {
		return Undefined, err
	}

	limit := int64(1)<<32 - 1
	if lv := arg(args, 1); !lv.IsUndefined() {
		n, err := r.toUint32(lv)
		if err != nil {
			return Undefined, err
		}
		limit = int64(n)
	}
	var out []Value
	if limit == 0 {
		return Obj(r.newArrayFrom(out)), nil
	}

	units := s.codeUnits()
	size := len(units)
	if size == 0 {
		// Splitting an empty string yields one empty piece unless the pattern
		// matches it, in which case it yields none.
		z, err := r.regExpExec(splitter, s)
		if err != nil {
			return Undefined, err
		}
		if z.IsNull() {
			out = append(out, Str(s))
		}
		return Obj(r.newArrayFrom(out)), nil
	}

	piece := func(lo, hi int) Value { return Str(NewString(wtf8.FromUTF16(units[lo:hi]))) }
	p, q := 0, 0
	for q < size {
		if err := r.tick(); err != nil {
			return Undefined, err
		}
		if err := r.setValueProp(splitter, atomLastIndex, Int(q), true); err != nil {
			return Undefined, err
		}
		z, err := r.regExpExec(splitter, s)
		if err != nil {
			return Undefined, err
		}
		if z.IsNull() {
			q = advanceStringIndex(units, q, fullUnicode)
			continue
		}
		li, err := r.getValueProp(splitter, atomLastIndex)
		if err != nil {
			return Undefined, err
		}
		e64, err := r.toLength(li)
		if err != nil {
			return Undefined, err
		}
		e := int(min64(e64, int64(size)))
		if e == p {
			// A separator that matched emptily where the last one ended would
			// not make progress.
			q = advanceStringIndex(units, q, fullUnicode)
			continue
		}
		out = append(out, piece(p, q))
		if int64(len(out)) == limit {
			return Obj(r.newArrayFrom(out)), nil
		}
		p = e
		// A capturing separator contributes its groups to the result.
		n, err := r.resultCaptureCount(z)
		if err != nil {
			return Undefined, err
		}
		for i := int64(1); i <= n; i++ {
			cap, err := r.getValueProp(z, r.indexKey(i))
			if err != nil {
				return Undefined, err
			}
			out = append(out, cap)
			if int64(len(out)) == limit {
				return Obj(r.newArrayFrom(out)), nil
			}
		}
		q = p
	}
	out = append(out, piece(p, size))
	return Obj(r.newArrayFrom(out)), nil
}

// resultCaptureCount reads how many groups a result carries, which is its
// length less the whole match.
func (r *Runtime) resultCaptureCount(result Value) (int64, error) {
	lv, err := r.getValueProp(result, atomLength)
	if err != nil {
		return 0, err
	}
	n, err := r.toLength(lv)
	if err != nil {
		return 0, err
	}
	if n < 1 {
		return 0, nil
	}
	return n - 1, nil
}

// regExpSymbolReplace implements RegExp.prototype[Symbol.replace].
func (r *Runtime) regExpSymbolReplace(rx Value, args []Value) (Value, error) {
	if !rx.IsObject() {
		return Undefined, r.throwTypeError("RegExp.prototype[Symbol.replace] called on a non-object")
	}
	s, err := r.toString(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	units := s.codeUnits()
	size := len(units)

	replValue := arg(args, 1)
	functional := isCallable(replValue)
	template := ""
	if !functional {
		tv, err := r.toString(replValue)
		if err != nil {
			return Undefined, err
		}
		template = tv.Go()
	}

	flags, err := r.regExpFlagsOf(rx)
	if err != nil {
		return Undefined, err
	}
	global := strings.ContainsRune(flags, 'g')
	fullUnicode := false
	if global {
		fullUnicode = strings.ContainsAny(flags, "uv")
		if err := r.setValueProp(rx, atomLastIndex, Int(0), true); err != nil {
			return Undefined, err
		}
	}

	// Every match is collected before any replacement runs, because a
	// replacement function may itself use the pattern.
	var results []Value
	for {
		if err := r.tick(); err != nil {
			return Undefined, err
		}
		result, err := r.regExpExec(rx, s)
		if err != nil {
			return Undefined, err
		}
		if result.IsNull() {
			break
		}
		results = append(results, result)
		if !global {
			break
		}
		matched, err := r.resultMatchString(result)
		if err != nil {
			return Undefined, err
		}
		if matched.Len() == 0 {
			if err := r.advanceAfterEmptyMatch(rx, units, fullUnicode); err != nil {
				return Undefined, err
			}
		}
	}

	var sb strings.Builder
	next := 0
	for _, result := range results {
		nCaps, err := r.resultCaptureCount(result)
		if err != nil {
			return Undefined, err
		}
		matched, err := r.resultMatchString(result)
		if err != nil {
			return Undefined, err
		}
		matchLen := matched.Len()

		posv, err := r.getValueProp(result, atomIndex)
		if err != nil {
			return Undefined, err
		}
		posf, err := r.toInteger(posv)
		if err != nil {
			return Undefined, err
		}
		// A result may claim any index at all; it is clamped rather than
		// trusted, since it indexes the string being built.
		position := clampFloatIndex(posf, size)

		captures := make([]Value, 0, nCaps)
		for i := int64(1); i <= nCaps; i++ {
			c, err := r.getValueProp(result, r.indexKey(i))
			if err != nil {
				return Undefined, err
			}
			if !c.IsUndefined() {
				cs, err := r.toString(c)
				if err != nil {
					return Undefined, err
				}
				c = Str(cs)
			}
			captures = append(captures, c)
		}
		named, err := r.getValueProp(result, atomGroups)
		if err != nil {
			return Undefined, err
		}

		var replacement string
		if functional {
			callArgs := make([]Value, 0, len(captures)+4)
			callArgs = append(callArgs, Str(matched))
			callArgs = append(callArgs, captures...)
			callArgs = append(callArgs, Int(position), Str(s))
			if !named.IsUndefined() {
				callArgs = append(callArgs, named)
			}
			res, err := r.call(replValue, Undefined, callArgs)
			if err != nil {
				return Undefined, err
			}
			rs, err := r.toString(res)
			if err != nil {
				return Undefined, err
			}
			replacement = rs.Go()
		} else {
			if !named.IsUndefined() {
				o, err := r.toObject(named)
				if err != nil {
					return Undefined, err
				}
				named = Obj(o)
			}
			replacement, err = r.getSubstitution(matched, units, position,
				captures, named, template)
			if err != nil {
				return Undefined, err
			}
		}

		// Results are taken in order, so one claiming an index behind where the
		// last left off contributes its replacement and nothing else.
		if position >= next {
			sb.WriteString(wtf8.FromUTF16(units[next:position]))
			sb.WriteString(replacement)
			next = position + matchLen
			if next > size {
				next = size
			}
		}
	}
	if next < size {
		sb.WriteString(wtf8.FromUTF16(units[next:]))
	}
	return Str(NewString(sb.String())), nil
}

// getSubstitution expands the $ forms in a replacement template.
func (r *Runtime) getSubstitution(matched *String, units []uint16, position int,
	captures []Value, named Value, template string) (string, error) {
	if !strings.ContainsRune(template, '$') {
		return template, nil
	}
	tail := position + matched.Len()
	if tail > len(units) {
		tail = len(units)
	}

	var sb strings.Builder
	for i := 0; i < len(template); i++ {
		if template[i] != '$' || i+1 >= len(template) {
			sb.WriteByte(template[i])
			continue
		}
		switch c := template[i+1]; {
		case c == '$':
			sb.WriteByte('$')
			i++
		case c == '&':
			sb.WriteString(matched.Go())
			i++
		case c == '`':
			sb.WriteString(wtf8.FromUTF16(units[:position]))
			i++
		case c == '\'':
			sb.WriteString(wtf8.FromUTF16(units[tail:]))
			i++
		case c == '<':
			// A named group reference, which is only a reference at all when
			// the result carries groups; otherwise it is four literal
			// characters.
			if named.IsUndefined() {
				sb.WriteByte('$')
				continue
			}
			end := strings.IndexByte(template[i+2:], '>')
			if end < 0 {
				sb.WriteByte('$')
				continue
			}
			name := template[i+2 : i+2+end]
			v, err := r.getValueProp(named, r.atoms.intern(name))
			if err != nil {
				return "", err
			}
			if !v.IsUndefined() {
				vs, err := r.toString(v)
				if err != nil {
					return "", err
				}
				sb.WriteString(vs.Go())
			}
			i += 2 + end
		case c >= '0' && c <= '9':
			// Two digits are preferred when they name a real group, so $12
			// means group 12 if it exists and group 1 followed by "2" if not.
			idx := int(c - '0')
			consumed := 1
			if i+2 < len(template) && template[i+2] >= '0' && template[i+2] <= '9' {
				two := idx*10 + int(template[i+2]-'0')
				if two >= 1 && two <= len(captures) {
					idx = two
					consumed = 2
				}
			}
			if idx < 1 || idx > len(captures) {
				sb.WriteByte('$')
				continue
			}
			if v := captures[idx-1]; !v.IsUndefined() {
				sb.WriteString(v.String().Go())
			}
			i += consumed
		default:
			sb.WriteByte('$')
		}
	}
	return sb.String(), nil
}

// regExpSymbolMatchAll implements RegExp.prototype[Symbol.matchAll].
//
// The iterator holds its own copy of the pattern, so that walking the matches
// does not move the original's lastIndex and two walks do not interfere.
func (r *Runtime) regExpSymbolMatchAll(rx Value, args []Value) (Value, error) {
	if !rx.IsObject() {
		return Undefined, r.throwTypeError("RegExp.prototype[Symbol.matchAll] called on a non-object")
	}
	s, err := r.toString(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	ctor, err := r.speciesConstructor(rx.Object(), r.proto.regexpCtor)
	if err != nil {
		return Undefined, err
	}
	flags, err := r.regExpFlagsOf(rx)
	if err != nil {
		return Undefined, err
	}
	matcher, err := r.construct(ctor, []Value{rx, Str(NewString(flags))})
	if err != nil {
		return Undefined, err
	}
	li, err := r.getValueProp(rx, atomLastIndex)
	if err != nil {
		return Undefined, err
	}
	n, err := r.toLength(li)
	if err != nil {
		return Undefined, err
	}
	if err := r.setValueProp(matcher, atomLastIndex, Float(float64(n)), true); err != nil {
		return Undefined, err
	}
	return r.newRegExpStringIterator(matcher, s,
		strings.ContainsRune(flags, 'g'), strings.ContainsAny(flags, "uv")), nil
}

// newRegExpStringIterator produces the matches lazily, which is what lets
// matchAll be used on a long string without materializing every result.
func (r *Runtime) newRegExpStringIterator(matcher Value, s *String, global, fullUnicode bool) Value {
	iter := newObject(r.proto.regexpStringIter, ClassIterator)
	iter.data = &regExpStringIterData{
		matcher: matcher, s: s, units: s.codeUnits(),
		global: global, fullUnicode: fullUnicode,
	}
	return Obj(iter)
}

// regExpStringIterData is where a matchAll iterator is in its subject.
type regExpStringIterData struct {
	matcher     Value
	s           *String
	units       []uint16
	global      bool
	fullUnicode bool
	done        bool
}

// initRegExpStringIteratorProto fills in %RegExpStringIteratorPrototype%, whose
// next method every matchAll iterator shares.
func (r *Runtime) initRegExpStringIteratorProto() {
	p := r.proto.regexpStringIter
	r.defMethod(p, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		var d *regExpStringIterData
		if this.IsObject() {
			d, _ = this.Object().data.(*regExpStringIterData)
		}
		if d == nil {
			return Undefined, rt.throwTypeError(
				"RegExp String Iterator.prototype.next called on an incompatible receiver")
		}
		if d.done {
			return Obj(rt.iterResult(Undefined, true)), nil
		}
		result, err := rt.regExpExec(d.matcher, d.s)
		if err != nil {
			d.done = true
			return Undefined, err
		}
		if result.IsNull() {
			d.done = true
			return Obj(rt.iterResult(Undefined, true)), nil
		}
		if !d.global {
			d.done = true
			return Obj(rt.iterResult(result, false)), nil
		}
		matched, err := rt.resultMatchString(result)
		if err != nil {
			d.done = true
			return Undefined, err
		}
		if matched.Len() == 0 {
			if err := rt.advanceAfterEmptyMatch(d.matcher, d.units, d.fullUnicode); err != nil {
				d.done = true
				return Undefined, err
			}
		}
		return Obj(rt.iterResult(result, false)), nil
	})
	p.setOwnRaw(r.atoms.internSymbol(r.wellKnown.toStringTag),
		Str(NewString("RegExp String Iterator")), propConfigurable)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// clampFloatIndex narrows a coerced index into a string, treating anything at
// or below zero -- NaN included -- as the start.
func clampFloatIndex(f float64, size int) int {
	if !(f > 0) {
		return 0
	}
	if f > float64(size) {
		return size
	}
	return int(f)
}
