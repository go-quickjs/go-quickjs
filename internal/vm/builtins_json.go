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
		// so the parse keeps a record of it; without one there is nothing to
		// keep.
		v, rec, err := rt.parseJSON(s.Go(), isCallable(reviver))
		if err != nil || !isCallable(reviver) {
			return v, err
		}
		// The reviver walks the result bottom-up, so a nested value is already
		// revived by the time its parent sees it.
		root := newObject(rt.proto.object, ClassObject)
		root.setOwnRaw(rt.atoms.intern(""), v, propDefault)
		return rt.reviveJSON(root, Str(emptyString), reviver, rec)
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
			e.argv[0] = Str(e.rt.keyString(key))
			v, err = e.rt.callDirect(tj, v, e.argv[:1])
			if err != nil {
				return Undefined, err
			}
		}
	}
	if e.hasFn {
		e.argv[0], e.argv[1] = Str(e.rt.keyString(key)), v
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
func (r *Runtime) reviveJSON(holder *Object, key Value, reviver Value, rec *jsonRecord) (Value, error) {
	return r.reviveJSONAt(holder, atomEmpty, key, reviver, rec, 0)
}

// reviveJSONAt is reviveJSON with the depth it has reached, which is bounded
// for the same reason the parser's is, and with k, the atom of key.
func (r *Runtime) reviveJSONAt(holder *Object, k Atom, key Value, reviver Value, rec *jsonRecord, depth int) (Value, error) {
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
	context := newObject(r.proto.object, ClassObject)
	if rec == nil || !rec.value.SameValue(val) {
		rec = nil
	} else if !val.IsObject() {
		context.setOwnRaw(atomSource, Str(NewString(rec.source)), propDefault)
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
				el, err := r.reviveJSONAt(o, ik, Str(r.keyString(ik)), reviver, child, depth+1)
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
				el, err := r.reviveJSONAt(o, pk, Str(r.keyString(pk)), reviver, child, depth+1)
				if err != nil {
					return Undefined, err
				}
				if err := r.reviveWrite(o, pk, el); err != nil {
					return Undefined, err
				}
			}
		}
	}
	return r.callDirect(reviver, Obj(holder), []Value{key, val, Obj(context)})
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
		// It stands for the BigInt it wraps, which has no JSON form -- so
		// the wrapper does not serialize as an empty object.
		bi, err := e.rt.toBigIntValue(v)
		if err != nil {
			return buf, false, err
		}
		return e.encode(buf, bi)
	}

	if o.IsCallable() {
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

// jsonParser is a recursive-descent parser for JSON text.
//
// An object is not built as its keys are read, as an object made one property
// at a time would be: its values wait on scratch while the parser walks the
// shape tree, and it is made once, at its last key, in the size it needs.
// Which shape comes next is guessed, as V8's parser takes its guess from the
// object before: the last object parsed in the same place in a document says
// each key to expect (hints), and past that a shape's first transition does,
// so that a key that was expected costs a comparison of its text with a name,
// where any other is hashed. A transition out of a shape is for a key the
// shape does not have, which is what shows the keys of an object made this
// way to be different: a key given twice is met where no transition leads, and
// such an object is built by the table (parseEntries), with the rest of what
// the walk cannot do.
type jsonParser struct {
	rt  *Runtime
	src string
	pos int
	// depth is how deeply the parser has descended, which is bounded: the
	// stack it recurses on cannot grow for ever.
	depth int
	// scratch collects the elements of the arrays and the values of the
	// objects being parsed, each above its parent's. Each is copied out at
	// exactly its own length when it closes, so growing this one buffer
	// replaces growing a slice per array. Above its length it is zero, so
	// that it holds nothing once it is empty.
	scratch []Value

	// st is what is kept from one parse to the next. root is the shape an
	// object starts from, or nil where objects are built by the table: for
	// a reviver's records, and where the runtime's objects share no layout.
	st    *jsonState
	hints *jsonHints
	root  *shape
	// ctx says where in the document the next value is: the keys' positions
	// that lead to it, whichever array it is an element of.
	ctx uint32
	// strs is strings made and not yet used, and strRoom how many bytes of
	// them may still be made together (newString).
	strs    []String
	strRoom int
	// esc is the buffer a string with an escape is built in, and text the
	// batch it goes on to (keepText).
	esc  []byte
	text []byte

	// records asks for a jsonRecord of each value, which a reviver needs;
	// last is the record of the value parsed most recently, and recScratch
	// is scratch for the records of arrays' elements.
	records    bool
	last       *jsonRecord
	recScratch []*jsonRecord
}

// jsonHintBits sizes jsonHints, and jsonKeyBits jsonState.keys. A
// document has few places objects are made in, each of few layouts; the
// names a program's documents use are more, and each is one word.
const (
	jsonHintBits = 7
	jsonKeyBits  = 8
	jsonMetBits  = 12
)

// jsonState is what JSON keeps from one call to the next, made at its first
// use: a runtime that uses no JSON pays one word for it.
type jsonState struct {
	// keys is the atoms of the keys hashed lately, by their text. An entry
	// is only a guess, which is compared with the name of the atom it holds.
	keys [1 << jsonKeyBits]Atom
	// values is a parser's scratch between calls, empty.
	values []Value
	// met is the layouts met once, as bits (metBefore), and metCount how
	// many are set.
	met      [1 << (jsonMetBits - 6)]uint64
	metCount int
	// enc is an encoder between calls: a call made from a toJSON, a
	// replacer or a getter, while another is out, makes its own.
	enc *jsonEncoder
}

// jsonKeepValues and jsonKeepBytes are the most of a parser's scratch, in
// values, and of an encoder's buffer, in bytes, kept between calls. A document
// past them is large enough to pay for its buffer, and is not left holding it.
const (
	jsonKeepValues = 1 << 14
	jsonKeepBytes  = 1 << 16
)

// jsonHints is, for each place in a document (jsonParser.ctx), the shape of
// the last object parsed there, which was reached from the realm's root by
// transitions of plain data properties, and stays valid: a shape in the tree
// never changes, and a place that two kinds of object share is a guess that
// fails at a key (parseObject). It is a realm's, so that its prototype is the
// one a parse there makes objects of: a shape holds its prototype, and a
// runtime's table would hold the realm of the last object parsed.
type jsonHints [1 << jsonHintBits]*shape

// jsonCaches is the runtime's jsonState.
func (r *Runtime) jsonCaches() *jsonState {
	if r.json == nil {
		r.json = new(jsonState)
	}
	return r.json
}

// parseJSON parses the whole of src.
func (r *Runtime) parseJSON(src string, records bool) (Value, *jsonRecord, error) {
	st := r.jsonCaches()
	p := jsonParser{rt: r, src: src, st: st, records: records, scratch: st.values, strRoom: len(src)}
	st.values = nil
	if !records {
		if root := rootFor(ClassObject, r.proto.object); root != nil && !root.unique && !root.loose && root.tree != nil {
			p.root = root
			if r.Realm.jsonHints == nil {
				r.Realm.jsonHints = new(jsonHints)
			}
			p.hints = r.Realm.jsonHints
		}
	}
	v, err := p.parseText()
	clear(p.scratch)
	if cap(p.scratch) <= jsonKeepValues {
		st.values = p.scratch[:0]
	}
	return v, p.last, err
}

// parseText parses one value and the space around it.
func (p *jsonParser) parseText() (Value, error) {
	p.skipSpace()
	v, err := p.parseValue()
	if err != nil {
		return Undefined, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return Undefined, p.rt.throwSyntaxError("unexpected trailing content in JSON at position %d", p.pos)
	}
	return v, nil
}

// jsonRecord is what JSON.parse remembers of a value for a reviver: the value
// itself, and for a primitive the text it was parsed from, or for an array or
// object the records of what it holds. A key given twice keeps the record of
// the value it ended up with, the last.
type jsonRecord struct {
	value    Value
	source   string
	elements []*jsonRecord
	entries  map[Atom]*jsonRecord
}

// skipSpace steps over white space, of which minified text has none.
func (p *jsonParser) skipSpace() {
	if p.pos < len(p.src) && p.src[p.pos] > ' ' {
		return
	}
	p.skipSpaces()
}

func (p *jsonParser) skipSpaces() {
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
	start := p.pos
	v, err := p.parseValueAt()
	if err == nil && p.records && !v.IsObject() {
		p.last = &jsonRecord{value: v, source: p.src[start:p.pos]}
	}
	return v, err
}

// parseValueAt is parseValue without the record, which an array or an object
// makes for itself.
func (p *jsonParser) parseValueAt() (Value, error) {
	if p.pos >= len(p.src) {
		return Undefined, p.rt.throwSyntaxError("unexpected end of JSON input")
	}
	c := p.src[p.pos]
	switch c {
	case '"':
		return p.parseStringValue()
	case '{', '[':
		if p.depth >= jsonMaxDepth {
			return Undefined, p.rt.throwRangeError(
				"JSON nests too deeply to be parsed")
		}
		p.depth++
		var v Value
		var err error
		if c == '{' {
			v, err = p.parseObject()
		} else {
			v, err = p.parseArray()
		}
		p.depth--
		return v, err
	case 't':
		if strings.HasPrefix(p.src[p.pos:], "true") {
			p.pos += 4
			return True, nil
		}
	case 'f':
		if strings.HasPrefix(p.src[p.pos:], "false") {
			p.pos += 5
			return False, nil
		}
	case 'n':
		if strings.HasPrefix(p.src[p.pos:], "null") {
			p.pos += 4
			return Null, nil
		}
	default:
		if c == '-' || (c >= '0' && c <= '9') {
			return p.parseNumber()
		}
	}
	return Undefined, p.rt.throwSyntaxError(
		"unexpected token %q in JSON at position %d", p.src[p.pos], p.pos)
}

// parseObject parses an object by walking the shape tree (see jsonParser).
func (p *jsonParser) parseObject() (Value, error) {
	root := p.root
	if root == nil {
		return p.parseObjectEntries()
	}
	p.pos++ // consume '{'
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		return Obj(newShapedLiteral(root, ClassObject, 0)), nil
	}
	ctx := p.ctx
	slot := &p.hints[ctx>>(32-jsonHintBits)]
	hint := *slot
	cur := root
	mark := len(p.scratch)
	n := int32(0)
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
		// While the object is the hint's, the hint says the shape, and cur
		// stands still. A hint is only a guess, and nothing it says is
		// taken on trust: each key is compared with the name in its place,
		// so that the keys so far are the hint's first n, and its ancestor
		// of n properties the shape they make, the one a walk by
		// transitions would have come to.
		if hint != nil && (n >= hint.n || !p.rt.atoms.names(hint.keys[n].key, key)) {
			cur = hint.ancestorAt(n)
			hint = nil
		}
		if hint == nil {
			c := p.nextShape(cur, key)
			if c == nil {
				// The key is still to come, and any after it: such an object
				// is given room, or the first of them would move it.
				return p.parseEntries(p.objectOf(cur, mark, jsonRoomLeft), nil, key)
			}
			cur = c
		}
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return Undefined, p.rt.throwSyntaxError("expected ':' in JSON at position %d", p.pos)
		}
		p.pos++
		p.skipSpace()
		n++
		p.ctx = (ctx + uint32(n)) * 0x9E3779B1
		v, err := p.parseValueAt()
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
		case '}':
			p.pos++
			s := cur
			if hint != nil {
				s = hint.ancestorAt(n)
			}
			if s != *slot {
				*slot = remember(s)
			}
			return Obj(p.objectOf(s, mark, 0)), nil
		default:
			return Undefined, p.rt.throwSyntaxError(
				"expected ',' or '}' in JSON at position %d", p.pos)
		}
	}
}

