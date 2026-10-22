package vm

import (
	"strings"
	"unicode/utf16"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/regexp"
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
		var res Value
		if exec.Object() == r.regexpExecFn {
			res, err = r.callIntrinsic1(r.regexpExecFn, rx, Str(s))
		} else {
			res, err = r.call(exec, rx, []Value{Str(s)})
		}
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
	if flags, ok := r.builtinFlags(rx); ok {
		return flags, nil
	}
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

// subject is a string as a match reads it: an ASCII one by its bytes, which
// are its code units, matched where they are; any other by its code units,
// which the string keeps once they are made. An ASCII string matched against
// a pattern is not given its code units, which would be twice its bytes for
// as long as it lives, made in a pass over all of it before a match that
// may read only its start.
type subject struct {
	s     *String
	ascii bool
	// bytes is an ASCII subject's text, and units any other's code units.
	bytes string
	units []uint16
}

// subjectOf is s as a match reads it.
func subjectOf(s *String) subject {
	if s.ascii {
		return subject{s: s, ascii: true, bytes: s.Go()}
	}
	return subject{s: s, units: s.codeUnits()}
}

// len is the subject's length in code units.
func (j *subject) len() int {
	if j.ascii {
		return len(j.bytes)
	}
	return len(j.units)
}

// inPair reports whether index i falls between the halves of a pair of
// surrogates, which a pattern that reads code points begins a match at the
// first of.
func (j *subject) inPair(i int) bool {
	return !j.ascii && i > 0 && i < len(j.units) &&
		j.units[i]&0xFC00 == 0xDC00 && j.units[i-1]&0xFC00 == 0xD800
}

// match is re's match at start, into dst, as MatchCheckedInto has it.
func (j *subject) match(re *regexp.Regexp, dst []int, start int, check func() error) ([]int, error) {
	if j.ascii {
		return re.MatchASCIICheckedInto(dst, j.bytes, start, check)
	}
	return re.MatchCheckedInto(dst, j.units, start, check)
}

// search is re's first match at start or after it, as SearchCheckedInto has
// it.
func (j *subject) search(re *regexp.Regexp, dst []int, start int, check func() error) ([]int, error) {
	if j.ascii {
		return re.SearchASCIICheckedInto(dst, j.bytes, start, check)
	}
	return re.SearchCheckedInto(dst, j.units, start, check)
}

// advance is advanceStringIndex in the subject.
func (j *subject) advance(index int, fullUnicode bool) int {
	if j.ascii {
		return index + 1
	}
	return advanceStringIndex(j.units, index, fullUnicode)
}

// slice is the subject's code units i to k, as a Go string.
func (j *subject) slice(i, k int) string { return j.s.unitSlice(j.units, i, k) }

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
func (r *Runtime) advanceAfterEmptyMatch(rx Value, j *subject, fullUnicode bool) error {
	li, err := r.getValueProp(rx, atomLastIndex)
	if err != nil {
		return err
	}
	n, err := r.toLength(li)
	if err != nil {
		return err
	}
	if n >= int64(j.len()) {
		// Past the end it only counts on, from where an int may not reach.
		return r.setValueProp(rx, atomLastIndex, Float(float64(n+1)), true)
	}
	return r.setValueProp(rx, atomLastIndex, Int(j.advance(int(n), fullUnicode)), true)
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
	// A RegExp whose flags would be read through the built-in getters is
	// asked for them as bits, which is the same answer without the string.
	fl, builtin := r.builtinFlagBits(rx)
	if !builtin {
		flags, err := r.regExpFlagsOf(rx)
		if err != nil {
			return Undefined, err
		}
		fl = 0
		if strings.ContainsRune(flags, 'g') {
			fl |= regexp.FlagGlobal
		}
		if strings.ContainsAny(flags, "uv") {
			fl |= regexp.FlagUnicode
		}
	}
	// Without the global flag, match is exec: one result with its groups.
	if fl&regexp.FlagGlobal == 0 {
		return r.regExpExec(rx, s)
	}
	// With it, match returns every matched substring and no group information,
	// which is a different shape entirely.
	fullUnicode := fl&(regexp.FlagUnicode|regexp.FlagUnicodeSets) != 0
	if builtin {
		err = r.setLastIndex(rx.Object(), Int(0))
	} else {
		err = r.setValueProp(rx, atomLastIndex, Int(0), true)
	}
	if err != nil {
		return Undefined, err
	}
	var out []Value
	// Where exec is the built-in, the matches are found as replace finds
	// them, without the arrays exec would make of them for this to read
	// element 0 of.
	if builtin && r.builtinExecOf(rx) {
		spans, err := r.execMatchAll(rx, s, fullUnicode, r.takeSpans())
		// Nothing below runs a script, which could match: the room is
		// given back at once, and read until the array is made.
		r.giveSpans(spans)
		if err != nil {
			return Undefined, err
		}
		if len(spans) == 0 {
			return Null, nil
		}
		// The matched substrings are the array's elements, written there.
		width := 2 * (rx.Object().dataOf().(*regexpData).re.GroupCount() + 1)
		arr := newArrayObject(r.proto.array, len(spans)/width)
		elems := arr.elemsOf()
		strs := newPieces(s, spans, width)
		for i := range elems {
			lo, hi := spans[i*width], spans[i*width+1]
			if strs != nil && ownPiece(s, lo, hi) {
				elems[i] = Str(s.substringInto(&strs[0], lo, hi))
				strs = strs[1:]
				continue
			}
			elems[i] = Str(s.Substring(lo, hi))
		}
		return Obj(arr), nil
	}
	j := subjectOf(s)
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
			if err := r.advanceAfterEmptyMatch(rx, &j, fullUnicode); err != nil {
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
	// Where exec is the built-in, its match is where it is found, without the
	// array, which only this would read its index of.
	result, index := Null, -1
	if r.execIsBuiltin(rx) {
		caps, err := r.execMatch(rx, s)
		if err != nil {
			return Undefined, err
		}
		if caps != nil {
			index = caps[0]
		}
	} else if result, err = r.regExpExec(rx, s); err != nil {
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
		return Int(index), nil
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
	// The copy is not made where making it would run nothing a script wrote,
	// and nothing but the search could see it: see splitDirect. A limit that
	// is an object could, as it is converted after the copy is made, so it
	// is left to the long way.
	if lv := arg(args, 1); (lv.IsUndefined() || lv.IsNumber()) && s.Len() > 0 {
		if fl, ok := r.splitDirect(rx); ok {
			limit, _ := r.splitLimit(lv)
			if limit == 0 {
				return Obj(r.newArrayFrom(nil)), nil
			}
			// The copy would have been the realm's RegExp's, whose matches
			// are the legacy statics' whatever rx's are.
			d := regexpData{re: rx.Object().dataOf().(*regexpData).re, realm: r.Realm}
			j := subjectOf(s)
			return r.splitBuiltin(rx, &d, &j, limit, fl&(regexp.FlagUnicode|regexp.FlagUnicodeSets) != 0)
		}
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

	limit, err := r.splitLimit(arg(args, 1))
	if err != nil {
		return Undefined, err
	}
	var out []Value
	if limit == 0 {
		return Obj(r.newArrayFrom(out)), nil
	}

	j := subjectOf(s)
	size := j.len()
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

	// A piece is cut from the subject rather than encoded again from its code
	// units, which for text that is all ASCII shares the bytes.
	piece := func(lo, hi int) Value { return Str(s.Substring(lo, hi)) }
	if r.builtinExec(splitter) {
		return r.splitBuiltin(splitter, splitter.Object().dataOf().(*regexpData), &j, limit, fullUnicode)
	}
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
			q = j.advance(q, fullUnicode)
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
			q = j.advance(q, fullUnicode)
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

// splitLimit is split's limit argument as the count of pieces it allows.
func (r *Runtime) splitLimit(lv Value) (int64, error) {
	if lv.IsUndefined() {
		return 1<<32 - 1, nil
	}
	n, err := r.toUint32(lv)
	return int64(n), err
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
	return r.regExpReplace(rx, args, 0, false)
}

// regExpReplace is RegExp.prototype[Symbol.replace], called directly where
// replaceDirect has found rx to be a RegExp that it may be: its flags are
// then fl, and its exec the built-in.
func (r *Runtime) regExpReplace(rx Value, args []Value, fl regexp.Flags, direct bool) (Value, error) {
	// The matches' indices are kept in the runtime's room for them, which
	// the replace has the use of until it returns.
	room := r.takeSpans()
	v, err := r.regExpReplaceIn(rx, args, fl, direct, &room)
	r.giveSpans(room)
	return v, err
}

// regExpReplaceIn is regExpReplace keeping its matches' indices in *room,
// which it leaves as it has grown it.
func (r *Runtime) regExpReplaceIn(rx Value, args []Value, fl regexp.Flags, direct bool, room *[]int) (Value, error) {
	if !rx.IsObject() {
		return Undefined, r.throwTypeError("RegExp.prototype[Symbol.replace] called on a non-object")
	}
	s, err := r.toString(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	j := subjectOf(s)
	size := j.len()

	replValue := arg(args, 1)
	functional := isCallable(replValue)
	template, tv := "", emptyString
	if !functional {
		if tv, err = r.toString(replValue); err != nil {
			return Undefined, err
		}
		template = tv.Go()
	}

	// A RegExp whose flags would be read through the built-in getters is
	// asked for them as bits, which is the same answer without the string.
	var global, fullUnicode bool
	builtin := direct
	if !direct {
		fl, builtin = r.builtinFlagBits(rx)
	}
	if builtin {
		global = fl&regexp.FlagGlobal != 0
		fullUnicode = fl&(regexp.FlagUnicode|regexp.FlagUnicodeSets) != 0
	} else {
		flags, err := r.regExpFlagsOf(rx)
		if err != nil {
			return Undefined, err
		}
		global = strings.ContainsRune(flags, 'g')
		fullUnicode = strings.ContainsAny(flags, "uv")
	}
	if global {
		if builtin {
			err = r.setLastIndex(rx.Object(), Int(0))
		} else {
			err = r.setValueProp(rx, atomLastIndex, Int(0), true)
		}
		if err != nil {
			return Undefined, err
		}
	} else {
		fullUnicode = false
	}

	// Every match is collected before any replacement runs, because a
	// replacement function may itself use the pattern.
	//
	// Where exec is the built-in, and nothing a script wrote can run until
	// the matching is done, the matches are kept as their indices rather than
	// as the arrays exec would have made of them, which only this reads.
	var results []Value
	spans := *room
	fast := direct || builtin && r.builtinExecOf(rx)
	if fast && global {
		var err error
		spans, err = r.execMatchAll(rx, s, fullUnicode, spans)
		*room = spans
		if err != nil {
			return Undefined, err
		}
	}
	for fast && !global {
		if err := r.tick(); err != nil {
			return Undefined, err
		}
		caps, err := r.execMatch(rx, s)
		if err != nil {
			return Undefined, err
		}
		if caps == nil {
			break
		}
		spans = append(spans, caps...)
		*room = spans
		if !global {
			break
		}
		if caps[0] == caps[1] {
			if err := r.advanceAfterEmptyMatch(rx, &j, fullUnicode); err != nil {
				return Undefined, err
			}
		}
	}
	for !fast {
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
			if err := r.advanceAfterEmptyMatch(rx, &j, fullUnicode); err != nil {
				return Undefined, err
			}
		}
	}

	// The result is built from pieces of the subject and the replacements
	// between them, either boundary of which may fall between the halves of
	// one character.
	sb := r.newParts()
	next := 0
	// The capture list and the callback's argument list are built once and
	// refilled for each match rather than allocated per match. A callee may
	// not keep what it was called with, any more than it may keep the
	// interpreter's own stack.
	var captures, callArgs, groupVals []Value
	count := len(results)
	width := 0
	// The built-in exec's matches of a pattern with named groups each have
	// a groups object, made as exec makes it.
	var gl *groupsLayout
	if fast {
		re := rx.Object().dataOf().(*regexpData).re
		width = 2 * (re.GroupCount() + 1)
		count = len(spans) / width
		gl = r.groupsLayoutOf(re)
	}
	// Nothing matched: the result is the subject, whole, which is the string
	// itself rather than a copy of it.
	if count == 0 {
		return Str(s), nil
	}
	// A template without a $ is the replacement whatever matched, so the
	// built-in exec's matches are not made into strings at all: only where
	// they are is read. The result is about as long as the subject.
	plain := fast && !functional && !strings.ContainsRune(template, '$')
	if plain && s.ascii && tv.ascii {
		if out := replacedShort(s, tv, spans, width, count); out != nil {
			return Str(out), nil
		}
	}
	// A template with a $ is expanded from where the built-in exec's
	// matches are, read once into its pieces: the text of each match and
	// group is written where it is in the subject, rather than made a string
	// to make the replacement of.
	var pieces []templatePiece
	var pieceRoom [16]templatePiece
	if fast && !functional && !plain {
		pieces = r.templatePieces(pieceRoom[:0], template, width/2-1, gl)
	}
	if plain || pieces != nil {
		if err := sb.Grow(len(s.Go())); err != nil {
			return Undefined, err
		}
	}
	for k := 0; k < count; k++ {
		if err := sb.check(); err != nil {
			return Undefined, err
		}
		if pieces != nil {
			caps := spans[k*width : (k+1)*width]
			sb.WriteString(j.slice(next, caps[0]))
			writeTemplate(&sb, &j, caps, pieces, template, gl)
			next = caps[1]
			continue
		}
		var matched *String
		var position, matchLen int
		named := Undefined
		captures = captures[:0]
		if plain {
			caps := spans[k*width : (k+1)*width]
			position, matchLen = caps[0], caps[1]-caps[0]
		} else if fast {
			caps := spans[k*width : (k+1)*width]
			matched, position = s.Substring(caps[0], caps[1]), caps[0]
			for i := 2; i < width; i += 2 {
				c := Undefined
				if caps[i] >= 0 && caps[i+1] >= 0 {
					c = Str(s.Substring(caps[i], caps[i+1]))
				}
				captures = append(captures, c)
			}
			if gl != nil {
				groupVals = append(append(groupVals[:0], Str(matched)), captures...)
				named = r.groupsObject(gl, groupVals)
			}
		} else {
			result := results[k]
			nCaps, err := r.resultCaptureCount(result)
			if err != nil {
				return Undefined, err
			}
			if matched, err = r.resultMatchString(result); err != nil {
				return Undefined, err
			}
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
			position = clampFloatIndex(posf, size)

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
			if named, err = r.getValueProp(result, atomGroups); err != nil {
				return Undefined, err
			}
		}
		if !plain {
			matchLen = matched.Len()
		}

		var replacement string
		if plain {
			replacement = template
		} else if functional {
			callArgs = append(callArgs[:0], Str(matched))
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
			replacement, err = r.getSubstitution(matched, &j, position,
				captures, named, template)
			if err != nil {
				return Undefined, err
			}
		}

		// Results are taken in order, so one claiming an index behind where the
		// last left off contributes its replacement and nothing else.
		if position >= next {
			if plain && s.ascii {
				// An ASCII subject's pieces have no surrogate halves at
				// their ends, and the template is the String it was.
				if position > next {
					sb.write(j.slice(next, position), false, false)
				}
				sb.WriteStr(tv)
			} else {
				sb.WriteString(j.slice(next, position))
				sb.WriteString(replacement)
			}
			next = position + matchLen
			if next > size {
				next = size
			}
		}
	}
	if next < size {
		sb.WriteString(j.slice(next, size))
	}
	if (plain || pieces != nil) && s.ascii && tv.ascii {
		// Made of ASCII alone, as long as its bytes, which overlong has
		// kept within a string's length.
		t := sb.String()
		return Str(&String{s: t, length: len(t), ascii: true}), nil
	}
	return r.builtString(sb.String())
}

// maxKeptSpans is the most indices the room a runtime keeps for a global
// replace's or match's matches holds on to after one: 8 KiB.
const maxKeptSpans = 1024

// takeSpans is the runtime's room for a global replace's or match's matches'
// indices, empty, which is the caller's until it gives it back (giveSpans):
// a replacement function's replace, which finds it taken, makes its own.
func (r *Runtime) takeSpans() []int {
	spans := r.spanBuf[:0]
	r.spanBuf = nil
	return spans
}

// giveSpans gives back the room takeSpans gave, as the caller's use of it has
// grown it, unless it has grown large: a replace over a long string with
// many matches would otherwise keep the room for them as long as the
// runtime lives.
func (r *Runtime) giveSpans(spans []int) {
	if cap(spans) <= maxKeptSpans {
		r.spanBuf = spans[:0]
	}
}

// replacedShort is the result of replacing the built-in exec's matches of an
// ASCII subject, spans of width each, by an ASCII template without a $, when
// it is short enough for a String and its bytes to be one allocation; or nil
// where it is not. The matches are in order and do not overlap, so its length
// is known before it is written.
func replacedShort(s, tv *String, spans []int, width, count int) *String {
	n := s.length + count*tv.length
	for k := 0; k < count; k++ {
		n -= spans[k*width+1] - spans[k*width]
	}
	if n > 72 {
		return nil
	}
	if n == 0 {
		return emptyString
	}
	src, t := s.Go(), tv.Go()
	out, b := newStringBytes(n)
	w, next := 0, 0
	for k := 0; k < count; k++ {
		w += copy(b[w:], src[next:spans[k*width]])
		w += copy(b[w:], t)
		next = spans[k*width+1]
	}
	copy(b[w:], src[next:])
	out.s = unsafe.String(unsafe.SliceData(b), n)
	out.length, out.ascii = n, true
	return out
}

// templatePiece is a piece of a replacement template, as templatePieces
// reads it: text of the template, or what a $ form names of the subject.
type templatePiece struct {
	kind templateKind
	// lo and hi are the bytes of the template text is, and lo the group a
	// group's piece is, 0 for the whole match.
	lo, hi int
}

type templateKind uint8

const (
	pieceText   templateKind = iota // template[lo:hi]
	pieceGroup                      // $&, $n and $nn: group lo
	pieceBefore                     // $`, what the subject has before the match
	pieceAfter                      // $', what it has after
	pieceNamed                      // $<name>: the groups gl.groups[lo] name
)

// templatePieces appends to pieces those of a template with a $, for a
// pattern of groups groups whose matches have the groups objects gl makes --
// or none where gl is nil, so that $< is text -- as getSubstitution reads it:
// what each piece is does not depend on the match, only on the pattern's
// groups and their names.
func (r *Runtime) templatePieces(pieces []templatePiece, template string, groups int, gl *groupsLayout) []templatePiece {
	for i := 0; i < len(template); i++ {
		if template[i] != '$' || i+1 >= len(template) {
			pieces = appendText(pieces, i, i+1)
			continue
		}
		switch c := template[i+1]; {
		case c == '$':
			pieces = appendText(pieces, i, i+1)
			i++
		case c == '&':
			pieces = append(pieces, templatePiece{kind: pieceGroup})
			i++
		case c == '`':
			pieces = append(pieces, templatePiece{kind: pieceBefore})
			i++
		case c == '\'':
			pieces = append(pieces, templatePiece{kind: pieceAfter})
			i++
		case c >= '0' && c <= '9':
			// Two digits are preferred when they name a real group, so $12
			// means group 12 if it exists and group 1 followed by "2" if not.
			idx := int(c - '0')
			consumed := 1
			if i+2 < len(template) && template[i+2] >= '0' && template[i+2] <= '9' {
				if two := idx*10 + int(template[i+2]-'0'); two >= 1 && two <= groups {
					idx, consumed = two, 2
				}
			}
			if idx < 1 || idx > groups {
				pieces = appendText(pieces, i, i+1)
				continue
			}
			pieces = append(pieces, templatePiece{kind: pieceGroup, lo: idx})
			i += consumed
		case c == '<' && gl != nil:
			// A name, read from the groups object: one it does not have is
			// undefined there, which is nothing. Without a > it is text.
			end := strings.IndexByte(template[i+2:], '>')
			if end < 0 {
				pieces = appendText(pieces, i, i+1)
				continue
			}
			name := r.atoms.intern(template[i+2 : i+2+end])
			for k, a := range gl.names {
				if a == name {
					pieces = append(pieces, templatePiece{kind: pieceNamed, lo: k})
					break
				}
			}
			i += 2 + end
		default:
			// $< among them, without a groups object: it names nothing.
			pieces = appendText(pieces, i, i+1)
		}
	}
	return pieces
}

// appendText appends the template's bytes lo to hi to pieces, as part of the
// text piece before them where they follow it.
func appendText(pieces []templatePiece, lo, hi int) []templatePiece {
	if n := len(pieces); n > 0 && pieces[n-1].kind == pieceText && pieces[n-1].hi == lo {
		pieces[n-1].hi = hi
		return pieces
	}
	return append(pieces, templatePiece{kind: pieceText, lo: lo, hi: hi})
}

// writeTemplate writes the replacement of the match caps of j by the
// template pieces were read from, for a pattern whose groups objects gl
// makes.
func writeTemplate(sb *partsBuilder, j *subject, caps []int, pieces []templatePiece, template string, gl *groupsLayout) {
	for _, p := range pieces {
		switch p.kind {
		case pieceText:
			sb.WriteString(template[p.lo:p.hi])
		case pieceGroup:
			// A group that took no part is undefined, which is nothing.
			if lo, hi := caps[2*p.lo], caps[2*p.lo+1]; lo >= 0 && hi >= 0 {
				sb.WriteString(j.slice(lo, hi))
			}
		case pieceBefore:
			sb.WriteString(j.slice(0, caps[0]))
		case pieceAfter:
			sb.WriteString(j.slice(min(caps[1], j.len()), j.len()))
		case pieceNamed:
			// The first of the groups the name is that took part.
			for _, g := range gl.groups[p.lo] {
				if lo, hi := caps[2*g], caps[2*g+1]; lo >= 0 && hi >= 0 {
					sb.WriteString(j.slice(lo, hi))
					break
				}
			}
		}
	}
}

// getSubstitution expands the $ forms in a replacement template.
func (r *Runtime) getSubstitution(matched *String, j *subject, position int,
	captures []Value, named Value, template string) (string, error) {
	if !strings.ContainsRune(template, '$') {
		return template, nil
	}
	tail := position + matched.Len()
	if tail > j.len() {
		tail = j.len()
	}

	// The pieces go into a buffer rather than a builder, because a piece may
	// begin with the half of a character the byte before it ends with -- which
	// WTF-8 spells as one sequence, and a string is its code units.
	buf := make([]byte, 0, len(template)+16)
	for i := 0; i < len(template); i++ {
		if template[i] != '$' || i+1 >= len(template) {
			buf = append(buf, template[i])
			continue
		}
		switch c := template[i+1]; {
		case c == '$':
			buf = append(buf, '$')
			i++
		case c == '&':
			buf = wtf8.AppendJoin(buf, matched.Go())
			i++
		case c == '`':
			buf = wtf8.AppendJoin(buf, j.slice(0, position))
			i++
		case c == '\'':
			buf = wtf8.AppendJoin(buf, j.slice(tail, j.len()))
			i++
		case c == '<':
			// A named group reference, which is only a reference at all when
			// the result carries groups; otherwise it is four literal
			// characters.
			if named.IsUndefined() {
				buf = append(buf, '$')
				continue
			}
			end := strings.IndexByte(template[i+2:], '>')
			if end < 0 {
				buf = append(buf, '$')
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
				buf = wtf8.AppendJoin(buf, vs.Go())
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
				buf = append(buf, '$')
				continue
			}
			if v := captures[idx-1]; !v.IsUndefined() {
				buf = wtf8.AppendJoin(buf, v.String().Go())
			}
			i += consumed
		default:
			buf = append(buf, '$')
		}
	}
	return string(buf), nil
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
	iter.extOf().data = &regExpStringIterData{
		matcher: matcher, s: s, j: subjectOf(s),
		global: global, fullUnicode: fullUnicode,
	}
	return Obj(iter)
}

// regExpStringIterData is where a matchAll iterator is in its subject.
type regExpStringIterData struct {
	matcher     Value
	s           *String
	j           subject
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
			d, _ = this.Object().dataOf().(*regExpStringIterData)
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
			if err := rt.advanceAfterEmptyMatch(d.matcher, &d.j, d.fullUnicode); err != nil {
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
