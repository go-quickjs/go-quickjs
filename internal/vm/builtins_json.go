package vm

import (
	"math"
	"strings"
	"unsafe"

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

// encodeBigIntWrapper is encodeObject for a BigInt's wrapper, which stands
// for the BigInt it wraps, which has no JSON form -- so the wrapper does not
// serialize as an empty object, and stringify throws. It is out of line
// because it always ends in that error: encodeObject, which every object
// written runs, keeps its length with the check for a host's opaque proxies
// (hostNoJSON), and the encoder's code after it keeps its place.
func (e *jsonEncoder) encodeBigIntWrapper(buf []byte, v Value) ([]byte, bool, error) {
	bi, err := e.rt.toBigIntValue(v)
	if err != nil {
		return buf, false, err
	}
	return e.encode(buf, bi)
}

func (r *Runtime) initJSONBuiltins() {
	j := newObject(r.proto.object, ClassJSONObject)
	keyOrder[struct{}](r, j, nil, "JSON")
	r.defValue(r.global, "JSON", Obj(j))
	r.defToStringTag(j, "JSON")

	r.defMethod(j, "stringify", 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
		st := rt.jsonCaches()
		enc := st.enc
		st.enc = nil
		if enc == nil {
			enc = new(jsonEncoder)
		}
		v, err := enc.stringify(rt, arg(args, 0), arg(args, 1), arg(args, 2))
		enc.finish()
		st.enc = enc
		return v, err
	})

	r.defMethod(j, "parse", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		reviver := arg(args, 1)
		// A reviver is told the source text of each primitive it is handed,
		// so the parse keeps a record of it; without one, or with one that
		// cannot see the context it is told it in, there is nothing to keep.
		context := isCallable(reviver) && rt.seesThirdArg(reviver)
		v, rec, err := parseJSONOf[struct{}](rt, s, context)
		if err != nil || !isCallable(reviver) {
			return v, err
		}
		// The reviver walks the result bottom-up, so a nested value is already
		// revived by the time its parent sees it.
		root := newObject(rt.proto.object, ClassObject)
		root.setOwnRaw(rt.atoms.intern(""), v, propDefault)
		rv := &jsonReviver{fn: reviver, context: context}
		if !context {
			rv.cb = rt.regCallbackOf(reviver, Obj(root))
			if rv.cb != nil {
				defer rv.cb.end()
			}
		}
		return rt.reviveJSON(root, Str(emptyString), rv, rec)
	})

	// JSON.rawJSON makes a value stringify emits as the text it was given,
	// which is what lets a number too precise for a double, or a BigInt, be
	// written into JSON. The text must be one JSON primitive with no space
	// around it.
	r.defMethod(j, "rawJSON", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		text := s.Go()
		if text == "" || isJSONSpace(text[0]) || isJSONSpace(text[len(text)-1]) {
			return Undefined, rt.throwSyntaxError("JSON.rawJSON requires text with no space around it")
		}
		if text[0] == '{' || text[0] == '[' {
			return Undefined, rt.throwSyntaxError("JSON.rawJSON requires a primitive, not an object or an array")
		}
		p := &jsonParser{rt: rt, src: text}
		if _, err := p.parseValue(); err != nil {
			return Undefined, err
		}
		if p.pos != len(text) {
			return Undefined, rt.throwSyntaxError("unexpected trailing content in JSON at position %d", p.pos)
		}
		o := newObjectExt(nil, ClassObject, objExtensible)
		o.setData(rawJSON{})
		o.setOwnRaw(rt.atoms.intern("rawJSON"), Str(s), propEnumerable)
		o.flags &^= objExtensible
		return Obj(o), nil
	})

	r.defMethod(j, "isRawJSON", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Bool(isRawJSON(arg(args, 0))), nil
	})
}

// rawJSON marks an object JSON.rawJSON made, which is the only thing that
// makes stringify emit its text rather than serialize it. An object that
// merely looks the same is serialized as the object it is.
type rawJSON struct{}

