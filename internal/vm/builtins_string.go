package vm

import (
	"math"
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

func (r *Runtime) initStringBuiltins() {
	p := r.proto.str

	ctor := r.newCtor("String", 1, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s := emptyString
		if len(args) > 0 {
			// String(sym) is the only conversion that accepts a symbol; every
			// implicit one throws. Under new, a symbol is still rejected.
			if args[0].IsSymbol() && !rt.Constructing() {
				return Str(NewString(args[0].Symbol().String())), nil
			}
			v, err := rt.toString(args[0])
			if err != nil {
				return Undefined, err
			}
			s = v
		}
		if !rt.Constructing() {
			return Str(s), nil
		}
		o := newObject(rt.proto.str, ClassStringWrapper)
		o.data = s
		return Obj(o), nil
	})

	r.defMethod(ctor, "fromCharCode", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// Each argument becomes one UTF-16 code unit, so this is the usual way
		// a script produces a lone surrogate.
		units := make([]uint16, len(args))
		ascii := true
		for i, a := range args {
			n, err := rt.toUint32(a)
			if err != nil {
				return Undefined, err
			}
			units[i] = uint16(n)
			ascii = ascii && uint16(n) < 128
		}
		if ascii && len(units) == 1 {
			return Str(rt.asciiCharString(byte(units[0]))), nil
		}
		return Str(fromUnits(units)), nil
	})

	r.defMethod(ctor, "fromCodePoint", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		var units []uint16
		for _, a := range args {
			n, err := rt.toNumber(a)
			if err != nil {
				return Undefined, err
			}
			if n != math.Trunc(n) || n < 0 || n > 0x10FFFF {
				return Undefined, rt.throwRangeError("invalid code point %v", n)
			}
			cp := rune(n)
			if cp > 0xFFFF {
				cp -= 0x10000
				units = append(units, uint16(0xD800+(cp>>10)), uint16(0xDC00+(cp&0x3FF)))
				continue
			}
			units = append(units, uint16(cp))
		}
		return Str(fromUnits(units)), nil
	})

	r.defMethod(ctor, "raw", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		strsVal := arg(args, 0)
		rawVal, err := rt.getValueProp(strsVal, atomRaw)
		if err != nil {
			return Undefined, err
		}
		raw, err := rt.viewArrayLike(rawVal)
		if err != nil {
			return Undefined, err
		}
		// A raw piece and the substitution after it may end and begin with the
		// halves of one character, which the builder joins. The pieces are
		// read as they are written, as the standard has it: listed first, a
		// length of billions was billions of values before the string was
		// found too long.
		var sb partsBuilder
		for i := int64(0); i < raw.n; i++ {
			if err := rt.tick(); err != nil {
				return Undefined, err
			}
			part, err := raw.get(rt, i)
			if err != nil {
				return Undefined, err
			}
			s, err := rt.toString(part)
			if err != nil {
				return Undefined, err
			}
			sb.WriteString(s.Go())
			if i+1 < raw.n && i+1 < int64(len(args)) {
				sub, err := rt.toString(args[i+1])
				if err != nil {
					return Undefined, err
				}
				sb.WriteString(sub.Go())
			}
			if sb.overlong() {
				return Undefined, rt.throwStringLength()
			}
		}
		return rt.builtString(sb.String())
	})

	// The receiver of a String method may be a primitive or a wrapper.
	thisStr := func(rt *Runtime, this Value) (*String, error) {
		switch {
		case this.IsString():
			return this.String(), nil
		case this.IsObject() && this.Object().class == ClassStringWrapper:
			if s, ok := this.Object().data.(*String); ok {
				return s, nil
			}
		case this.IsNullish():
			return nil, rt.throwTypeError("String.prototype method called on %s", this.Kind())
		}
		return rt.toString(this)
	}

	// Unlike the rest, these two are not generic: they hand back the string a
	// receiver already is or holds, and there is nothing to hand back for a
	// receiver that is neither.
	thisStringValue := func(rt *Runtime, this Value) (*String, error) {
		switch {
		case this.IsString():
			return this.String(), nil
		case this.IsObject() && this.Object().class == ClassStringWrapper:
			if s, ok := this.Object().data.(*String); ok {
				return s, nil
			}
		}
		return nil, rt.throwTypeError("not a string")
	}
	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStringValue(rt, this)
		if err != nil {
			return Undefined, err
		}
		return Str(s), nil
	})
	r.defMethod(p, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStringValue(rt, this)
		if err != nil {
			return Undefined, err
		}
		return Str(s), nil
	})

	r.defMethod(p, "charAt", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		i, err := rt.toInteger(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if i < 0 || i >= float64(s.Len()) {
			return Str(emptyString), nil
		}
		return Str(rt.unitString(s, int(i))), nil
	})

	r.defMethod(p, "charCodeAt", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		i, err := rt.toInteger(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		c := -1
		if i >= 0 && i < float64(s.Len()) {
			c = s.CharCodeAt(int(i))
		}
		if c < 0 {
			return Float(nan()), nil
		}
		return Int(c), nil
	})

	r.defMethod(p, "codePointAt", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		i, err := rt.toInteger(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if i < 0 || i >= float64(s.Len()) {
			return Undefined, nil
		}
		return Int(int(s.CodePointAt(int(i)))), nil
	})

	r.defMethod(p, "at", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		i, err := rt.toInteger(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if i < 0 {
			i += float64(s.Len())
		}
		if i < 0 || i >= float64(s.Len()) {
			return Undefined, nil
		}
		return Str(rt.unitString(s, int(i))), nil
	})

	r.defMethod(p, "indexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, needle, err := rt.strAndSearch(thisStr, this, args)
		if err != nil {
			return Undefined, err
		}
		// The position is narrowed while it is still a number: an infinity
		// has no int to convert to, and converting one wraps rather than
		// saturates, which put a search past the end back at the start.
		from, err := rt.clampedPosition(arg(args, 1), s.Len())
		if err != nil {
			return Undefined, err
		}
		return Int(s.IndexOf(needle, from)), nil
	})

	r.defMethod(p, "lastIndexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, needle, err := rt.strAndSearch(thisStr, this, args)
		if err != nil {
			return Undefined, err
		}
		// The starting point is converted after the search string, and NaN --
		// which is what undefined becomes -- means the end of the string
		// rather than the beginning.
		end := s.Len()
		if len(args) > 1 {
			n, err := rt.toNumber(args[1])
			if err != nil {
				return Undefined, err
			}
			if !math.IsNaN(n) {
				switch {
				case n < 0:
					end = 0
				case n < float64(end):
					end = int(n)
				}
			}
		}
		return Int(s.LastIndexOf(needle, end)), nil
	})

	r.defMethod(p, "includes", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, needle, err := rt.strAndPattern(thisStr, this, args, "includes")
		if err != nil {
			return Undefined, err
		}
		from, err := rt.clampedPosition(arg(args, 1), s.Len())
		if err != nil {
			return Undefined, err
		}
		return Bool(s.IndexOf(needle, from) >= 0), nil
	})

	r.defMethod(p, "startsWith", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, needle, err := rt.strAndPattern(thisStr, this, args, "startsWith")
		if err != nil {
			return Undefined, err
		}
		from, err := rt.clampedPosition(arg(args, 1), s.Len())
		if err != nil {
			return Undefined, err
		}
		if from+needle.Len() > s.Len() {
			return False, nil
		}
		return Bool(s.Substring(from, from+needle.Len()).Equals(needle)), nil
	})

	r.defMethod(p, "endsWith", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, needle, err := rt.strAndPattern(thisStr, this, args, "endsWith")
		if err != nil {
			return Undefined, err
		}
		// The position names where the match must end rather than begin, and
		// undefined means the end of the string rather than zero.
		end := s.Len()
		if ev := arg(args, 1); !ev.IsUndefined() {
			var err error
			if end, err = rt.clampedPosition(ev, s.Len()); err != nil {
				return Undefined, err
			}
		}
		start := end - needle.Len()
		if start < 0 {
			return False, nil
		}
		return Bool(s.Substring(start, end).Equals(needle)), nil
	})

	r.defMethod(p, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		n := s.Len()
		start, err := rt.relativeIndex(arg(args, 0), n, 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.relativeIndex(arg(args, 1), n, n)
		if err != nil {
			return Undefined, err
		}
		if start >= end {
			return Str(emptyString), nil
		}
		return Str(s.Substring(start, end)), nil
	})

	r.defMethod(p, "substring", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		n := s.Len()
		start, err := rt.clampIndex(arg(args, 0), n, 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.clampIndex(arg(args, 1), n, n)
		if err != nil {
			return Undefined, err
		}
		// substring swaps its arguments when they are out of order, unlike
		// slice, which returns empty.
		if start > end {
			start, end = end, start
		}
		return Str(s.Substring(start, end)), nil
	})

	r.defMethod(p, "toUpperCase", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		if s.ascii {
			return Str(asciiCase(s, true)), nil
		}
		return Str(NewString(caseConvert(s.Go(), true))), nil
	})
	r.defMethod(p, "toLowerCase", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		if s.ascii {
			return Str(asciiCase(s, false)), nil
		}
		return Str(NewString(caseConvert(s.Go(), false))), nil
	})

	r.defMethod(p, "trim", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(strings.Trim(s.Go(), jsWhitespace))), nil
	})
	r.defMethod(p, "trimStart", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(strings.TrimLeft(s.Go(), jsWhitespace))), nil
	})
	r.defMethod(p, "trimEnd", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(strings.TrimRight(s.Go(), jsWhitespace))), nil
	})

	r.defMethod(p, "concat", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		for _, a := range args {
			o, err := rt.toString(a)
			if err != nil {
				return Undefined, err
			}
			if s, err = rt.concat(s, o); err != nil {
				return Undefined, err
			}
		}
		return Str(s), nil
	})

	r.defMethod(p, "repeat", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		n, err := rt.toInteger(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if n < 0 || math.IsInf(n, 0) {
			return Undefined, rt.throwRangeError("invalid repeat count")
		}
		if n == 0 || s.Len() == 0 {
			return Str(emptyString), nil
		}
		// Guard against a count that would exhaust memory before building it.
		if float64(s.Len())*n > maxStringLength {
			return Undefined, rt.throwStringLength()
		}
		if err := rt.reserveMemory(stringBytes(s) * int(n)); err != nil {
			return Undefined, err
		}
		if s.endsHigh && s.startsLow {
			// Each copy's high half meets the next one's low half, which
			// together are one character, spelled as one.
			var sb partsBuilder
			for i := 0; i < int(n); i++ {
				sb.WriteString(s.Go())
			}
			return rt.builtString(sb.String())
		}
		return Str(NewString(strings.Repeat(s.Go(), int(n)))), nil
	})

	r.defMethod(p, "split", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if this.IsNullish() {
			return Undefined, rt.throwTypeError(
				"String.prototype.split called on %s", this.Kind())
		}
		// An object separator is asked for its symbol method first, so anything
		// can act as one. A primitive is not asked: it would find one on its
		// own prototype, which is not a separator the caller supplied.
		if sep := arg(args, 0); sep.IsObject() {
			m, err := rt.getValueProp(sep, rt.atoms.internSymbol(rt.wellKnown.split))
			if err != nil {
				return Undefined, err
			}
			if !m.IsNullish() {
				if !isCallable(m) {
					return Undefined, rt.throwTypeError(
						"the separator's split method is not callable")
				}
				return rt.call2(m, sep, this, arg(args, 1))
			}
		}
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		// The limit is settled before the separator is looked at, so a limit of
		// zero produces an empty array whatever the separator is -- including
		// none at all, which otherwise yields the whole string.
		limit := math.MaxInt32
		if lv := arg(args, 1); !lv.IsUndefined() {
			n, err := rt.toUint32(lv)
			if err != nil {
				return Undefined, err
			}
			// No string splits into more pieces than an int32 counts, and
			// an int may be no wider.
			limit = int(min(n, math.MaxInt32))
		}
		// The separator is converted before the limit is looked at, so a
		// toString that throws is heard even for a limit of zero. Undefined is
		// the exception in both directions: converting it has no effect, and
		// with a limit above zero it yields the whole string rather than
		// splitting on "undefined".
		sepVal := arg(args, 0)
		var sep *String
		if !sepVal.IsUndefined() {
			sep, err = rt.toString(sepVal)
			if err != nil {
				return Undefined, err
			}
		}
		if limit == 0 {
			return Obj(rt.newArrayFrom(nil)), nil
		}
		if sepVal.IsUndefined() {
			return Obj(rt.newArrayFrom([]Value{Str(s)})), nil
		}
		// The pieces are collected in the runtime's buffer and copied into
		// the array once there are all of them: nothing in the walk can run
		// a script, which might split as well.
		out := rt.splitBuf[:0]
		if sep.Len() == 0 {
			// An empty separator splits into individual code units.
			for i := 0; i < s.Len() && len(out) < limit; i++ {
				out = append(out, Str(rt.unitString(s, i)))
			}
			return Obj(rt.splitArray(out)), nil
		}
		// The search is over code units rather than bytes: a separator that is
		// half of a surrogate pair matches inside the pair, which shares no
		// bytes with the half on its own.
		for pos := 0; ; {
			if len(out) >= limit {
				break
			}
			i := s.IndexOf(sep, pos)
			if i < 0 {
				out = append(out, Str(s.Substring(pos, s.Len())))
				break
			}
			out = append(out, Str(s.Substring(pos, i)))
			pos = i + sep.Len()
		}
		return Obj(rt.splitArray(out)), nil
	})

	// One argument each: the padding string is optional, and a function's
	// length counts only what it requires.
	r.defMethod(p, "padStart", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.padString(thisStr, this, args, true)
	})
	r.defMethod(p, "padEnd", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.padString(thisStr, this, args, false)
	})

	r.defMethod(p, "replace", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.stringReplace(thisStr, this, args, false)
	})
	r.defMethod(p, "replaceAll", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.stringReplace(thisStr, this, args, true)
	})

	r.defMethod(p, "isWellFormed", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		return Bool(wtf8.WellFormed(s.Go())), nil
	})
	r.defMethod(p, "toWellFormed", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(wtf8.ToWellFormed(s.Go()))), nil
	})

	// Strings are iterable by code point, not by code unit, so a surrogate
	// pair yields one element.
	r.defSymbolMethod(p, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			s, err := thisStr(rt, this)
			if err != nil {
				return Undefined, err
			}
			return rt.newStringIterator(s)
		})
}