// nextShape is the shape of an object of shape s given key as its next
// property, or nil if the walk cannot go there: s has the key, or the tree
// will not hold the shape.
func (p *jsonParser) nextShape(s *shape, key string) *shape {
	if s.next != nil && s.nextFlags == propDefault && p.rt.atoms.names(s.nextKey, key) {
		return s.next
	}
	a := p.internKey(key)
	e := shapeEdge{a, propDefault}
	if s.next != nil {
		if s.nextEdge() == e {
			return s.next
		}
		if c := s.more[e]; c != nil {
			return c
		}
	}
	// No transition: s may have the key. A table long enough to want an
	// index is asked by the object, which has one.
	if s.n > linearScanLimit {
		return nil
	}
	for _, k := range s.keys[:s.n] {
		if k.key == a {
			return nil
		}
	}
	return s.child(e)
}

// internKey is the atom of a key, which may be a slice of the document.
func (p *jsonParser) internKey(key string) Atom {
	atoms := p.rt.atoms
	e := &p.st.keys[jsonKeyHash(key)]
	if atoms.names(*e, key) {
		return *e
	}
	a := atoms.internCopy(key)
	*e = a
	return a
}

// jsonKeyHash is a slot of jsonState.keys for a key, from its length and its
// ends, which is where the keys of a document differ.
func jsonKeyHash(key string) uint32 {
	n := len(key)
	var h uint64
	switch {
	case n >= 8:
		h = wtf8.Load64(key, 0) ^ wtf8.Load64(key, n-8)<<1
	case n > 0:
		h = uint64(key[0]) | uint64(key[n/2])<<8 | uint64(key[n-1])<<16
	}
	return uint32((h + uint64(n)) * 0x9E3779B97F4A7C15 >> (64 - jsonKeyBits))
}

