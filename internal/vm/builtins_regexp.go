package vm

import (
	"math"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/regexp"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// RegExp, and the String methods that delegate to it.
//
// A RegExp object holds a compiled pattern plus lastIndex, which is an ordinary
// writable property rather than an internal slot because scripts assign to it
// directly. The global and sticky flags are what make it meaningful: without
// either, every call starts from zero.

// regexpData is a RegExp's internal state.
type regexpData struct {
	re *regexp.Regexp
}

// regexpOf recovers the compiled pattern from a receiver.
func (r *Runtime) regexpOf(this Value, name string) (*regexp.Regexp, error) {
	if !this.IsObject() || this.Object().class != ClassRegExp {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	d, ok := this.Object().data.(*regexpData)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized RegExp", name)
	}
	return d.re, nil
}

// newRegExp builds a RegExp object from a pattern and flags.
func (r *Runtime) newRegExp(source, flags string) (Value, error) {
	re, err := regexp.Compile(source, flags)
	if err != nil {
		return Undefined, r.throwSyntaxError("%s", err.Error())
	}
	o := newObject(r.proto.regexp, ClassRegExp)
	o.data = &regexpData{re: re}
	// lastIndex is writable but neither enumerable nor configurable.
	o.setOwnRaw(atomLastIndex, Int(0), propWritable)
	return Obj(o), nil
}

func (r *Runtime) initRegExpBuiltins() {
	r.proto.regexp = newObject(r.proto.object, ClassObject)
	p := r.proto.regexp

	reCtor := r.newCtor("RegExp", 2, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		pattern := arg(args, 0)
		flagsArg := arg(args, 1)

		// new RegExp(re) copies the pattern, taking its flags unless new ones
		// are supplied.
		if pattern.IsObject() && pattern.Object().class == ClassRegExp {
			d := pattern.Object().data.(*regexpData)
			flags := d.re.Flags().String()
			if !flagsArg.IsUndefined() {
				s, err := rt.toString(flagsArg)
				if err != nil {
					return Undefined, err
				}
				flags = s.Go()
			}
			return rt.newRegExp(d.re.Source(), flags)
		}

		source := ""
		if !pattern.IsUndefined() {
			s, err := rt.toString(pattern)
			if err != nil {
				return Undefined, err
			}
			source = s.Go()
		}
		flags := ""
		if !flagsArg.IsUndefined() {
			s, err := rt.toString(flagsArg)
			if err != nil {
				return Undefined, err
			}
			flags = s.Go()
		}
		return rt.newRegExp(source, flags)
	})
	r.defSpecies(reCtor)
	r.initRegExpSymbolMethods(p)

	r.defGetter(p, "source", func(rt *Runtime, this Value, args []Value) (Value, error) {
		re, err := rt.regexpOf(this, "RegExp.prototype.source")
		if err != nil {
			return Undefined, err
		}
		if re.Source() == "" {
			// An empty pattern reports "(?:)" so that the result can be fed
			// back to the constructor.
			return Str(NewString("(?:)")), nil
		}
		return Str(NewString(re.Source())), nil
	})
	r.defGetter(p, "flags", func(rt *Runtime, this Value, args []Value) (Value, error) {
		re, err := rt.regexpOf(this, "RegExp.prototype.flags")
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(re.Flags().String())), nil
	})

	// Each flag is exposed as its own getter.
	flagGetters := []struct {
		name string
		bit  regexp.Flags
	}{
		{"global", regexp.FlagGlobal},
		{"ignoreCase", regexp.FlagIgnoreCase},
		{"multiline", regexp.FlagMultiline},
		{"dotAll", regexp.FlagDotAll},
		{"unicode", regexp.FlagUnicode},
		{"sticky", regexp.FlagSticky},
		{"hasIndices", regexp.FlagHasIndices},
	}
	for _, fg := range flagGetters {
		bit, name := fg.bit, fg.name
		r.defGetter(p, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			re, err := rt.regexpOf(this, "RegExp.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			return Bool(re.Flags()&bit != 0), nil
		})
	}

	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		re, err := rt.regexpOf(this, "RegExp.prototype.toString")
		if err != nil {
			return Undefined, err
		}
		src := re.Source()
		if src == "" {
			src = "(?:)"
		}
		return Str(NewString("/" + src + "/" + re.Flags().String())), nil
	})

	r.defMethod(p, "exec", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return rt.regexpExec(this, s)
	})

	r.defMethod(p, "test", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		res, err := rt.regexpExec(this, s)
		if err != nil {
			return Undefined, err
		}
		return Bool(!res.IsNull()), nil
	})
}

