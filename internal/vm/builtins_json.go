package vm

import (
	"encoding/binary"
	"math"
	"math/bits"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
	"github.com/go-quickjs/go-quickjs/internal/jsonscan"
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
// for the same reason the parser's is, and with k, the atom of key.
func (r *Runtime) reviveJSONAt(holder *Object, k Atom, key Value, reviver *jsonReviver, rec *jsonRecord, depth int) (Value, error) {
	if depth >= jsonMaxDepth {
		return Undefined, r.throwRangeError(
			"a structure nested this deeply cannot be revived")
	}
	// A reviver may parse again, and revive that, which the depth above
	// does not see.
	if err := r.nest(); err != nil {
		return Undefined, err
	}
	defer r.unnest()
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
	if e.depth >= jsonMaxDepth {
		return buf, false, e.rt.throwRangeError(
			"a structure nested this deeply cannot be serialized")
	}
	if err := e.rt.nest(); err != nil {
		return buf, false, err
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
	e.rt.unnest()
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

// encodeElements writes an array, or a proxy of one.
func (e *jsonEncoder) encodeElements(buf []byte, v Value, o *Object) ([]byte, error) {
	a := arrayLike{o: o}
	if o.class == ClassArray {
		a.n = int64(o.arrayLength())
	} else if err := e.rt.readArrayLike(&a, v); err != nil {
		return buf, err
	}
	if a.n == 0 {
		return append(buf, "[]"...), nil
	}
	var inner []byte
	if e.indent != "" {
		inner = e.margin(e.depth)
	}
	buf = append(buf, '[')
	buf = append(buf, inner...)
	for i := int64(0); i < a.n; i++ {
		// A dense array is read without the script running, so the walk
		// checks for an interrupt itself (arrayLike.get); and what it writes
		// may grow past what a string can be.
		if len(buf) >= e.limit {
			if err := e.charged(len(buf)); err != nil {
				return buf, err
			}
		}
		if i > 0 {
			buf = append(buf, ',')
			buf = append(buf, inner...)
		}
		el, err := a.get(e.rt, i)
		if err != nil {
			return buf, err
		}
		if e.needsApply(el) {
			// The key reaches the replacer as a string, the way a property
			// name does: an array index is not a number here.
			var key Atom
			if i <= math.MaxUint32 {
				key = e.rt.atoms.indexAtom(uint32(i))
			} else {
				// A proxy over an array can report a length past uint32.
				key = e.rt.atoms.intern(strconv.FormatInt(i, 10))
			}
			el, err = e.apply(v, key, el)
			if err != nil {
				return buf, err
			}
		}
		var ok bool
		buf, ok, err = e.encode(buf, el)
		if err != nil {
			return buf, err
		}
		if !ok {
			// An element with no JSON form becomes null, unlike a
			// property, which is omitted.
			buf = append(buf, "null"...)
		}
	}
	if inner != nil {
		buf = append(buf, e.margin(e.depth-1)...)
	}
	return append(buf, ']'), nil
}

// encodeProps writes an object that is no array.
func (e *jsonEncoder) encodeProps(buf []byte, v Value, o *Object) ([]byte, error) {
	if e.hasAllowed {
		return e.writeKeys(buf, v, o, e.allowed)
	}
	// Small ordinary objects can snapshot their keys on the stack. Keep
	// the snapshot even for getters: one may change the keys still to come.
	var localKeys [8]Atom
	var keys []Atom
	plain := o.class == ClassObject && o.flags&objHasExt == 0 && int(o.valn) <= len(localKeys)
	if plain {
		keys = localKeys[:0]
		for _, p := range o.keys() {
			if p.flags&propDeleted != 0 || p.flags&propEnumerable == 0 || e.rt.atoms.symbol(p.key) != nil {
				continue
			}
			if _, index := e.rt.atoms.arrayIndex(p.key); index {
				plain = false
				break
			}
			keys = append(keys, p.key)
		}
	}
	if !plain {
		keys = nil
		var own []Atom
		if p := proxyOf(o); p != nil {
			// A proxy's keys come from its ownKeys trap, which the plain
			// property table would not reflect.
			var err error
			if own, err = e.rt.proxyOwnKeys(p); err != nil {
				return buf, err
			}
		} else {
			var err error
			if own, err = e.rt.ownKeysOf(o, false); err != nil {
				return buf, err
			}
		}
		keys = own[:0]
		for _, k := range own {
			if e.rt.atoms.symbol(k) != nil {
				continue
			}
			enumerable, err := e.rt.isEnumerable(o, k)
			if err != nil {
				return buf, err
			}
			if !enumerable {
				continue
			}
			keys = append(keys, k)
		}
	}
	return e.writeKeys(buf, v, o, keys)
}

// writeKeys writes the properties of o that keys name, in their order: an
// explicit key list fixes both which properties appear and their order,
// and otherwise they are its own enumerable string keys.
func (e *jsonEncoder) writeKeys(buf []byte, v Value, o *Object, keys []Atom) ([]byte, error) {
	var inner []byte
	colon := ":"
	if e.indent != "" {
		inner = e.margin(e.depth)
		colon = ": "
	}
	start := len(buf)
	buf = append(buf, '{')
	buf = append(buf, inner...)
	written := 0
	for _, k := range keys {
		if err := e.rt.tick(); err != nil {
			return buf, err
		}
		if len(buf) >= e.limit {
			if err := e.charged(len(buf)); err != nil {
				return buf, err
			}
		}
		val, err := e.rt.getProp(o, k, v)
		if err != nil {
			return buf, err
		}
		if e.needsApply(val) {
			if val, err = e.apply(v, k, val); err != nil {
				return buf, err
			}
		}
		// A property whose value has no JSON form is omitted, key and all, so
		// the separator and the key are written first and taken back when the
		// value turns out not to be there.
		mark := len(buf)
		if written > 0 {
			buf = append(buf, ',')
			buf = append(buf, inner...)
		}
		name := e.rt.atoms.name(k)
		ascii := wtf8.IsASCII(name)
		if !ascii {
			e.rescan = true
		}
		buf, _ = appendJSONString(buf, name, ascii)
		buf = append(buf, colon...)
		var ok bool
		buf, ok, err = e.encode(buf, val)
		if err != nil {
			return buf, err
		}
		if !ok {
			buf = buf[:mark]
			continue
		}
		written++
	}
	if written == 0 {
		return append(buf[:start], "{}"...), nil
	}
	if inner != nil {
		buf = append(buf, e.margin(e.depth-1)...)
	}
	return append(buf, '}'), nil
}

// jsonLayout is what JSON.stringify keeps of a layout objects share, on its
// side: its keys, their text, and whether a toJSON can be there. The first
// object of a layout makes it for the rest, as V8's stringifier reads a map's
// descriptors rather than ask each object.
//
// The keys are the object's as it is reached. Like any snapshot of them they
// are not looked at again, so that a getter or a toJSON that changes the
// object changes the values the rest of the walk reads, not the walk: a
// value is read straight from its slot only while the object still has the
// layout, and past that by Get, which is what the specification does for each.
type jsonLayout struct {
	// ordinary says keys is what the object's properties are, or else that
	// it holds none: an array's, or one too long to keep (jsonLayoutKeys).
	ordinary bool
	// ascii says text is all ASCII.
	ascii bool
	// asks says toJSON has to be looked up on such an object, because it may
	// be there -- on the object, on its prototypes, or behind a proxy --
	// while cell stands.
	asks bool
	// cell is the validity cell of the chain above the layout: while it is
	// valid, what was found there is still so. nil means none can be kept.
	cell *protoCell
	keys []jsonLayoutKey
	// text is the keys' text, each a comma, the key quoted, and a colon.
	text string
}

// jsonLayoutKey is a key of a jsonLayout: the end of its text in the
// layout's, its atom, and, for a property that holds a value, its slot.
type jsonLayoutKey struct {
	atom Atom
	end  uint32
	slot int32
	data bool
}

// jsonLayoutOf is the jsonLayout of o, an ordinary object or an array, or nil
// for a layout only o has, which may change under it.
func (r *Runtime) jsonLayoutOf(o *Object) *jsonLayout {
	s := o.shape
	if s == nil || s.unique || s.loose || o.flags&objPolyProto != 0 {
		return nil
	}
	if sd := s.side; sd != nil && sd.json != nil {
		return sd.json
	}
	if !r.jsonCaches().metBefore(s) {
		return nil
	}
	return r.newJSONLayout(o)
}

// metBefore reports whether a layout with no jsonLayout has been met before,
// and notes that it has: most are, and those made for one object -- with keys
// of its own, or of a document written once -- are spared what a jsonLayout
// costs to make and to keep. Layouts are told apart by a hash of their address
// and each one's bit stands till the bits are cleared, so that one may be
// taken for another, which only makes its record sooner.
func (st *jsonState) metBefore(s *shape) bool {
	h := uint64(uintptr(unsafe.Pointer(s))) * 0x9E3779B97F4A7C15 >> (64 - jsonMetBits)
	word, bit := &st.met[h/64], uint64(1)<<(h%64)
	if *word&bit != 0 {
		return true
	}
	*word |= bit
	if st.metCount++; st.metCount >= 1<<(jsonMetBits-1) {
		clear(st.met[:])
		st.metCount = 0
	}
	return false
}

// jsonLayoutKeys and jsonLayoutText are the most keys, and bytes of their
// text, a jsonLayout holds: a layout past them keeps what it says of toJSON
// alone, and its objects are written by their keys. Each layout of a chain
// has its keys' text whole, so that an object given a key at a time, and
// written at each, had made the square of what it holds -- 500 MB for a
// thousand keys of a kilobyte, kept as long as the layouts are, and charged
// to the script's memory limit. Records are smaller than this.
const (
	jsonLayoutKeys = 128
	jsonLayoutText = 4096
)

// newJSONLayout makes o's layout's jsonLayout.
func (r *Runtime) newJSONLayout(o *Object) *jsonLayout {
	l := &jsonLayout{ordinary: o.class == ClassObject, ascii: true}
	if l.ordinary {
		var text []byte
		for _, k := range o.ownKeys(false, r.atoms) {
			i := o.findOwn(k)
			if i < 0 {
				continue
			}
			flags := o.shape.keys[i].flags
			if flags&(propEnumerable|propPrivate|propDeleted) != propEnumerable || r.atoms.symbol(k) != nil {
				continue
			}
			text = append(text, ',')
			name := r.atoms.name(k)
			ascii := wtf8.IsASCII(name)
			if !ascii {
				l.ascii = false
			}
			text, _ = appendJSONString(text, name, ascii)
			text = append(text, ':')
			l.keys = append(l.keys, jsonLayoutKey{atom: k, end: uint32(len(text)), slot: i, data: flags&propAccessor == 0})
			if len(l.keys) > jsonLayoutKeys || len(text) > jsonLayoutText {
				l.ordinary, l.keys, text = false, nil, nil
				break
			}
		}
		l.text = string(text)
	}
	r.lookForToJSON(l, o.shape)
	sideOf[struct{}](o.shape).json = l
	return l
}

// lookForToJSON says in l what objects of layout s make of toJSON, and how
// long that holds.
func (r *Runtime) lookForToJSON(l *jsonLayout, s *shape) {
	l.cell = r.chainCell(s.proto)
	l.asks = l.cell == nil
	for _, k := range s.keys[:s.n] {
		if k.key == atomToJSON {
			l.asks = true
		}
	}
	for p := s.proto; p != nil && !l.asks; p = p.proto() {
		// A proxy or a namespace answers for itself; the rest hold it in
		// their tables.
		l.asks = proxyOf(p) != nil || p.class == ClassModuleNamespace || p.getOwn(atomToJSON).ok()
	}
}

// lacksToJSON reports whether o is known to have no toJSON, without looking
// one up, which walks its prototypes.
func (r *Runtime) lacksToJSON(o *Object) bool {
	if o.class != ClassObject && o.class != ClassArray {
		return false
	}
	l := r.jsonLayoutOf(o)
	if l == nil {
		return false
	}
	if c := l.cell; c == nil || !c.valid {
		r.lookForToJSON(l, o.shape)
	}
	return !l.asks
}

// writeLayout is writeKeys for an object of a layout with a jsonLayout.
func (e *jsonEncoder) writeLayout(buf []byte, v Value, o *Object, l *jsonLayout) ([]byte, error) {
	if !l.ascii {
		e.rescan = true
	}
	var inner []byte
	if e.indent != "" {
		inner = e.margin(e.depth)
	}
	s := o.shape
	start := len(buf)
	buf = append(buf, '{')
	buf = append(buf, inner...)
	written := false
	from := 0
	for i := range l.keys {
		k := &l.keys[i]
		text := l.text[from:k.end]
		from = int(k.end)
		if err := e.rt.tick(); err != nil {
			return buf, err
		}
		if len(buf) >= e.limit {
			if err := e.charged(len(buf)); err != nil {
				return buf, err
			}
		}
		var val Value
		var err error
		if k.data && o.shape == s {
			val = o.valAt(int(k.slot))
		} else if val, err = e.rt.getProp(o, k.atom, v); err != nil {
			return buf, err
		}
		if e.needsApply(val) {
			if val, err = e.apply(v, k.atom, val); err != nil {
				return buf, err
			}
		}
		// As in writeKeys, a value with no JSON form takes its key back.
		mark := len(buf)
		if inner != nil {
			if written {
				buf = append(buf, ',')
				buf = append(buf, inner...)
			}
			buf = append(buf, text[1:len(text)-1]...)
			buf = append(buf, ": "...)
		} else if written {
			buf = append(buf, text...)
		} else {
			buf = append(buf, text[1:]...)
		}
		var ok bool
		buf, ok, err = e.encode(buf, val)
		if err != nil {
			return buf, err
		}
		if !ok {
			buf = buf[:mark]
			continue
		}
		written = true
	}
	if !written {
		return append(buf[:start], "{}"...), nil
	}
	if inner != nil {
		buf = append(buf, e.margin(e.depth-1)...)
	}
	return append(buf, '}'), nil
}

// appendJSONString appends a quoted string, escaping the characters JSON
// requires plus any lone surrogate, which must be written as an escape because
// it has no valid UTF-8 form. It says how many it escaped so. ascii says the
// string is all ASCII.
//
// Text that needs no escape is found a word at a time, and written as it is
// found while the string is ASCII, as most are: most strings are short, for
// which a call to copy them costs more than the copying.
func appendJSONString(buf []byte, s string, ascii bool) ([]byte, int) {
	if cap(buf)-len(buf) < len(s)+2 {
		buf = growJSON(buf, len(s)+2)
	}
	buf = append(buf, '"')
	i := 0
	if ascii && len(s) < jsonscan.MinLength {
		to := len(buf)
		out := buf[:cap(buf)]
		for ; i+8 <= len(s); i += 8 {
			w := wtf8.Load64(s, i)
			if jsonStops(w) != 0 {
				break
			}
			binary.LittleEndian.PutUint64(out[to:], w)
			to += 8
		}
		for ; i < len(s); i++ {
			c := s[i]
			if c < 0x20 || c == '"' || c == '\\' {
				break
			}
			out[to] = c
			to++
		}
		buf = out[:to]
		if i == len(s) {
			return append(buf, '"'), 0
		}
	}
	plain, surrogates := i, 0
	for {
		var at int
		if ascii {
			at = jsonEscapeASCII(s, i)
		} else {
			at = jsonEscapeWide(s, i)
		}
		if at == len(s) {
			break
		}
		c := s[at]
		if c >= utf8.RuneSelf {
			if r, ok := wtf8.DecodeSurrogateAt(s, at); ok {
				// A lone surrogate is emitted as an escape, which is what the
				// specification's well-formed stringify requires.
				buf = append(buf, s[plain:at]...)
				buf = append(buf, `\u`...)
				buf = strconv.AppendUint(buf, uint64(r), 16)
				surrogates++
				i = at + 3
				plain = i
				continue
			}
			i = at + 1
			continue
		}
		buf = append(buf, s[plain:at]...)
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
		i = at + 1
		plain = i
	}
	buf = append(buf, s[plain:]...)
	return append(buf, '"'), surrogates
}

// growJSON is buf with room for n more bytes. A document that is mostly long
// strings grows its buffer here, and append takes a large one up by only a
// quarter, copying it about four times over: this doubles it. One string
// longer than all there is, though, is most of the text, and the buffer can
// be the text itself if it is about that size.
func growJSON(buf []byte, n int) []byte {
	size := 2 * cap(buf)
	if n >= cap(buf) {
		size = len(buf) + n + n/8
	}
	// It is written before it is read, so it need not be cleared first:
	// only a Builder gets memory that is not.
	var b strings.Builder
	b.Grow(size)
	b.Write(buf)
	b.WriteByte(0)
	out := unsafe.Slice(unsafe.StringData(b.String()), b.Cap())
	return out[:len(buf)]
}

// jsonEscapeASCII is the index from i of the first byte of s, all ASCII, that
// needs an escape, or its length.
func jsonEscapeASCII(s string, i int) int {
	n := len(s)
	i += jsonscan.ASCII(s[i:])
	for ; i+8 <= n; i += 8 {
		if m := jsonStops(wtf8.Load64(s, i)); m != 0 {
			return i + bits.TrailingZeros64(m)>>3
		}
	}
	for ; i < n; i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' {
			return i
		}
	}
	return n
}

// jsonEscapeWide is jsonEscapeASCII for text wider than ASCII: the index of
// the first byte that needs an escape or may begin a surrogate.
func jsonEscapeWide(s string, i int) int {
	n := len(s)
	i += jsonscan.Wide(s[i:], 0xED)
	for ; i+8 <= n; i += 8 {
		w := wtf8.Load64(s, i)
		q, b, d := w^jsonQuotes, w^jsonSlashs, w^jsonLeadED
		if m := ((q-jsonOnes)&^q | (b-jsonOnes)&^b | (d-jsonOnes)&^d | (w-jsonSpaces)&^w) & jsonHigh; m != 0 {
			return i + bits.TrailingZeros64(m)>>3
		}
	}
	for ; i < n; i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' || c == 0xED {
			return i
		}
	}
	return n
}

// jsonMaxDepth bounds how deeply JSON may nest.
//
// Parsing, serializing and reviving all walk the structure by recursion, and
// the goroutine stack a deep enough document would exhaust cannot be grown for
// ever -- nor can its exhaustion be caught, which would take the host down
// rather than the script. Real documents nest tens deep; this is the point past
// which one is no longer data.
const jsonMaxDepth = 10000