// jsonRoomLeft is the room an object is made with for keys the walk could not
// take.
const jsonRoomLeft = 2

// objectOf makes the object of shape s whose values are those on scratch from
// mark, and takes them off it, with room for more.
func (p *jsonParser) objectOf(s *shape, mark, more int) *Object {
	vals := p.scratch[mark:]
	n := len(vals)
	o := newShapedLiteral(s, ClassObject, n+more)
	copy(o.vals()[:n], vals)
	o.valn = uint32(n)
	if n > linearScanLimit && s.index == nil {
		o.buildIndex()
	}
	clear(vals)
	p.scratch = p.scratch[:mark]
	if verifyShapes {
		checkShape(o)
	}
	return o
}

// parseObjectEntries is parseObject for an object built by the table: all of
// them while a reviver wants records of what they hold.
func (p *jsonParser) parseObjectEntries() (Value, error) {
	// How many properties there are is not known without parsing them, and
	// growing the table from nothing costs an allocation per doubling. Room
	// for a few, in the object's own allocation, pays for itself by the second
	// key.
	o := newLiteralObject(p.rt.proto.object, ClassObject, 0)
	var rec *jsonRecord
	if p.records {
		rec = &jsonRecord{value: Obj(o), entries: make(map[Atom]*jsonRecord)}
	}
	p.pos++ // consume '{'
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		p.last = rec
		return Obj(o), nil
	}
	key, err := p.parseKey()
	if err != nil {
		return Undefined, err
	}
	return p.parseEntries(o, rec, key)
}