// regexpExec runs a pattern against a string, honouring and updating lastIndex.
func (r *Runtime) regexpExec(this Value, s *String) (Value, error) {
	re, err := r.regexpOf(this, "RegExp.prototype.exec")
	if err != nil {
		return Undefined, err
	}
	o := this.Object()

	// lastIndex is consulted only when the pattern is global or sticky; a plain
	// pattern always searches from the start.
	stateful := re.Flags()&(regexp.FlagGlobal|regexp.FlagSticky) != 0
	start := 0
	if stateful {
		liVal, err := r.getProp(o, atomLastIndex, this)
		if err != nil {
			return Undefined, err
		}
		n, err := r.toInteger(liVal)
		if err != nil {
			return Undefined, err
		}
		start = int(n)
	}

	units := wtf8.ToUTF16(s.Go())
	if start < 0 || start > len(units) {
		if stateful {
			o.setOwnRaw(atomLastIndex, Int(0), propWritable)
		}
		return Null, nil
	}

	caps, err := re.Match(units, start)
	if err != nil {
		return Undefined, r.throwError(errSyntax, "%s", err.Error())
	}
	if caps == nil {
		if stateful {
			o.setOwnRaw(atomLastIndex, Int(0), propWritable)
		}
		return Null, nil
	}
	if stateful {
		o.setOwnRaw(atomLastIndex, Int(caps[1]), propWritable)
	}
	return Obj(r.buildMatchResult(re, units, caps, s)), nil
}

// buildMatchResult assembles the array exec returns: the matched text, then
// each group, with index, input and groups attached as properties.
func (r *Runtime) buildMatchResult(re *regexp.Regexp, units []uint16, caps []int, input *String) *Object {
	n := len(caps) / 2
	elems := make([]Value, n)
	for i := 0; i < n; i++ {
		lo, hi := caps[2*i], caps[2*i+1]
		if lo < 0 || hi < 0 {
			// A group that did not participate reads as undefined, which is
			// distinct from having matched the empty string.
			elems[i] = Undefined
			continue
		}
		elems[i] = Str(NewString(wtf8.FromUTF16(units[lo:hi])))
	}

	arr := r.newArrayFrom(elems)
	arr.setOwnRaw(atomIndex, Int(caps[0]), propDefault)
	arr.setOwnRaw(atomInput, Str(input), propDefault)

	names := re.GroupNames()
	if len(names) == 0 {
		arr.setOwnRaw(atomGroups, Undefined, propDefault)
		return arr
	}
	groups := newObject(nil, ClassObject)
	for name, idx := range names {
		groups.setOwnRaw(r.atoms.intern(name), elems[idx], propDefault)
	}
	arr.setOwnRaw(atomGroups, Obj(groups), propDefault)
	return arr
}

// ---------------------------------------------------------------------------
// The String methods that take a pattern
// ---------------------------------------------------------------------------

// toRegExp coerces a value used as a pattern, compiling a string as a literal
// would with the given extra flags.
func (r *Runtime) toRegExp(v Value, extraFlags string) (Value, error) {
	if v.IsObject() && v.Object().class == ClassRegExp {
		return v, nil
	}
	source := ""
	if !v.IsUndefined() {
		s, err := r.toString(v)
		if err != nil {
			return Undefined, err
		}
		source = regexp.QuoteMeta(s.Go())
	}
	return r.newRegExp(source, extraFlags)
}

func (r *Runtime) initStringRegExpMethods() {
	p := r.proto.str

	// Each of these dispatches through the corresponding symbol method, which
	// is what lets a RegExp subclass -- or any object at all -- define how it
	// matches. The regexp behaviour lives on RegExp.prototype under the symbol;
	// only the fallback for a non-regexp argument is here.
	r.defMethod(p, "match", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.dispatchStringRegExp(this, args, rt.wellKnown.match, "", nil)
	})

	r.defMethod(p, "matchAll", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// matchAll is the one that insists on the global flag, and it checks
		// before dispatching so that a non-global regexp is refused whatever
		// its symbol method would have done.
		if re := arg(args, 0); re.IsObject() && re.Object().class == ClassRegExp {
			flags, err := rt.getValueProp(re, rt.atoms.intern("flags"))
			if err != nil {
				return Undefined, err
			}
			if flags.IsString() && !strings.Contains(flags.String().Go(), "g") {
				return Undefined, rt.throwTypeError(
					"matchAll requires a global regular expression")
			}
		}
		return rt.dispatchStringRegExp(this, args, rt.wellKnown.matchAll, "g", nil)
	})

	r.defMethod(p, "search", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.dispatchStringRegExp(this, args, rt.wellKnown.search, "", nil)
	})
}

