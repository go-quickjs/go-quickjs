package vm

import (
	"math"
	"strconv"
	"strings"

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
		for i, a := range args {
			n, err := rt.toUint32(a)
			if err != nil {
				return Undefined, err
			}
			units[i] = uint16(n)
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
		parts, err := rt.arrayToSlice(rawVal)
		if err != nil {
			return Undefined, err
		}
		var sb strings.Builder
		for i, part := range parts {
			s, err := rt.toString(part)
			if err != nil {
				return Undefined, err
			}
			sb.WriteString(s.Go())
			if i+1 < len(parts) && i+1 < len(args) {
				sub, err := rt.toString(args[i+1])
				if err != nil {
					return Undefined, err
				}
				sb.WriteString(sub.Go())
			}
		}
		return Str(NewString(sb.String())), nil
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

	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		return Str(s), nil
	})
	r.defMethod(p, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
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
		return Str(s.Substring(int(i), int(i)+1)), nil
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
		return Str(s.Substring(int(i), int(i)+1)), nil
	})

	r.defMethod(p, "indexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, needle, err := rt.strAndSearch(thisStr, this, args)
		if err != nil {
			return Undefined, err
		}
		from := 0
		if len(args) > 1 {
			n, err := rt.toInteger(args[1])
			if err != nil {
				return Undefined, err
			}
			from = int(n)
		}
		return Int(s.IndexOf(needle, from)), nil
	})

	r.defMethod(p, "lastIndexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, needle, err := rt.strAndSearch(thisStr, this, args)
		if err != nil {
			return Undefined, err
		}
		best := -1
		for i := 0; ; {
			at := s.IndexOf(needle, i)
			if at < 0 {
				break
			}
			best = at
			i = at + 1
		}
		return Int(best), nil
	})

	r.defMethod(p, "includes", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, needle, err := rt.strAndSearch(thisStr, this, args)
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
		s, needle, err := rt.strAndSearch(thisStr, this, args)
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
		s, needle, err := rt.strAndSearch(thisStr, this, args)
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
		return Str(NewString(strings.ToUpper(s.Go()))), nil
	})
	r.defMethod(p, "toLowerCase", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(strings.ToLower(s.Go()))), nil
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
			s = s.Concat(o)
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
		if float64(s.Len())*n > 1<<30 {
			return Undefined, rt.throwRangeError("repeat count is too large")
		}
		return Str(NewString(strings.Repeat(s.Go(), int(n)))), nil
	})

	r.defMethod(p, "split", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if this.IsNullish() {
			return Undefined, rt.throwTypeError(
				"String.prototype.split called on %s", this.Kind())
		}
		// The separator is asked for its symbol method first, so anything can
		// act as one.
		if sep := arg(args, 0); !sep.IsNullish() {
			m, err := rt.getValueProp(sep, rt.atoms.internSymbol(rt.wellKnown.split))
			if err != nil {
				return Undefined, err
			}
			if !m.IsNullish() {
				if !isCallable(m) {
					return Undefined, rt.throwTypeError(
						"the separator's split method is not callable")
				}
				return rt.call(m, sep, []Value{this, arg(args, 1)})
			}
		}
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		sepVal := arg(args, 0)
		if sepVal.IsUndefined() {
			return Obj(rt.newArrayFrom([]Value{Str(s)})), nil
		}
		limit := math.MaxInt32
		if lv := arg(args, 1); !lv.IsUndefined() {
			n, err := rt.toUint32(lv)
			if err != nil {
				return Undefined, err
			}
			limit = int(n)
		}
		sep, err := rt.toString(sepVal)
		if err != nil {
			return Undefined, err
		}
		var out []Value
		if sep.Len() == 0 {
			// An empty separator splits into individual code units.
			for i := 0; i < s.Len() && len(out) < limit; i++ {
				out = append(out, Str(s.Substring(i, i+1)))
			}
			return Obj(rt.newArrayFrom(out)), nil
		}
		for _, part := range strings.Split(s.Go(), sep.Go()) {
			if len(out) >= limit {
				break
			}
			out = append(out, Str(NewString(part)))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})

	r.defMethod(p, "padStart", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.padString(thisStr, this, args, true)
	})
	r.defMethod(p, "padEnd", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
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
	pad := " "
	if pv := arg(args, 1); !pv.IsUndefined() {
		ps, err := r.toString(pv)
		if err != nil {
			return Undefined, err
		}
		pad = ps.Go()
	}
	if pad == "" {
		return Str(s), nil
	}
	need := int(target) - s.Len()
	filler := NewString(strings.Repeat(pad, need/len([]rune(pad))+1))
	filler = filler.Substring(0, need)
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
		m, err := r.getValueProp(pat, r.atoms.internSymbol(r.wellKnown.replace))
		if err != nil {
			return Undefined, err
		}
		if !m.IsNullish() {
			if !isCallable(m) {
				return Undefined, r.throwTypeError("the pattern's replace method is not callable")
			}
			return r.call(m, pat, []Value{this, arg(args, 1)})
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

	src := s.Go()
	pat := pattern.Go()
	if pat == "" && !all {
		// An empty pattern matches at the start.
		repl, err := r.replacementFor(replVal, pattern, 0, s)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(repl + src)), nil
	}

	var sb strings.Builder
	pos := 0
	for {
		i := strings.Index(src[pos:], pat)
		if i < 0 {
			break
		}
		i += pos
		sb.WriteString(src[pos:i])
		repl, err := r.replacementFor(replVal, pattern, i, s)
		if err != nil {
			return Undefined, err
		}
		sb.WriteString(repl)
		pos = i + len(pat)
		if !all {
			break
		}
		if len(pat) == 0 {
			// Avoid looping forever on an empty pattern.
			if pos >= len(src) {
				break
			}
			sb.WriteByte(src[pos])
			pos++
		}
	}
	sb.WriteString(src[pos:])
	return Str(NewString(sb.String())), nil
}