// parseKey parses a property name.
func (p *jsonParser) parseKey() (string, error) {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return "", p.rt.throwSyntaxError(
			"expected a property name in JSON at position %d", p.pos)
	}
	return p.parseString()
}

// parseEntries parses what is left of an object, from the colon after key.
func (p *jsonParser) parseEntries(o *Object, rec *jsonRecord, key string) (Value, error) {
	for {
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
		atom := p.internKey(key)
		o.setOwnRaw(atom, v, propDefault)
		if rec != nil {
			rec.entries[atom] = p.last
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
			p.last = rec
			return Obj(o), nil
		default:
			return Undefined, p.rt.throwSyntaxError(
				"expected ',' or '}' in JSON at position %d", p.pos)
		}
		if key, err = p.parseKey(); err != nil {
			return Undefined, err
		}
	}
}

func (p *jsonParser) parseArray() (Value, error) {
	p.pos++ // consume '['
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		a := Obj(p.rt.newArrayFrom(nil))
		if p.records {
			p.last = &jsonRecord{value: a}
		}
		return a, nil
	}
	ctx := p.ctx
	mark := len(p.scratch)
	recMark := len(p.recScratch)
	for {
		p.skipSpace()
		p.ctx = ctx
		var v Value
		var err error
		if p.records {
			v, err = p.parseValue()
			p.recScratch = append(p.recScratch, p.last)
		} else {
			v, err = p.parseValueAt()
		}
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
			elems := p.scratch[mark:]
			a := Obj(p.rt.newArrayFrom(elems))
			clear(elems)
			p.scratch = p.scratch[:mark]
			if p.records {
				recs := make([]*jsonRecord, len(p.recScratch)-recMark)
				copy(recs, p.recScratch[recMark:])
				clear(p.recScratch[recMark:])
				p.recScratch = p.recScratch[:recMark]
				p.last = &jsonRecord{value: a, elements: recs}
			}
			return a, nil
		default:
			return Undefined, p.rt.throwSyntaxError(
				"expected ',' or ']' in JSON at position %d", p.pos)
		}
	}
}

