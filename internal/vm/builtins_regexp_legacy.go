package vm

import "unsafe"

// RegExp's legacy static properties -- RegExp.$1 to $9, input, lastMatch,
// lastParen, leftContext and rightContext -- and RegExp.prototype.compile.
//
// The properties describe the last successful match made by a RegExp of the
// intrinsic constructor, and nothing else: they are read through RegExp itself,
// and a subclass or an instance reading them is a TypeError. A match by a
// subclass's instance, whose legacy features are off, leaves them undefined
// rather than wrong, and reading them then is a TypeError too.
//
// Every match would otherwise pay for strings nobody reads, so a match records
// only its subject and where its groups fell; the strings are cut when a
// property is read.

// legacyRegExpStatics is the state behind the static properties.
type legacyRegExpStatics struct {
	// invalid marks the state a subclass's match leaves.
	invalid bool
	input   *String
	// bufs[which] holds the last match's capture positions, n of them, and
	// the other is where exec reads the next match's (spare): taking that
	// match is turning which over, not a copy. Neither is written in place
	// but by a match, so that one that is taken costs no store of a pointer
	// -- each of which, with the collector running, as it mostly is where
	// matches are made in a loop, is a write barrier's.
	bufs  [2][]int
	which uint8
	n     int
}

// spare is the buffer the next match is read into, which takeLegacyMatch
// then takes.
func (st *legacyRegExpStatics) spare() []int { return st.bufs[st.which^1] }

// setInput makes s the last match's subject, storing it only where it
// changes: a loop's matches are mostly of one string.
func (st *legacyRegExpStatics) setInput(s *String) {
	if st.input != s {
		st.input = s
	}
}

// recordLegacyMatch updates the statics after a successful match, or
// invalidates them when the RegExp's legacy features are off.
func (r *Runtime) recordLegacyMatch(legacy bool, input *String, caps []int) {
	st := &r.legacyRegExp
	r.takeLegacyMatch(legacy, input, append(st.spare()[:0], caps...))
}

// takeLegacyMatch is recordLegacyMatch for a match just read into the
// statics' spare buffer, caps, which they take: from now until the match
// after the next, which is read where they had been, the indices are
// theirs.
func (r *Runtime) takeLegacyMatch(legacy bool, input *String, caps []int) {
	st := &r.legacyRegExp
	w := st.which ^ 1
	if unsafe.SliceData(caps) != unsafe.SliceData(st.bufs[w]) {
		// It has grown.
		st.bufs[w] = caps
	}
	if !legacy {
		st.invalid, st.n = true, 0
		if st.input != nil {
			st.input = nil
		}
		return
	}
	st.invalid, st.which, st.n = false, w, len(caps)
	st.setInput(input)
}

// piece cuts one of the statics from the last match.
func (st *legacyRegExpStatics) piece(which string) *String {
	if which == "input" && st.input != nil {
		return st.input
	}
	if st.input == nil || st.n < 2 {
		return emptyString
	}
	s, caps := st.input, st.bufs[st.which][:st.n]
	group := func(i int) *String {
		if 2*i+1 >= len(caps) || caps[2*i] < 0 || caps[2*i+1] < 0 {
			return emptyString
		}
		return s.Substring(caps[2*i], caps[2*i+1])
	}
	switch which {
	case "input":
		return s
	case "lastMatch":
		return group(0)
	case "lastParen":
		if n := len(caps)/2 - 1; n > 0 {
			return group(n)
		}
		return emptyString
	case "leftContext":
		return s.Substring(0, caps[0])
	case "rightContext":
		return s.Substring(caps[1], s.Len())
	}
	// $1 to $9.
	return group(int(which[1] - '0'))
}

func (r *Runtime) initRegExpLegacyStatics(ctor *Object) {
	r.legacyRegExp.input = emptyString
	getter := func(prop, which string) *Object {
		return r.newNativeFunc("get "+prop, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			if !this.IsObject() || this.Object() != ctor {
				return Undefined, rt.throwTypeError("RegExp.%s is only readable through RegExp itself", prop)
			}
			if rt.legacyRegExp.invalid {
				return Undefined, rt.throwTypeError("RegExp.%s is not available after a subclass's match", prop)
			}
			return Str(rt.legacyRegExp.piece(which)), nil
		})
	}
	define := func(prop, which string, settable bool) {
		var set *Object
		if settable {
			set = r.newNativeFunc("set "+prop, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
				if !this.IsObject() || this.Object() != ctor {
					return Undefined, rt.throwTypeError("RegExp.%s is only writable through RegExp itself", prop)
				}
				s, err := rt.toString(arg(args, 0))
				if err != nil {
					return Undefined, err
				}
				// Setting the input changes only the input: the rest still
				// describe the last match.
				st := &rt.legacyRegExp
				if st.invalid || st.input == nil {
					st.invalid, st.n = false, 0
				}
				st.input = s
				return Undefined, nil
			})
		}
		r.defineAccessor(ctor, r.atoms.intern(prop), getter(prop, which), set, propConfigurable)
	}
	define("input", "input", true)
	define("$_", "input", true)
	define("lastMatch", "lastMatch", false)
	define("$&", "lastMatch", false)
	define("lastParen", "lastParen", false)
	define("$+", "lastParen", false)
	define("leftContext", "leftContext", false)
	define("$`", "leftContext", false)
	define("rightContext", "rightContext", false)
	define("$'", "rightContext", false)
	for i := 1; i <= 9; i++ {
		name := "$" + string(rune('0'+i))
		define(name, name, false)
	}

	// compile reinitializes a RegExp in place with a new pattern and flags,
	// which only one of the intrinsic constructor's may be.
	r.defMethod(r.proto.regexp, "compile", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !this.IsObject() || this.Object().class != ClassRegExp {
			return Undefined, rt.throwTypeError("RegExp.prototype.compile called on an incompatible receiver")
		}
		d, ok := this.Object().dataOf().(*regexpData)
		if !ok {
			return Undefined, rt.throwTypeError("RegExp.prototype.compile called on an uninitialized RegExp")
		}
		if !d.legacy() {
			return Undefined, rt.throwTypeError("RegExp.prototype.compile cannot be used on a subclass's instance")
		}
		if d.realm != rt.Realm {
			return Undefined, rt.throwTypeError("RegExp.prototype.compile cannot be used on another realm's RegExp")
		}
		pattern, flagsArg := arg(args, 0), arg(args, 1)
		var source, flags string
		if pattern.IsObject() && pattern.Object().class == ClassRegExp {
			if !flagsArg.IsUndefined() {
				return Undefined, rt.throwTypeError("flags cannot be given with a RegExp to compile")
			}
			pd := pattern.Object().dataOf().(*regexpData)
			source, flags = pd.re.Source(), pd.re.Flags().String()
		} else {
			if !pattern.IsUndefined() {
				s, err := rt.toString(pattern)
				if err != nil {
					return Undefined, err
				}
				source = s.Go()
			}
			if !flagsArg.IsUndefined() {
				f, err := rt.toString(flagsArg)
				if err != nil {
					return Undefined, err
				}
				flags = f.Go()
			}
		}
		fresh, err := rt.newRegExp(source, flags)
		if err != nil {
			return Undefined, err
		}
		d.re = fresh.Object().dataOf().(*regexpData).re
		if _, err := rt.setProp(this.Object(), atomLastIndex, Int(0), this, true); err != nil {
			return Undefined, err
		}
		return this, nil
	})
}
