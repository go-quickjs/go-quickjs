package vm

import (
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
	r.proto.regexpCtor = reCtor
	r.initRegExpSymbolMethods(p)

	r.defGetter(p, "source", func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !this.IsObject() {
			return Undefined, rt.throwTypeError("RegExp.prototype.source called on a non-object")
		}
		re, err := rt.regexpOf(this, "RegExp.prototype.source")
		if err != nil {
			// RegExp.prototype is itself not a RegExp, and reporting "(?:)"
			// for it is what keeps String(RegExp.prototype) working.
			if this.Object() == rt.proto.regexp {
				return Str(NewString("(?:)")), nil
			}
			return Undefined, err
		}
		if re.Source() == "" {
			// An empty pattern reports "(?:)" so that the result can be fed
			// back to the constructor.
			return Str(NewString("(?:)")), nil
		}
		return Str(NewString(escapeRegExpSource(re.Source()))), nil
	})

	// flags is assembled from the individual getters rather than from the
	// compiled pattern, so that overriding one of them is honoured by
	// everything that reads flags -- which is every method that matches.
	r.defGetter(p, "flags", func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !this.IsObject() {
			return Undefined, rt.throwTypeError("RegExp.prototype.flags called on a non-object")
		}
		var sb strings.Builder
		for _, fg := range regExpFlagNames {
			v, err := rt.getValueProp(this, rt.atoms.intern(fg.name))
			if err != nil {
				return Undefined, err
			}
			if v.Truthy() {
				sb.WriteByte(fg.letter)
			}
		}
		return Str(NewString(sb.String())), nil
	})

	// Each flag is exposed as its own getter. Read on RegExp.prototype itself,
	// which carries no pattern, they answer undefined rather than throwing, so
	// that feature detection does not have to guard every one.
	for _, fg := range regExpFlagNames {
		bit, name := fg.bit, fg.name
		r.defGetter(p, name, func(rt *Runtime, this Value, args []Value) (Value, error) {
			re, err := rt.regexpOf(this, "RegExp.prototype."+name)
			if err != nil {
				if this.IsObject() && this.Object() == rt.proto.regexp {
					return Undefined, nil
				}
				return Undefined, err
			}
			// The v flag turns on the u behaviour internally, but the two
			// are alternatives to a script: /a/v.unicode is false.
			fl := re.Flags()
			if fl&regexp.FlagUnicodeSets != 0 {
				fl &^= regexp.FlagUnicode
			}
			return Bool(fl&bit != 0), nil
		})
	}

	// toString is written against source and flags rather than the pattern, so
	// that a subclass overriding either is reflected in what it prints.
	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !this.IsObject() {
			return Undefined, rt.throwTypeError("RegExp.prototype.toString called on a non-object")
		}
		src, err := rt.getValueProp(this, atomSource)
		if err != nil {
			return Undefined, err
		}
		srcStr, err := rt.toString(src)
		if err != nil {
			return Undefined, err
		}
		flags, err := rt.getValueProp(this, atomFlags)
		if err != nil {
			return Undefined, err
		}
		flagStr, err := rt.toString(flags)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString("/" + srcStr.Go() + "/" + flagStr.Go())), nil
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
			return rt.regExpSymbolMatch(this, args)
		})
	r.defSymbolMethod(p, r.wellKnown.matchAll, "[Symbol.matchAll]", 1,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return rt.regExpSymbolMatchAll(this, args)
		})
	r.defSymbolMethod(p, r.wellKnown.search, "[Symbol.search]", 1,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return rt.regExpSymbolSearch(this, args)
		})
	r.defSymbolMethod(p, r.wellKnown.split, "[Symbol.split]", 2,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return rt.regExpSymbolSplit(this, args)
		})
	r.defSymbolMethod(p, r.wellKnown.replace, "[Symbol.replace]", 2,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return rt.regExpSymbolReplace(this, args)
		})

	// The iterator matchAll returns has its own prototype, so that adding a
	// method to it does not add one to every array iterator.
	r.defToStringTag(r.proto.regexpStringIter, "RegExp String Iterator")
}

// regExpFlagNames pairs each flag's property name with the letter it
// contributes to the flags string, in the order the string uses.
var regExpFlagNames = []struct {
	name   string
	letter byte
	bit    regexp.Flags
}{
	{"hasIndices", 'd', regexp.FlagHasIndices},
	{"global", 'g', regexp.FlagGlobal},
	{"ignoreCase", 'i', regexp.FlagIgnoreCase},
	{"multiline", 'm', regexp.FlagMultiline},
	{"dotAll", 's', regexp.FlagDotAll},
	{"unicode", 'u', regexp.FlagUnicode},
	{"unicodeSets", 'v', regexp.FlagUnicodeSets},
	{"sticky", 'y', regexp.FlagSticky},
}

// escapeRegExpSource makes a pattern safe to sit between two slashes.
//
// A literal cannot contain an unescaped slash or line terminator, so a pattern
// built by the constructor from a string that does has to be escaped before it
// can be printed -- otherwise new RegExp("/") would print as /// and not parse
// back.
func escapeRegExpSource(src string) string {
	if !strings.ContainsAny(src, "/\n\r\u2028\u2029") {
		return src
	}
	var sb strings.Builder
	escaped := false
	for _, c := range src {
		switch {
		case escaped:
			sb.WriteRune(c)
			escaped = false
			continue
		case c == '\\':
			sb.WriteRune(c)
			escaped = true
			continue
		case c == '/':
			sb.WriteString("\\/")
		case c == '\n':
			sb.WriteString("\\n")
		case c == '\r':
			sb.WriteString("\\r")
		case c == '\u2028':
			sb.WriteString("\\u2028")
		case c == '\u2029':
			sb.WriteString("\\u2029")
		default:
			sb.WriteRune(c)
		}
	}
	return sb.String()
}