// dispatchStringRegExp implements the shape every String method that takes a
// pattern shares.
//
// The argument is asked for its symbol method first, so that anything can act
// as a pattern; only when it has none is it coerced to a RegExp and the
// intrinsic used. extra is appended to the arguments the symbol method
// receives, which is how replace passes its replacement along.
func (r *Runtime) dispatchStringRegExp(this Value, args []Value, sym *Symbol,
	addFlags string, extra []Value) (Value, error) {
	if this.IsNullish() {
		return Undefined, r.throwTypeError("String.prototype method called on %s", this.Kind())
	}
	pattern := arg(args, 0)
	if !pattern.IsNullish() {
		method, err := r.getValueProp(pattern, r.atoms.internSymbol(sym))
		if err != nil {
			return Undefined, err
		}
		if isCallable(method) {
			return r.call(method, pattern, append([]Value{this}, extra...))
		}
	}
	s, err := r.toString(this)
	if err != nil {
		return Undefined, err
	}
	reVal, err := r.toRegExp(pattern, addFlags)
	if err != nil {
		return Undefined, err
	}
	method, err := r.getValueProp(reVal, r.atoms.internSymbol(sym))
	if err != nil {
		return Undefined, err
	}
	if !isCallable(method) {
		return Undefined, r.throwTypeError("the pattern has no matching method")
	}
	return r.call(method, reVal, append([]Value{Str(s)}, extra...))
}

// initRegExpSymbolMethods defines the five symbol methods that carry the actual
// regexp behaviour.
//
// They live on RegExp.prototype rather than inside the String methods because
// that is where a subclass can override them, and because String.prototype
// reaches them by lookup rather than by knowing what a RegExp is.
func (r *Runtime) initRegExpSymbolMethods(p *Object) {
	r.defSymbolMethod(p, r.wellKnown.match, "[Symbol.match]", 1,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			re, err := rt.regexpOf(this, "RegExp.prototype[Symbol.match]")
			if err != nil {
				return Undefined, err
			}
			s, err := rt.toString(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			// Without the global flag, match is exec: one result with its
			// groups.
			if re.Flags()&regexp.FlagGlobal == 0 {
				return rt.regexpExec(this, s)
			}
			// With it, match returns every matched substring and no group
			// information, which is a different shape entirely.
			this.Object().setOwnRaw(atomLastIndex, Int(0), propWritable)
			var out []Value
			err = rt.forEachMatch(re, s, func(units []uint16, caps []int) error {
				out = append(out, Str(NewString(wtf8.FromUTF16(units[caps[0]:caps[1]]))))
				return nil
			})
			if err != nil {
				return Undefined, err
			}
			if len(out) == 0 {
				return Null, nil
			}
			return Obj(rt.newArrayFrom(out)), nil
		})

	r.defSymbolMethod(p, r.wellKnown.matchAll, "[Symbol.matchAll]", 1,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			re, err := rt.regexpOf(this, "RegExp.prototype[Symbol.matchAll]")
			if err != nil {
				return Undefined, err
			}
			s, err := rt.toString(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			// The results are collected up front rather than produced lazily,
			// which is observable only if lastIndex is mutated mid-iteration.
			var results []Value
			units := wtf8.ToUTF16(s.Go())
			err = rt.forEachMatch(re, s, func(u []uint16, caps []int) error {
				results = append(results, Obj(rt.buildMatchResult(re, units, caps, s)))
				return nil
			})
			if err != nil {
				return Undefined, err
			}
			return rt.newArrayIterator(Obj(rt.newArrayFrom(results)))
		})

	r.defSymbolMethod(p, r.wellKnown.search, "[Symbol.search]", 1,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			re, err := rt.regexpOf(this, "RegExp.prototype[Symbol.search]")
			if err != nil {
				return Undefined, err
			}
			s, err := rt.toString(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			caps, err := re.Match(wtf8.ToUTF16(s.Go()), 0)
			if err != nil {
				return Undefined, rt.throwError(errSyntax, "%s", err.Error())
			}
			if caps == nil {
				return Int(-1), nil
			}
			return Int(caps[0]), nil
		})

	r.defSymbolMethod(p, r.wellKnown.split, "[Symbol.split]", 2,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			if _, err := rt.regexpOf(this, "RegExp.prototype[Symbol.split]"); err != nil {
				return Undefined, err
			}
			s, err := rt.toString(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			limit := math.MaxInt32
			if lv := arg(args, 1); !lv.IsUndefined() {
				n, err := rt.toUint32(lv)
				if err != nil {
					return Undefined, err
				}
				limit = int(n)
			}
			return rt.regexpSplit(this, s, limit)
		})

	r.defSymbolMethod(p, r.wellKnown.replace, "[Symbol.replace]", 2,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			if _, err := rt.regexpOf(this, "RegExp.prototype[Symbol.replace]"); err != nil {
				return Undefined, err
			}
			s, err := rt.toString(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			// The global flag decides whether every match is replaced, so
			// the caller does not have to say.
			re, _ := rt.regexpOf(this, "")
			all := re.Flags()&regexp.FlagGlobal != 0
			return rt.regexpReplace(this, s, arg(args, 1), all)
		})
}