// thisStrFunc is the receiver-unwrapping helper shared by the String methods.
type thisStrFunc func(*Runtime, Value) (*String, error)

// strAndSearch resolves the receiver and the first argument as strings, which
// the search methods all need.
func (r *Runtime) strAndSearch(thisStr thisStrFunc, this Value, args []Value) (*String, *String, error) {
	s, err := thisStr(r, this)
	if err != nil {
		return nil, nil, err
	}
	needle, err := r.toString(arg(args, 0))
	if err != nil {
		return nil, nil, err
	}
	return s, needle, nil
}

// strAndPattern is strAndSearch for the three methods that refuse a regular
// expression outright.
//
// `"a".includes(/a/)` is a TypeError rather than a search for "/a/": these
// three take text, and a pattern given to one is a mistake. What counts as a
// regular expression is what Symbol.match says, which is a read that may throw.
func (r *Runtime) strAndPattern(thisStr thisStrFunc, this Value, args []Value,
	name string) (*String, *String, error) {
	s, err := thisStr(r, this)
	if err != nil {
		return nil, nil, err
	}
	isRe, err := r.isRegExp(arg(args, 0))
	if err != nil {
		return nil, nil, err
	}
	if isRe {
		return nil, nil, r.throwTypeError(
			"String.prototype.%s takes a string, not a regular expression", name)
	}
	needle, err := r.toString(arg(args, 0))
	if err != nil {
		return nil, nil, err
	}
	return s, needle, nil
}