// replacementFor produces the text a single match is replaced with, calling the
// replacement function when one was supplied.
func (r *Runtime) replacementFor(replVal Value, matched *String, offset int, whole *String) (string, error) {
	if isCallable(replVal) {
		res, err := r.call(replVal, Undefined, []Value{
			Str(matched), Int(offset), Str(whole),
		})
		if err != nil {
			return "", err
		}
		s, err := r.toString(res)
		if err != nil {
			return "", err
		}
		return s.Go(), nil
	}
	s, err := r.toString(replVal)
	if err != nil {
		return "", err
	}
	// $& in the replacement stands for the matched text.
	return strings.ReplaceAll(s.Go(), "$&", matched.Go()), nil
}

// newStringIterator iterates a string by code point.
func (r *Runtime) newStringIterator(s *String) (Value, error) {
	i := 0
	iter := newObject(r.proto.stringIter, ClassIterator)
	r.defMethod(iter, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		res := newObject(rt.proto.object, ClassObject)
		if i >= s.Len() {
			res.setOwnRaw(atomValue, Undefined, propDefault)
			res.setOwnRaw(atomDone, True, propDefault)
			return Obj(res), nil
		}
		// A surrogate pair is one code point and advances by two units.
		width := 1
		if cp := s.CodePointAt(i); cp > 0xFFFF {
			width = 2
		}
		v := s.Substring(i, i+width)
		i += width
		res.setOwnRaw(atomValue, Str(v), propDefault)
		res.setOwnRaw(atomDone, False, propDefault)
		return Obj(res), nil
	})
	r.defSymbolMethod(iter, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return this, nil
		})
	return Obj(iter), nil
}

// jsWhitespace is the set of code points the trim methods remove.
const jsWhitespace = " \t\n\v\f\r" +
	"\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007" +
	"\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

// formatFixed implements Number.prototype.toFixed.
func formatFixed(n float64, digits int) string {
	return strconv.FormatFloat(n, 'f', digits, 64)
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