// forEachMatch walks every non-overlapping match of a global pattern.
//
// An empty match advances the position by one, which is what stops a pattern
// like /a*/g from looping forever on a string it matches emptily.
func (r *Runtime) forEachMatch(re *regexp.Regexp, s *String, fn func([]uint16, []int) error) error {
	units := wtf8.ToUTF16(s.Go())
	pos := 0
	for pos <= len(units) {
		caps, err := re.Match(units, pos)
		if err != nil {
			return r.throwError(errSyntax, "%s", err.Error())
		}
		if caps == nil {
			return nil
		}
		if err := fn(units, caps); err != nil {
			return err
		}
		if caps[1] == caps[0] {
			pos = caps[1] + 1
			continue
		}
		pos = caps[1]
	}
	return nil
}

// regexpReplace implements String.prototype.replace and replaceAll when the
// pattern is a regular expression.
func (r *Runtime) regexpReplace(reVal Value, s *String, repl Value, all bool) (Value, error) {
	re, err := r.regexpOf(reVal, "String.prototype.replace")
	if err != nil {
		return Undefined, err
	}
	global := all || re.Flags()&regexp.FlagGlobal != 0

	units := wtf8.ToUTF16(s.Go())
	var sb strings.Builder
	last := 0
	pos := 0

	for pos <= len(units) {
		caps, err := re.Match(units, pos)
		if err != nil {
			return Undefined, r.throwError(errSyntax, "%s", err.Error())
		}
		if caps == nil {
			break
		}
		sb.WriteString(wtf8.FromUTF16(units[last:caps[0]]))

		text, err := r.expandReplacement(re, units, caps, s, repl)
		if err != nil {
			return Undefined, err
		}
		sb.WriteString(text)
		last = caps[1]

		if !global {
			break
		}
		if caps[1] == caps[0] {
			// An empty match must advance, or the loop would not terminate.
			if caps[1] < len(units) {
				sb.WriteString(wtf8.FromUTF16(units[caps[1] : caps[1]+1]))
			}
			last = caps[1] + 1
			pos = caps[1] + 1
			continue
		}
		pos = caps[1]
	}

	if last < len(units) {
		sb.WriteString(wtf8.FromUTF16(units[last:]))
	}
	if global {
		reVal.Object().setOwnRaw(atomLastIndex, Int(0), propWritable)
	}
	return Str(NewString(sb.String())), nil
}

// expandReplacement produces the text one match is replaced with.
func (r *Runtime) expandReplacement(re *regexp.Regexp, units []uint16, caps []int, input *String, repl Value) (string, error) {
	n := len(caps) / 2

	if isCallable(repl) {
		// A replacement function receives the match, each group, the offset and
		// the whole input.
		callArgs := make([]Value, 0, n+2)
		for i := 0; i < n; i++ {
			lo, hi := caps[2*i], caps[2*i+1]
			if lo < 0 {
				callArgs = append(callArgs, Undefined)
				continue
			}
			callArgs = append(callArgs, Str(NewString(wtf8.FromUTF16(units[lo:hi]))))
		}
		callArgs = append(callArgs, Int(caps[0]), Str(input))
		res, err := r.call(repl, Undefined, callArgs)
		if err != nil {
			return "", err
		}
		s, err := r.toString(res)
		if err != nil {
			return "", err
		}
		return s.Go(), nil
	}

	s, err := r.toString(repl)
	if err != nil {
		return "", err
	}
	return r.expandDollarPatterns(s.Go(), re, units, caps), nil
}