// The word-at-a-time tests of a string's text: each has the top bit of a byte
// of its result set if that byte, or a lower one, is what it looks for, so
// that the lowest bit set is the first such byte, exactly. A subtraction
// borrows only from a byte that is itself found.
const (
	jsonOnes   = 0x0101010101010101
	jsonHigh   = 0x8080808080808080
	jsonQuotes = 0x2222222222222222 // '"'
	jsonSlashs = 0x5C5C5C5C5C5C5C5C // '\\'
	jsonSpaces = 0x2020202020202020 // the first character that is not a control
	jsonLeadED = 0xEDEDEDEDEDEDEDED // what a surrogate's three bytes begin with
)

// jsonStops finds in w a quotation mark, a backslash, a control character or
// a byte that is not ASCII.
func jsonStops(w uint64) uint64 {
	return (w | (w ^ jsonQuotes - jsonOnes) | (w ^ jsonSlashs - jsonOnes) | (w - jsonSpaces)) & jsonHigh
}

// jsonStopsWide is jsonStops for text that may have bytes above ASCII, which
// are stepped over. A byte's own top bit would otherwise be taken for a find.
func jsonStopsWide(w uint64) uint64 {
	q, b := w^jsonQuotes, w^jsonSlashs
	return ((q-jsonOnes)&^q | (b-jsonOnes)&^b | (w-jsonSpaces)&^w) & jsonHigh
}

// jsonWordsFirst is how many words of a string are read before it is taken for
// a long one.
const jsonWordsFirst = 4

// jsonPlainEnd is where the plain text from i ends: the index of the first
// quotation mark, backslash or control character from there, or the text's
// length, and whether the text is all ASCII to there.
func jsonPlainEnd(s string, i int) (int, bool) {
	n := len(s)
	ascii := true
	// s is the rest of the document, whose strings are short: the scan of
	// long ones is for one that has got this far.
	for words := 0; i+8 <= n; words++ {
		if words == jsonWordsFirst {
			if i += jsonscan.ASCII(s[i:]); i+8 > n {
				break
			}
		}
		if m := jsonStops(wtf8.Load64(s, i)); m != 0 {
			i += bits.TrailingZeros64(m) >> 3
			if s[i] < utf8.RuneSelf {
				return i, true
			}
			ascii = false
			break
		}
		i += 8
	}
	if ascii {
		for ; i < n; i++ {
			c := s[i]
			if c >= utf8.RuneSelf {
				ascii = false
				break
			}
			if c == '"' || c == '\\' || c < 0x20 {
				return i, true
			}
		}
		if ascii {
			return n, true
		}
	}
	for words := 0; i+8 <= n; words++ {
		// Bytes above ASCII are stepped over: a quotation mark is stopped
		// at anyway.
		if words == jsonWordsFirst {
			if i += jsonscan.Wide(s[i:], '"'); i+8 > n {
				break
			}
		}
		if m := jsonStopsWide(wtf8.Load64(s, i)); m != 0 {
			return i + bits.TrailingZeros64(m)>>3, false
		}
		i += 8
	}
	for ; i < n; i++ {
		if c := s[i]; c == '"' || c == '\\' || c < 0x20 {
			return i, false
		}
	}
	return n, false
}