func isRawJSON(v Value) bool {
	if !v.IsObject() {
		return false
	}
	_, ok := v.Object().dataOf().(rawJSON)
	return ok
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
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
	// pad is a newline and the indent as many times as any depth has wanted
	// (margin).
	pad []byte
	// meter charges the text as it is written: an array holding one object
	// many times is written out in full each time.
	meter heldMeter
	// depth is how deeply the walk has descended, which is bounded for the
	// same reason the parser's is: the stack cannot grow for ever, and running
	// out of it would take the host down rather than the script.
	depth int
	// stack is the objects being written, innermost last, and deep those
	// past jsonStackScan of them, which detect the cycles that would
	// otherwise recurse forever.
	stack []*Object
	deep  map[*Object]bool
	// limit is the length of the text at which charged is to be called.
	limit int
	// buf is the buffer kept for the next call, empty.
	buf []byte
	// wide says the text has strings wider than ASCII, and excess by how many
	// bytes the strings are longer than their code units, so that the text's
	// length is known without its being read. A string's own bytes are all
	// that could be: what the walk writes around it is ASCII, and starts and
	// ends the text, which has no surrogate left unpaired to join anything.
	wide   bool
	excess int
	// rescan says the text has what is not counted so: a key, an indent or
	// raw text wider than ASCII.
	rescan bool

	// replacer is the function form, called for every key.
	replacer Value
	hasFn    bool
	// argv is the arguments of a toJSON or a replacer, which a call copies,
	// as those of Array.prototype.forEach's callback are.
	argv [2]Value
	// allowed is the array form: the property names to keep, in the order they
	// were listed, which becomes the order of the output.
	allowed    []Atom
	hasAllowed bool
}

// stringify is JSON.stringify.
func (e *jsonEncoder) stringify(rt *Runtime, value, replacer, space Value) (Value, error) {
	e.rt, e.meter, e.limit = rt, heldMeter{r: rt}, builderChunk
	e.wide, e.excess = false, 0
	// In this order, which a script can see: a replacer's proxy or getters,
	// then the valueOf of a Number object given as the space.
	if err := e.setReplacer(replacer); err != nil {
		return Undefined, err
	}
	indent, err := rt.jsonIndent(space)
	if err != nil {
		return Undefined, err
	}
	e.indent, e.rescan = indent, !wtf8.IsASCII(indent)
	// The levels the walk enters it leaves as it returns, but for an error,
	// which ends the walk.
	nesting := rt.nesting
	defer func() { rt.nesting = nesting }()
	// The value is presented to the replacer as a property of a wrapper
	// object under the empty key, which is what gives the top-level call a
	// holder to pass along. Nothing else can observe that object, so it is
	// built only when there is a replacer to see it.
	holder := Undefined
	if e.hasFn {
		root := newObject(rt.proto.object, ClassObject)
		root.setOwnRaw(rt.atoms.intern(""), value, propDefault)
		holder = Obj(root)
	}
	v, err := e.apply(holder, atomEmpty, value)
	if err != nil {
		return Undefined, err
	}
	buf := e.buf
	if buf == nil {
		buf = make([]byte, 0, 64)
	}
	e.buf = nil
	buf, ok, err := e.encode(buf, v)
	if err != nil {
		return Undefined, err
	}
	if !ok {
		// A value with no JSON form, such as undefined or a function,
		// stringifies to undefined rather than to a string.
		e.buf = buf[:0]
		return Undefined, nil
	}
	counted := !e.rescan && (!e.wide || e.excess != 0)
	if counted && len(buf) <= jsonShortText {
		e.buf = buf[:0]
		return Str(shortString(buf, len(buf)-e.excess, !e.wide)), nil
	}
	var text string
	if cap(buf) > jsonKeepBytes && len(buf) >= cap(buf)-cap(buf)/8 {
		// Too large to be kept, and nearly all text: it is the string.
		text = unsafe.String(unsafe.SliceData(buf), len(buf))
	} else {
		text = string(buf)
		e.buf = buf[:0]
	}
	if !counted {
		return rt.builtString(text)
	}
	length := len(text) - e.excess
	if length > maxStringLength {
		return Undefined, rt.throwStringLength()
	}
	return Str(&String{s: text, length: length, ascii: !e.wide}), nil
}

// jsonShortText is the longest text a String is made around (shortString).
const jsonShortText = 48

// shortString is a String of the text, of the given length, which has no
// surrogate unpaired, in one allocation with it, of one of Go's size classes:
// a String and text apart are two, which is half of what JSON.stringify
// costs a small value.
func shortString(text []byte, length int, ascii bool) *String {
	var s *String
	var to []byte
	switch n := len(text); {
	case n <= 16:
		x := new(struct {
			String
			text [16]byte
		})
		s, to = &x.String, x.text[:n]
	case n <= 32:
		x := new(struct {
			String
			text [32]byte
		})
		s, to = &x.String, x.text[:n]
	default:
		x := new(struct {
			String
			text [jsonShortText]byte
		})
		s, to = &x.String, x.text[:n]
	}
	copy(to, text)
	s.s, s.length, s.ascii = unsafe.String(unsafe.SliceData(to), len(to)), length, ascii
	return s
}