// clampIndex converts an index argument, clamping rather than treating a
// negative value as an offset from the end.
func (r *Runtime) clampIndex(v Value, length, def int) (int, error) {
	if v.IsUndefined() {
		return def, nil
	}
	n, err := r.toInteger(v)
	if err != nil {
		return 0, err
	}
	switch {
	case n < 0 || math.IsNaN(n):
		return 0, nil
	case n > float64(length):
		return length, nil
	}
	return int(n), nil
}

func (r *Runtime) padString(thisStr thisStrFunc, this Value, args []Value, atStart bool) (Value, error) {
	s, err := thisStr(r, this)
	if err != nil {
		return Undefined, err
	}
	target, err := r.toInteger(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	if target <= float64(s.Len()) {
		return Str(s), nil
	}
	if target > maxStringLength {
		return Undefined, r.throwStringLength()
	}
	pad := NewString(" ")
	if pv := arg(args, 1); !pv.IsUndefined() {
		ps, err := r.toString(pv)
		if err != nil {
			return Undefined, err
		}
		pad = ps
	}
	if pad.Len() == 0 {
		return Str(s), nil
	}
	need := int(target) - s.Len()
	if err := r.reserveMemory(need * 3); err != nil {
		return Undefined, err
	}
	// The filler is counted in code units, as the result is: in runes a lone
	// surrogate is three, and the result came out short. It is built with
	// the halves of a pair joined where one copy meets the next.
	var sb partsBuilder
	for i, copies := 0, need/pad.Len()+1; i < copies; i++ {
		sb.WriteString(pad.Go())
	}
	filler := NewString(sb.String()).Substring(0, need)
	if atStart {
		return Str(filler.Concat(s)), nil
	}
	return Str(s.Concat(filler)), nil
}

// stringReplace implements replace and replaceAll for a string pattern.
func (r *Runtime) stringReplace(thisStr thisStrFunc, this Value, args []Value, all bool) (Value, error) {
	if this.IsNullish() {
		return Undefined, r.throwTypeError("String.prototype.replace called on %s", this.Kind())
	}
	// A string replaced in a string, by a RegExp whose replace is the
	// built-in, is replaced by the built-in directly; see replaceDirect.
	if pat := arg(args, 0); !all && this.IsString() && arg(args, 1).IsString() {
		if fl, ok := r.replaceDirect(pat); ok {
			return r.regExpReplace(pat, []Value{this, args[1]}, fl, true)
		}
	}
	// The pattern is asked for its symbol method first, so that a subclass --
	// or anything else -- can define how it replaces. Only an object is asked:
	// a primitive cannot carry the method itself, and reaching through to its
	// wrapper prototype would let a change there rewrite every string replace
	// in the program.
	if pat := arg(args, 0); pat.IsObject() {
		// replaceAll additionally insists on the global flag, since replacing
		// once would silently do the wrong thing.
		if all {
			isRe, err := r.isRegExp(pat)
			if err != nil {
				return Undefined, err
			}
			if isRe {
				flags, err := r.getValueProp(pat, atomFlags)
				if err != nil {
					return Undefined, err
				}
				if flags.IsNullish() {
					return Undefined, r.throwTypeError("the pattern has no flags")
				}
				fs, err := r.toString(flags)
				if err != nil {
					return Undefined, err
				}
				if !strings.Contains(fs.Go(), "g") {
					return Undefined, r.throwTypeError(
						"replaceAll requires a global regular expression")
				}
			}
		}
		var m Value
		if pat.IsObject() {
			var err error
			m, err = r.getValueProp(pat, r.atoms.internSymbol(r.wellKnown.replace))
			if err != nil {
				return Undefined, err
			}
		}
		if !m.IsNullish() {
			if !isCallable(m) {
				return Undefined, r.throwTypeError("the pattern's replace method is not callable")
			}
			return r.call2(m, pat, this, arg(args, 1))
		}
	}
	s, err := thisStr(r, this)
	if err != nil {
		return Undefined, err
	}
	pattern, err := r.toString(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	replVal := arg(args, 1)
	// A replacement that is not a function is converted once, before the
	// search: a toString that counts its calls sees exactly one, however many
	// matches there turn out to be -- including none at all.
	replText := ""
	if !isCallable(replVal) {
		rs, err := r.toString(replVal)
		if err != nil {
			return Undefined, err
		}
		replText = rs.Go()
		replVal = Undefined
	}

	// Everything here counts in code units rather than bytes: the position a
	// replacement function is given, and the text `$\'` and "$`" stand for,
	// are indices into the string as a script sees it.
	patLen := pattern.Len()
	// One list for the replacement function's arguments, refilled per match.
	var argv [3]Value
	if patLen == 0 && !all {
		// An empty pattern matches at the start.
		repl, err := r.replacementFor(replVal, replText, pattern, 0, s, &argv)
		if err != nil {
			return Undefined, err
		}
		out, err := r.concat(NewString(repl), s)
		if err != nil {
			return Undefined, err
		}
		return Str(out), nil
	}

	// The result goes into one buffer: joined one piece at a time, a string
	// with a million matches would be a million rope nodes.
	var sb partsBuilder
	pos := 0
	for {
		i := s.IndexOf(pattern, pos)
		if i < 0 {
			break
		}
		sb.WriteString(s.Substring(pos, i).Go())
		repl, err := r.replacementFor(replVal, replText, pattern, i, s, &argv)
		if err != nil {
			return Undefined, err
		}
		sb.WriteString(repl)
		if sb.overlong() {
			return Undefined, r.throwStringLength()
		}
		pos = i + patLen
		if !all {
			break
		}
		if patLen == 0 {
			// Avoid looping forever on an empty pattern.
			if pos >= s.Len() {
				break
			}
			sb.WriteString(s.Substring(pos, pos+1).Go())
			pos++
		}
		if err := r.tick(); err != nil {
			return Undefined, err
		}
	}
	sb.WriteString(s.Substring(pos, s.Len()).Go())
	return r.builtString(sb.String())
}

// replacementFor produces the text a single match is replaced with, calling the
// replacement function when one was supplied.
func (r *Runtime) replacementFor(replVal Value, replText string, matched *String,
	offset int, whole *String, argv *[3]Value) (string, error) {
	if isCallable(replVal) {
		argv[0], argv[1], argv[2] = Str(matched), Int(offset), Str(whole)
		res, err := r.call(replVal, Undefined, argv[:])
		if err != nil {
			return "", err
		}
		s, err := r.toString(res)
		if err != nil {
			return "", err
		}
		return s.Go(), nil
	}
	// The replacement may name the match and the text around it, the same way
	// it may for a regular expression -- there are simply no capture groups.
	return r.getSubstitution(matched, whole.codeUnits(), offset,
		nil, Undefined, replText)
}

// newStringIterator iterates a string by code point.
func (r *Runtime) newStringIterator(s *String) (Value, error) {
	iter := newObject(r.proto.stringIter, ClassIterator)
	iter.data = &stringIterData{s: s}
	return Obj(iter), nil
}

// stringIterData is where a string iterator is in its string.
type stringIterData struct {
	s *String
	i int
}

// initStringIteratorProto fills in %StringIteratorPrototype%, whose next method
// every string iterator shares.
func (r *Runtime) initStringIteratorProto() {
	p := r.proto.stringIter
	r.defMethod(p, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		var d *stringIterData
		if this.IsObject() {
			d, _ = this.Object().data.(*stringIterData)
		}
		if d == nil {
			return Undefined, rt.throwTypeError(
				"String Iterator.prototype.next called on an incompatible receiver")
		}
		if d.i >= d.s.Len() {
			return Obj(rt.iterResult(Undefined, true)), nil
		}
		// A surrogate pair is one code point and advances by two units.
		width := 1
		if cp := d.s.CodePointAt(d.i); cp > 0xFFFF {
			width = 2
		}
		v := d.s.Substring(d.i, d.i+width)
		d.i += width
		return Obj(rt.iterResult(Str(v), false)), nil
	})
	p.setOwnRaw(r.atoms.internSymbol(r.wellKnown.toStringTag),
		Str(NewString("String Iterator")), propConfigurable)
}

// jsWhitespace is the set of code points the trim methods remove.
const jsWhitespace = " \t\n\v\f\r" +
	"\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007" +
	"\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

// formatFixed implements Number.prototype.toFixed.
//
// A magnitude of 10**21 or more is written the way ToString writes it, since
// beyond that a double has no digits left to place after the point. Below it
// the rounding takes the larger value on a tie, which is what the
// specification asks for and not what formatting a float would do.
func formatFixed(n float64, digits int) string {
	return string(appendFixed(nil, n, digits))
}

// appendFixed is formatFixed appending to dst, which lets toFixed write the
// digits where the string will keep them.
func appendFixed(dst []byte, n float64, digits int) []byte {
	if math.Abs(n) >= 1e21 {
		return jsnum.AppendFloat(dst, n)
	}
	if n < 0 {
		dst, n = append(dst, '-'), -n
	}
	// strconv rounds the exact value correctly, but a tie to even. A tie is
	// an expansion that ends one place past the last digit kept, with a 5,
	// and the expansion of m*2**p ends -p places after the point -- so only
	// that one case needs the digits taken by hand. -0 has no sign here.
	if _, p := binaryParts(n); n == 0 || -p != digits+1 {
		return strconv.AppendFloat(dst, math.Abs(n), 'f', digits, 64)
	}
	return append(dst, formatFixedExact(n, digits)...)
}

// formatFixedExact is formatFixed of a non-negative number from all its
// digits, which is where a tie is broken upward.
func formatFixedExact(n float64, digits int) string {
	d, e := exactDecimal(n)
	if n == 0 {
		d, e = "0", 0
	}
	// e is the exponent of the first digit, so e+1+digits of them are kept.
	keep := e + 1 + digits
	if keep <= 0 {
		// Everything rounds away unless the first dropped digit carries, which
		// leaves one unit in the last place.
		if keep == 0 && d[0] >= '5' {
			return withPoint(strings.Repeat("0", digits)+"1", digits)
		}
		return withPoint(strings.Repeat("0", digits+1), digits)
	}
	rounded, carried := roundSignificant(d, keep)
	if carried {
		e++
	}
	// Pad out to e+1 integer digits and then the fractional ones.
	intDigits := e + 1
	if intDigits < 1 {
		rounded = strings.Repeat("0", 1-intDigits) + rounded
		intDigits = 1
	}
	for len(rounded) < intDigits+digits {
		rounded += "0"
	}
	return withPoint(rounded[:intDigits+digits], digits)
}

// withPoint puts the decimal point digits places from the right.
func withPoint(s string, digits int) string {
	if digits == 0 {
		return s
	}
	return s[:len(s)-digits] + "." + s[len(s)-digits:]
}

// splitArray makes split's array of the pieces collected in the runtime's
// buffer, and gives the buffer back for the next split -- unless it grew
// large, which would keep that much memory for as long as the runtime.
func (r *Runtime) splitArray(out []Value) *Object {
	a := r.newArrayFrom(out)
	clear(out)
	if cap(out) <= 1024 {
		r.splitBuf = out[:0]
	} else {
		r.splitBuf = nil
	}
	return a
}

// clampedPosition narrows a position argument into a string, which is what
// makes "word".includes("w", 5) false rather than a search from somewhere
// outside the string.
func (r *Runtime) clampedPosition(v Value, length int) (int, error) {
	n, err := r.toInteger(v)
	if err != nil {
		return 0, err
	}
	return clampFloatIndex(n, length), nil
}

// isRegExp implements the IsRegExp abstract operation.
//
// It asks for Symbol.match rather than checking the class, so that an object
// can present itself as a pattern -- or a real RegExp can disclaim being one.
func (r *Runtime) isRegExp(v Value) (bool, error) {
	if !v.IsObject() {
		return false, nil
	}
	m, err := r.getValueProp(v, r.atoms.internSymbol(r.wellKnown.match))
	if err != nil {
		return false, err
	}
	if !m.IsUndefined() {
		return m.Truthy(), nil
	}
	return v.Object().class == ClassRegExp, nil
}