// parseString parses a string: a property name, which only the shape tree or
// the atom table goes on to keep, or the text of a value.
func (p *jsonParser) parseString() (string, error) {
	s, _, err := p.parseStringASCII()
	return s, err
}

// parseStringASCII is parseString, which says too if a string it could slice
// out of the document is all ASCII.
func (p *jsonParser) parseStringASCII() (string, bool, error) {
	p.pos++ // consume '"'

	// A string with no escape in it -- which nearly every string in real JSON
	// is -- is a slice of the source text, costing neither a copy nor an
	// allocation. Sharing the bytes is what the engine's own substrings do.
	start := p.pos
	i, ascii := jsonPlainEnd(p.src, start)
	if i >= len(p.src) {
		p.pos = i
		return "", false, p.rt.throwSyntaxError("unterminated string in JSON")
	}
	if c := p.src[i]; c == '"' {
		p.pos = i + 1
		return p.src[start:i], ascii, nil
	} else if c != '\\' {
		p.pos = i
		return "", false, p.rt.throwSyntaxError(
			"a control character is not allowed in a JSON string at position %d", i)
	}
	return p.parseEscaped(start, i, ascii)
}

// parseEscaped builds the string from start, whose first escape is at i, and
// says if it is all ASCII, as the text before i is if ascii says so.
func (p *jsonParser) parseEscaped(start, i int, ascii bool) (string, bool, error) {
	buf := append(p.esc[:0], p.src[start:i]...)
	p.pos = i
	for {
		if p.pos >= len(p.src) {
			return "", false, p.rt.throwSyntaxError("unterminated string in JSON")
		}
		c := p.src[p.pos]
		switch {
		case c == '"':
			p.pos++
			p.esc = buf
			return p.keepText(buf), ascii, nil
		case c == '\\':
			p.pos++
			if p.pos >= len(p.src) {
				return "", false, p.rt.throwSyntaxError("unterminated escape in JSON")
			}
			var b byte
			switch p.src[p.pos] {
			case '"':
				b = '"'
			case '\\':
				b = '\\'
			case '/':
				b = '/'
			case 'n':
				b = '\n'
			case 'r':
				b = '\r'
			case 't':
				b = '\t'
			case 'b':
				b = '\b'
			case 'f':
				b = '\f'
			case 'u':
				r, err := p.parseUnicodeEscape()
				if err != nil {
					return "", false, err
				}
				if r >= utf8.RuneSelf {
					ascii = false
				}
				if r >= 0xDC00 && r <= 0xDFFF {
					// Text can end in a high surrogate just as an escape
					// can, and the two are then one character (AppendJoin).
					buf = wtf8.AppendJoin(buf, string(wtf8.AppendRune(nil, r)))
					continue
				}
				// A lone surrogate is preserved rather than replaced.
				buf = wtf8.AppendRune(buf, r)
				continue
			default:
				return "", false, p.rt.throwSyntaxError(
					"invalid escape %q in JSON at position %d", p.src[p.pos], p.pos)
			}
			buf = append(buf, b)
			p.pos++
		case c < 0x20:
			return "", false, p.rt.throwSyntaxError(
				"a control character is not allowed in a JSON string at position %d", p.pos)
		default:
			// Escapes come close together where there are any, and the text
			// between them is taken a byte at a time for as long as it is
			// short.
			j, end := p.pos, min(p.pos+jsonShortRun, len(p.src))
			for j < end {
				if c := p.src[j]; c < 0x20 || c == '"' || c == '\\' || c >= utf8.RuneSelf {
					break
				}
				j++
			}
			if j == end || j < len(p.src) && p.src[j] >= utf8.RuneSelf {
				var plain bool
				if j, plain = jsonPlainEnd(p.src, j); !plain {
					ascii = false
				}
			}
			if c == 0xED {
				// May be a low surrogate, after an escape of a high one.
				buf = wtf8.AppendJoin(buf, p.src[p.pos:j])
			} else {
				buf = append(buf, p.src[p.pos:j]...)
			}
			p.pos = j
		}
	}
}