// charged is called as the text reaches limit: what it holds is charged, and
// it may have grown past what a string can be.
func (e *jsonEncoder) charged(n int) error {
	if n > 3*maxStringLength {
		return e.rt.throwStringLength()
	}
	e.limit = n + builderChunk
	return e.meter.charge(n)
}

// finish leaves the encoder ready for the next call, holding nothing of this
// one's.
func (e *jsonEncoder) finish() {
	if cap(e.buf) > jsonKeepBytes {
		e.buf = nil
	}
	clear(e.stack)
	e.stack, e.deep, e.depth = e.stack[:0], nil, 0
	e.pad, e.indent = e.pad[:0], ""
	e.replacer, e.hasFn, e.allowed, e.hasAllowed = Undefined, false, nil, false
	e.argv = [2]Value{}
	e.meter = heldMeter{}
	e.rt = nil
}

// setReplacer resolves the second argument, which is either a function called
// for every key or a list of the keys to keep.
func (e *jsonEncoder) setReplacer(v Value) error {
	if isCallable(v) {
		e.replacer, e.hasFn = v, true
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
// string, a number, a boolean or null has no toJSON to look up, so only a
// replacer can take an interest in it.
func (e *jsonEncoder) needsApply(v Value) bool {
	return v.IsObject() || v.IsBigInt() || e.hasFn
}

// apply runs toJSON and then the replacer on one value, in that order.
func (e *jsonEncoder) apply(holder Value, key Atom, v Value) (Value, error) {
	// A BigInt has a toJSON hook like an object does, which is the only way to
	// serialize one at all: without it there is no JSON form and the attempt
	// is an error. A string, like a number or a boolean, is not asked
	// (SerializeJSONProperty): a String.prototype.toJSON is never called.
	if v.IsBigInt() || v.IsObject() && !e.rt.lacksToJSON(v.Object()) {
		tj, err := e.rt.getValueProp(v, atomToJSON)
		if err != nil {
			return Undefined, err
		}
		if isCallable(tj) {
			e.argv[0] = Str(e.rt.keyText(key))
			v, err = e.rt.callDirect(tj, v, e.argv[:1])
			if err != nil {
				return Undefined, err
			}
		}
	}
	if e.hasFn {
		e.argv[0], e.argv[1] = Str(e.rt.keyText(key)), v
		return e.rt.callDirect(e.replacer, holder, e.argv[:2])
	}
	return v, nil
}

// reviveJSON walks a parsed value bottom-up, handing each entry to the reviver.
//
// A value the reviver returns undefined for is deleted, which is how a reviver
// prunes what it does not want.
//
// Each call is given a context object as well, which holds the source text of
// a primitive the parse produced. A value the reviver has replaced by then --
// by changing a property it has yet to reach -- has no source, since the text
// is not the text of that value.
func (r *Runtime) reviveJSON(holder *Object, key Value, reviver *jsonReviver, rec *jsonRecord) (Value, error) {
	return r.reviveJSONAt(holder, atomEmpty, key, reviver, rec, 0)
}

// jsonReviver is a reviver as JSON.parse calls it: context says it is
// given the context object, which a reviver that cannot see a third
// argument is not (seesThirdArg), nor has the parse kept records for; cb
// is such a reviver kept ready for its calls (regCallback), or nil; argv
// the list its arguments go in, made once, as the array methods' are.
type jsonReviver struct {
	fn      Value
	context bool
	cb      *regCallback
	argv    [3]Value
}

// seesThirdArg reports whether fn, called with three arguments, may see
// the third: anything but a function of this realm's code that declares
// fewer than three parameters, none of them a rest parameter, and reads
// no arguments object, in a runtime with no debugger, where f.arguments
// reads nothing (Node's quirks), no frame of it is inspected, and nothing
// else reads what a call was passed.
func (r *Runtime) seesThirdArg(fn Value) bool {
	if !fn.IsObject() || r.nodeQuirks || r.debug != nil {
		return true
	}
	fd := fn.Object().fn()
	if fd == nil || fd.native != nil || fd.bound || fd.closure == nil || fd.closure.realm != r.Realm {
		return true
	}
	b := fd.closure.fn
	return b == nil || b.ParamCount >= 3 || b.HasRest || b.UsesArguments || b.ForwardsArguments || b.HasDirectEval
}

// reviveJSONAt is reviveJSON with the depth it has reached, which is bounded
// for the same reason the parser's is (jsonLevelsPerNest), and with k, the
// atom of key.
func (r *Runtime) reviveJSONAt(holder *Object, k Atom, key Value, reviver *jsonReviver, rec *jsonRecord, depth int) (Value, error) {
	if depth%jsonLevelsPerNest == 0 {
		if err := r.nest(); err != nil {
			return Undefined, err
		}
		defer r.unnest()
	}
	val, err := r.getProp(holder, k, Obj(holder))
	if err != nil {
		return Undefined, err
	}
	var context *Object
	if reviver.context {
		context = newObject(r.proto.object, ClassObject)
	}
	if rec == nil || !rec.value.SameValue(val) {
		rec = nil
	} else if !val.IsObject() {
		context.setOwnRaw(atomSource, Str(NewString(r.ownText(rec.source))), propDefault)
	}
	if val.IsObject() {
		o := val.Object()
		if o.IsArray() {
			a, err := r.viewArrayLike(val)
			if err != nil {
				return Undefined, err
			}
			for i := int64(0); i < a.n; i++ {
				var child *jsonRecord
				if rec != nil && i < int64(len(rec.elements)) {
					child = rec.elements[i]
				}
				ik := r.indexKey(i)
				el, err := r.reviveJSONAt(o, ik, Str(r.keyText(ik)), reviver, child, depth+1)
				if err != nil {
					return Undefined, err
				}
				if err := r.reviveWrite(o, ik, el); err != nil {
					return Undefined, err
				}
			}
		} else {
			// The keys are settled before the walk starts, so a property the
			// reviver deletes is still visited -- and then read through the
			// prototype chain, which is what Get does.
			var keys []Atom
			if c := r.shapeKeys(o); c != nil {
				keys = c.atoms
			} else {
				pks, err := r.ownKeysOf(o, false)
				if err != nil {
					return Undefined, err
				}
				keys = make([]Atom, 0, len(pks))
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
			}
			for _, pk := range keys {
				var child *jsonRecord
				if rec != nil {
					child = rec.entries[pk]
				}
				el, err := r.reviveJSONAt(o, pk, Str(r.keyText(pk)), reviver, child, depth+1)
				if err != nil {
					return Undefined, err
				}
				if err := r.reviveWrite(o, pk, el); err != nil {
					return Undefined, err
				}
			}
		}
	}
	if !reviver.context {
		// A reviver that cannot see the context is given none.
		argv := reviver.argv[:2]
		argv[0], argv[1] = key, val
		if cb := reviver.cb; cb != nil {
			return cb.callWith(Obj(holder), argv)
		}
		return r.callDirect(reviver.fn, Obj(holder), argv)
	}
	return r.callDirect(reviver.fn, Obj(holder), []Value{key, val, Obj(context)})
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
	// What is there already, as the parse left it, takes the value in place:
	// to define a property that has every attribute as one with every
	// attribute changes nothing else, whatever kind of data property it is.
	if o.class == ClassObject && !key.IsIndex() {
		if i := o.findOwn(key); i >= 0 && o.shape.keys[i].flags == propDefault {
			o.shape.written()
			o.vals()[i] = v
			return nil
		}
	}
	_, err := r.defineProperty(o, key, &propDesc{
		value: v, hasValue: true,
		writable: true, hasWritable: true,
		enumerable: true, hasEnumerable: true,
		configurable: true, hasConfigurable: true,
	})
	return err
}

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
//
// toJSON and the replacer have already run: apply does both, and does it
// before the value is inspected, so that what they return is what gets
// encoded.
func (e *jsonEncoder) encode(buf []byte, v Value) ([]byte, bool, error) {
	switch v.Kind() {
	case KindNumber:
		n := v.Number()
		// A non-finite number has no JSON form and becomes null.
		if math.Float64bits(n)>>52&0x7FF == 0x7FF {
			return append(buf, "null"...), true, nil
		}
		return jsnum.AppendFloat(buf, n), true, nil
	case KindString:
		return e.appendString(buf, v.String()), true, nil
	case KindNull:
		return append(buf, "null"...), true, nil
	case KindBool:
		if v.BoolValue() {
			return append(buf, "true"...), true, nil
		}
		return append(buf, "false"...), true, nil
	case KindUndefined, KindSymbol:
		return buf, false, nil
	case KindBigInt:
		return buf, false, e.rt.throwTypeError("a BigInt cannot be serialized to JSON")
	}
	return e.encodeObject(buf, v)
}

// appendString writes a string that is a value, whose length it knows: each
// of a string's bytes past its code units stays in the text, but for a lone
// surrogate's, which its escape replaces.
func (e *jsonEncoder) appendString(buf []byte, s *String) []byte {
	text := s.Go()
	buf, surrogates := appendJSONString(buf, text, s.ascii)
	if !s.ascii {
		e.wide = true
		e.excess += len(text) - s.length - 2*surrogates
	}
	return buf
}

// encodeObject is encode for an object.
func (e *jsonEncoder) encodeObject(buf []byte, v Value) ([]byte, bool, error) {
	o := v.Object()
	switch o.class {
	case ClassObject:
		// What JSON.rawJSON made is its text, and is written as it is.
		if o.flags&objHasExt != 0 && isRawJSON(v) {
			text, err := e.rt.getValueProp(v, e.rt.atoms.intern("rawJSON"))
			if err != nil {
				return buf, false, err
			}
			s := text.String().Go()
			if !wtf8.IsASCII(s) {
				e.rescan = true
			}
			return append(buf, s...), true, nil
		}
	// A wrapper object stands for its primitive, which is what makes
	// JSON.stringify(new Number(1)) produce 1 rather than {}.
	case ClassNumberWrapper:
		n, err := e.rt.toNumber(v)
		if err != nil {
			return buf, false, err
		}
		return e.encode(buf, Float(n))
	case ClassStringWrapper:
		sv, err := e.rt.toString(v)
		if err != nil {
			return buf, false, err
		}
		return e.encode(buf, Str(sv))
	case ClassBooleanWrapper:
		b, _ := o.dataOf().(bool)
		return e.encode(buf, Bool(b))
	case ClassBigIntWrapper:
		return e.encodeBigIntWrapper(buf, v)
	}

	if o.IsCallable() || o.class == ClassProxy && hostNoJSON(o) {
		return buf, false, nil
	}
	if e.entered(o) {
		return buf, false, e.rt.throwTypeError("converting a circular structure to JSON")
	}
	// The walk's levels are bounded as the parser's are (jsonLevelsPerNest).
	if e.depth%jsonLevelsPerNest == 0 {
		if err := e.rt.nest(); err != nil {
			return buf, false, err
		}
	}
	e.enter(o)
	var err error
	switch {
	case o.class == ClassArray || o.IsArray():
		buf, err = e.encodeElements(buf, v, o)
	case o.class == ClassObject && o.flags&objHasExt == 0 && !e.hasAllowed:
		if l := e.rt.jsonLayoutOf(o); l != nil && l.ordinary {
			buf, err = e.writeLayout(buf, v, o, l)
			break
		}
		fallthrough
	default:
		buf, err = e.encodeProps(buf, v, o)
	}
	if err != nil {
		// stringify undoes the rest: this call is over.
		return buf, false, err
	}
	e.leave(o)
	if e.depth%jsonLevelsPerNest == 0 {
		e.rt.unnest()
	}
	return buf, true, nil
}

// jsonStackScan is how many of the objects being written are compared with
// each object met. Real documents nest less, and for one that does not the
// rest are in a map.
const jsonStackScan = 64

// entered reports whether o is being written, so that a cycle would write it
// for ever.
func (e *jsonEncoder) entered(o *Object) bool {
	for _, x := range e.stack {
		if x == o {
			return true
		}
	}
	return e.deep[o]
}

// enter notes that o is being written.
func (e *jsonEncoder) enter(o *Object) {
	e.depth++
	if len(e.stack) < jsonStackScan {
		e.stack = append(e.stack, o)
		return
	}
	if e.deep == nil {
		e.deep = map[*Object]bool{}
	}
	e.deep[o] = true
}

// leave notes that o, entered last, is written.
func (e *jsonEncoder) leave(o *Object) {
	e.depth--
	if e.depth < jsonStackScan {
		e.stack[e.depth] = nil
		e.stack = e.stack[:e.depth]
		return
	}
	delete(e.deep, o)
}

// margin is a newline and the indent of the object or array at depth levels,
// the last byte of which is written first: what comes before a line of it.
func (e *jsonEncoder) margin(depth int) []byte {
	n := 1 + depth*len(e.indent)
	for len(e.pad) < n {
		if len(e.pad) == 0 {
			e.pad = append(e.pad, '\n')
		}
		e.pad = append(e.pad, e.indent...)
	}
	return e.pad[:n]
}