// expandDollarPatterns substitutes the $ forms in a replacement string.
func (r *Runtime) expandDollarPatterns(repl string, re *regexp.Regexp, units []uint16, caps []int) string {
	if !strings.ContainsRune(repl, '$') {
		return repl
	}
	n := len(caps) / 2
	group := func(i int) string {
		if i < 0 || i >= n {
			return ""
		}
		lo, hi := caps[2*i], caps[2*i+1]
		if lo < 0 {
			return ""
		}
		return wtf8.FromUTF16(units[lo:hi])
	}

	var sb strings.Builder
	for i := 0; i < len(repl); i++ {
		if repl[i] != '$' || i+1 >= len(repl) {
			sb.WriteByte(repl[i])
			continue
		}
		switch c := repl[i+1]; {
		case c == '$':
			sb.WriteByte('$')
			i++
		case c == '&':
			sb.WriteString(group(0))
			i++
		case c == '`':
			sb.WriteString(wtf8.FromUTF16(units[:caps[0]]))
			i++
		case c == '\'':
			sb.WriteString(wtf8.FromUTF16(units[caps[1]:]))
			i++
		case c == '<':
			// A named group reference.
			end := strings.IndexByte(repl[i+2:], '>')
			if end < 0 {
				sb.WriteByte('$')
				continue
			}
			name := repl[i+2 : i+2+end]
			if idx, ok := re.GroupNames()[name]; ok {
				sb.WriteString(group(idx))
			}
			i += 2 + end
		case c >= '0' && c <= '9':
			// Two digits are preferred when they name a real group, so $12
			// means group 12 if it exists and group 1 followed by "2" if not.
			idx := int(c - '0')
			consumed := 1
			if i+2 < len(repl) && repl[i+2] >= '0' && repl[i+2] <= '9' {
				two := idx*10 + int(repl[i+2]-'0')
				if two < n {
					idx = two
					consumed = 2
				}
			}
			if idx == 0 || idx >= n {
				sb.WriteByte('$')
				continue
			}
			sb.WriteString(group(idx))
			i += consumed
		default:
			sb.WriteByte('$')
		}
	}
	return sb.String()
}

// regexpSplit implements String.prototype.split with a pattern separator.
func (r *Runtime) regexpSplit(reVal Value, s *String, limit int) (Value, error) {
	re, err := r.regexpOf(reVal, "String.prototype.split")
	if err != nil {
		return Undefined, err
	}
	units := wtf8.ToUTF16(s.Go())
	var out []Value

	if len(units) == 0 {
		// Splitting an empty string yields one empty piece unless the pattern
		// matches it, in which case it yields none.
		caps, err := re.Match(units, 0)
		if err != nil {
			return Undefined, r.throwError(errSyntax, "%s", err.Error())
		}
		if caps == nil {
			out = append(out, Str(s))
		}
		return Obj(r.newArrayFrom(out)), nil
	}

	last := 0
	pos := 0
	for pos < len(units) && len(out) < limit {
		caps, err := re.Match(units, pos)
		if err != nil {
			return Undefined, r.throwError(errSyntax, "%s", err.Error())
		}
		if caps == nil || caps[0] >= len(units) {
			break
		}
		if caps[1] == last {
			// A separator that matches emptily at the current position would
			// not make progress.
			pos++
			continue
		}
		out = append(out, Str(NewString(wtf8.FromUTF16(units[last:caps[0]]))))
		// A capturing separator contributes its groups to the result.
		for i := 1; i < len(caps)/2 && len(out) < limit; i++ {
			lo, hi := caps[2*i], caps[2*i+1]
			if lo < 0 {
				out = append(out, Undefined)
				continue
			}
			out = append(out, Str(NewString(wtf8.FromUTF16(units[lo:hi]))))
		}
		last = caps[1]
		pos = caps[1]
		if caps[1] == caps[0] {
			pos++
		}
	}
	if len(out) < limit {
		out = append(out, Str(NewString(wtf8.FromUTF16(units[last:]))))
	}
	return Obj(r.newArrayFrom(out)), nil
}