// jsonShortRun is how many bytes of text between escapes are read one by one.
const jsonShortRun = 8

// jsonTextBatch is the most text made room for at once.
const jsonTextBatch = 1 << 12

// keepText is a string of the text an escaped string came to. A document's
// are written one after another into a batch, one allocation for many, which
// a string that outlives the rest holds: no more than jsonTextBatch, and no
// more than is left of the document it would otherwise be a slice of.
func (p *jsonParser) keepText(b []byte) string {
	if len(b) > jsonTextBatch/8 {
		return string(b)
	}
	if cap(p.text)-len(p.text) < len(b) {
		p.text = make([]byte, 0, max(min(len(p.src)-p.pos, jsonTextBatch), len(b)))
	}
	at := len(p.text)
	p.text = append(p.text, b...)
	return unsafe.String(unsafe.SliceData(p.text[at:]), len(b))
}

// parseStringValue parses a string that is a value.
func (p *jsonParser) parseStringValue() (Value, error) {
	s, ascii, err := p.parseStringASCII()
	if err != nil {
		return Undefined, err
	}
	if s == "" {
		return Str(emptyString), nil
	}
	if !ascii {
		// WTF-8 joined from an escape's pairs is what NewString reads, as it
		// reads any other.
		ascii, length := scanString(s)
		out := p.newString()
		out.s, out.length, out.ascii = s, length, ascii
		if !ascii {
			out.endsHigh, out.startsLow = wtf8.UnpairedEnds(s)
		}
		return Str(out), nil
	}
	out := p.newString()
	out.s, out.length, out.ascii = s, len(s), true
	return Str(out), nil
}

// jsonMaxStrs is the most strings made at once.
const jsonMaxStrs = 32

// newString is a new empty String. A document's strings are made a batch at a
// time, which is one allocation for many, and a string that outlives the rest
// holds its batch, as it holds the document its text is a slice of. So that
// what the batches of a document hold together is never more than its size
// again, whichever strings outlive it -- one of each record's ten had held
// every String of a document, four times its size -- they take strRoom's
// bytes in all, and the strings past that one allocation each. A batch is
// smaller than what is left of the document too, which has room for that
// many strings.
func (p *jsonParser) newString() *String {
	if len(p.strs) == 0 {
		size := int(unsafe.Sizeof(String{}))
		n := min((len(p.src)-p.pos)/(2*size), p.strRoom/size, jsonMaxStrs)
		if n < 2 {
			return new(String)
		}
		p.strRoom -= n * size
		p.strs = make([]String, n)
	}
	s := &p.strs[0]
	p.strs = p.strs[1:]
	return s
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
	// An integer, the commonest number there is, ends at a comma or a bracket.
	i := p.pos
	var m uint64
	for i < len(p.src) {
		d := p.src[i] - '0'
		if d > 9 {
			break
		}
		m = m*10 + uint64(d)
		i++
	}
	if n := i - p.pos; n > 0 && n < 16 && i < len(p.src) && (p.src[i] == ',' || p.src[i] == ']' || p.src[i] == '}') &&
		(n == 1 || p.src[p.pos] != '0') {
		p.pos = i
		return Float(float64(m)), nil
	}
	if f, end, ok := jsnum.ParseShort(p.src, p.pos); ok {
		p.pos = end
		return Float(f), nil
	}
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
	f, err := jsnum.ParseFloat(p.src[start:p.pos])
	if err != nil {
		// Overflow yields infinity, which is the same result JavaScript gives.
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return Float(f), nil
		}
		return Undefined, p.rt.throwSyntaxError("invalid number in JSON at position %d", start)
	}
	return Float(f), nil
}
