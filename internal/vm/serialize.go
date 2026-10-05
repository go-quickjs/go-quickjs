package vm

import (
	"math/big"

	"github.com/go-quickjs/go-intl/date"
)

// Structured serialization, as HTML defines it for postMessage and
// structuredClone.
//
// A value is serialized into a form that belongs to no runtime -- Go strings,
// numbers and byte slices, with every object a node that others refer to by
// index, so that cycles and shared references come out as they went in -- and
// deserialized in any runtime, the one it came from or another on another
// goroutine. What the engine knows of an object is read from its internal
// slots rather than guessed from what it says about itself, so a plain object
// that calls itself a Date is cloned as the plain object it is.
//
// The host's own objects -- a MessagePort, a DOMException -- are the host's to
// serialize, through a Codec.

// Serialized is a value serialized to be deserialized elsewhere, once.
type Serialized struct {
	nodes []snode
	root  int
}

// Copy is another serialization of the same value, which can be deserialized
// as this one can, once: deserializing moves an ArrayBuffer's bytes, and a
// value sent to many -- a broadcast -- is serialized once and copied for each.
// A transferred buffer's bytes are copied like any other's; a shared memory
// is shared, as it is.
func (s *Serialized) Copy() *Serialized {
	out := &Serialized{nodes: make([]snode, len(s.nodes)), root: s.root}
	for i, n := range s.nodes {
		if n.bytes != nil {
			n.bytes = append([]byte(nil), n.bytes...)
		}
		if n.big != nil {
			n.big = new(big.Int).Set(n.big)
		}
		out.nodes[i] = n
	}
	return out
}

// DataCloneError is a value that cannot be serialized, with V8's message for
// it. The host turns it into the DOMException it is.
type DataCloneError struct{ Message string }

// CloneBrand says how a host's object clones. It is kept as the internal
// slot of an ordinary object, which has none of its own.
type CloneBrand uint8

const (
	// CloneHost is asked of the host's codec, whatever its prototype.
	CloneHost CloneBrand = iota + 1
	// CloneOpaque clones as an empty object: its state is not in its
	// properties.
	CloneOpaque
	// CloneUnsupported cannot be cloned.
	CloneUnsupported
	// CloneNeedsTransfer can only be transferred, as a stream is.
	CloneNeedsTransfer
)

// SetCloneBrand brands an ordinary object with how it clones. It reports
// false for any other.
func (r *Runtime) SetCloneBrand(v Value, b CloneBrand) bool {
	if !v.IsObject() {
		return false
	}
	o := v.Object()
	if o.class != ClassObject || o.data != nil && !isBrand(o.data) {
		return false
	}
	o.data = b
	return true
}

func isBrand(d any) bool { _, ok := d.(CloneBrand); return ok }

func (e *DataCloneError) Error() string { return e.Message }

// Codec lets the host serialize objects of its own.
type Codec struct {
	// Serialize returns a token for a host object that can be cloned, and
	// whether v is one. It is asked about errors and about objects whose
	// prototype is not Object.prototype, which is what a host's own objects
	// are, and never about an object literal.
	Serialize func(v Value) (token any, ok bool, err error)
	// Transfer returns a token for a host object in the transfer list, and
	// whether v is one that can be transferred. It is asked about each entry
	// once, and turns down one listed twice.
	Transfer func(v Value) (token any, ok bool, err error)
	// Revive makes the host object a token stands for.
	Revive func(token any) (Value, error)
}

type snodeKind uint8

const (
	snPrimitive snodeKind = iota
	snObject
	snArray
	snBooleanObject
	snNumberObject
	snStringObject
	snBigIntObject
	snDate
	snRegExp
	snMap
	snSet
	snArrayBuffer
	snSharedArrayBuffer
	snView
	snError
	snHost
)

// snode is one serialized value.
type snode struct {
	kind snodeKind
	// prim is a primitive's kind, and num, str and big its value -- also a
	// wrapper's, a Date's time, a RegExp's source.
	prim Kind
	num  float64
	str  string
	flag string
	big  *big.Int
	// props are an object's or an array's enumerable string-keyed
	// properties, and length an array's length.
	props  []sprop
	length uint32
	// items are a Map's keys and values in turn, or a Set's values.
	items []int
	// bytes are an ArrayBuffer's, moved rather than copied when it was
	// transferred; resizable and max are its growth.
	bytes     []byte
	resizable bool
	max       int64
	mem       *SharedMemory
	// A view's buffer, element type (or a DataView), offset, length, and
	// whether it tracks a resizable buffer's length.
	buffer   int
	elem     elemType
	dataView bool
	offset   int
	count    int
	tracking bool
	// An error's kind, and which of message, stack and cause it has; the
	// cause is items[0].
	errKind              errorKind
	hasMessage, hasStack bool
	stack                string
	hasCause             bool
	// token is a host object's.
	token any
}

type sprop struct {
	key string
	val int
}

// serializer is one serialization in progress.
type serializer struct {
	r      *Runtime
	codec  *Codec
	out    *Serialized
	memory map[*Object]int
	depth  int
}

// maxCloneDepth bounds how deeply nested a serialized value may be, which the
// recursion that walks it would otherwise not.
const maxCloneDepth = 10000

// Serialize serializes v, transferring what transfer lists: an ArrayBuffer's
// bytes move, and it is detached; a host object is the codec's to transfer.
// A value that cannot be serialized is a *DataCloneError.
func (r *Runtime) Serialize(v Value, transfer []Value, codec *Codec) (*Serialized, error) {
	s := &serializer{r: r, codec: codec, out: &Serialized{}, memory: map[*Object]int{}}
	var buffers []*Object
	for _, t := range transfer {
		if !t.IsObject() {
			return nil, &DataCloneError{"Found invalid value in transferList."}
		}
		o := t.Object()
		if b, ok := o.data.(*arrayBufferData); ok && o.class == ClassArrayBuffer && !b.shared {
			if _, dup := s.memory[o]; dup {
				return nil, &DataCloneError{"Transfer list contains duplicate ArrayBuffer"}
			}
			if b.detached || b.immutable || b.external {
				return nil, &DataCloneError{"Cannot transfer object of unsupported type."}
			}
			s.memory[o] = s.add(snode{kind: snArrayBuffer})
			buffers = append(buffers, o)
			continue
		}
		if codec != nil && codec.Transfer != nil {
			token, ok, err := codec.Transfer(t)
			if err != nil {
				return nil, err
			}
			if ok {
				s.memory[o] = s.add(snode{kind: snHost, token: token})
				continue
			}
		}
		return nil, &DataCloneError{"Found invalid value in transferList."}
	}
	root, err := s.value(v)
	if err != nil {
		return nil, err
	}
	s.out.root = root
	// Only once everything is serialized are the buffers taken: a failure
	// leaves them where they were.
	for _, o := range buffers {
		b := o.data.(*arrayBufferData)
		n := &s.out.nodes[s.memory[o]]
		n.bytes, n.resizable, n.max = b.bytes, b.resizable, b.maxByteLength
		b.bytes, b.detached = nil, true
	}
	return s.out, nil
}

func (s *serializer) add(n snode) int {
	s.out.nodes = append(s.out.nodes, n)
	return len(s.out.nodes) - 1
}

func (s *serializer) value(v Value) (int, error) {
	switch v.Kind() {
	case KindUndefined, KindNull, KindBool, KindNumber:
		return s.add(snode{prim: v.Kind(), num: v.num, str: boolString(v)}), nil
	case KindString:
		return s.add(snode{prim: KindString, str: v.String().Go()}), nil
	case KindBigInt:
		return s.add(snode{prim: KindBigInt, big: new(big.Int).Set(&v.BigInt().V)}), nil
	case KindSymbol:
		return 0, &DataCloneError{v.Symbol().String() + " could not be cloned."}
	}
	o := v.Object()
	if i, ok := s.memory[o]; ok {
		return i, nil
	}
	if s.depth >= maxCloneDepth {
		return 0, s.r.throwRangeError("maximum call stack size exceeded")
	}
	s.depth++
	defer func() { s.depth-- }()

	if p := proxyOf(o); p != nil {
		if p.target != nil && p.target.class == ClassArray {
			return 0, &DataCloneError{"[object Array] could not be cloned."}
		}
		// A proxy of a function is named by the function, as V8 names it.
		for t := p.target; t != nil; {
			if tp := proxyOf(t); tp != nil {
				t = tp.target
				continue
			}
			if t.IsCallable() {
				return 0, &DataCloneError{functionText(t) + " could not be cloned."}
			}
			break
		}
		return 0, &DataCloneError{"#<Object> could not be cloned."}
	}
	if o.IsCallable() {
		return 0, &DataCloneError{functionText(o) + " could not be cloned."}
	}
	// A host object is branded with how it clones, which its prototype --
	// that a script may change -- cannot say.
	brand, branded := o.data.(CloneBrand)
	if branded && o.class == ClassObject {
		switch brand {
		case CloneUnsupported:
			return 0, &DataCloneError{"Cannot clone object of unsupported type."}
		case CloneNeedsTransfer:
			return 0, &DataCloneError{"Object that needs transfer was found in message but not listed in transferList"}
		case CloneOpaque:
			// Its state is its own, and none of it is cloned.
			i := s.add(snode{kind: snObject})
			s.memory[o] = i
			return i, nil
		}
	}
	if s.codec != nil && s.codec.Serialize != nil && (branded ||
		o.class == ClassError || o.class == ClassObject && o.proto != s.r.proto.object && o.proto != nil) {
		token, ok, err := s.codec.Serialize(v)
		if err != nil {
			return 0, err
		}
		if ok {
			i := s.add(snode{kind: snHost, token: token})
			s.memory[o] = i
			return i, nil
		}
	}
	if branded && o.class == ClassObject {
		// A host object its host did not serialize has nothing else to it.
		return 0, &DataCloneError{"Cannot clone object of unsupported type."}
	}

	i := s.add(snode{})
	s.memory[o] = i
	n := snode{}
	switch o.class {
	case ClassBooleanWrapper, ClassNumberWrapper, ClassStringWrapper, ClassBigIntWrapper:
		prim := primitiveOf(o)
		n.kind = map[Class]snodeKind{
			ClassBooleanWrapper: snBooleanObject, ClassNumberWrapper: snNumberObject,
			ClassStringWrapper: snStringObject, ClassBigIntWrapper: snBigIntObject,
		}[o.class]
		n.prim, n.num, n.str = prim.Kind(), prim.num, boolString(prim)
		switch prim.Kind() {
		case KindString:
			n.str = prim.String().Go()
		case KindBigInt:
			n.big = new(big.Int).Set(&prim.BigInt().V)
		}
	case ClassSymbolWrapper:
		return 0, &DataCloneError{"[object Symbol] could not be cloned."}
	case ClassDate:
		n.kind = snDate
		d, ok := o.data.(*date.Date)
		if !ok {
			return 0, s.uncloneable(o)
		}
		n.num = d.Value()
	case ClassRegExp:
		d, ok := o.data.(*regexpData)
		if !ok {
			return 0, s.uncloneable(o)
		}
		n.kind, n.str, n.flag = snRegExp, d.re.Source(), d.re.Flags().String()
	case ClassArrayBuffer:
		b, ok := o.data.(*arrayBufferData)
		if !ok {
			return 0, s.uncloneable(o)
		}
		if b.block != nil {
			mem, _, err := s.r.SharedMemoryOf(v)
			if err != nil {
				return 0, &DataCloneError{err.Error()}
			}
			n.kind, n.mem = snSharedArrayBuffer, mem
			break
		}
		if b.detached {
			return 0, &DataCloneError{"An ArrayBuffer is detached and could not be cloned."}
		}
		n.kind = snArrayBuffer
		n.bytes = append([]byte(nil), b.bytes...)
		n.resizable, n.max = b.resizable, b.maxByteLength
	case ClassTypedArray, ClassDataView:
		var buf *Object
		switch d := o.data.(type) {
		case *typedArrayData:
			buf, n.elem, n.offset, n.count, n.tracking = d.buffer, d.kind, d.byteOffset, d.fixedLength, d.tracking
		case *dataViewData:
			buf, n.dataView, n.offset, n.count, n.tracking = d.buffer, true, d.byteOffset, d.byteLength, d.tracking
		default:
			return 0, s.uncloneable(o)
		}
		if b, ok := buf.data.(*arrayBufferData); ok && b.detached {
			return 0, &DataCloneError{"An ArrayBuffer is detached and could not be cloned."}
		}
		// A view its buffer has shrunk out from under is refused, as V8
		// refuses it, rather than cloned out of bounds.
		switch d := o.data.(type) {
		case *typedArrayData:
			if d.outOfBounds() {
				return 0, s.uncloneable(o)
			}
		case *dataViewData:
			if d.outOfBounds() {
				return 0, s.uncloneable(o)
			}
		}
		n.kind = snView
		bi, err := s.value(Obj(buf))
		if err != nil {
			return 0, err
		}
		n.buffer = bi
	case ClassMap, ClassSet:
		m, ok := o.data.(*jsMap)
		if !ok {
			return 0, s.uncloneable(o)
		}
		// The entries are copied first: serializing one may run a getter
		// that changes the collection.
		var entries []mapEntry
		for _, e := range m.entries {
			if !e.deleted() {
				entries = append(entries, e)
			}
		}
		n.kind = snMap
		if o.class == ClassSet {
			n.kind = snSet
		}
		for _, e := range entries {
			k, err := s.value(e.key)
			if err != nil {
				return 0, err
			}
			n.items = append(n.items, k)
			if o.class == ClassMap {
				val, err := s.value(e.value)
				if err != nil {
					return 0, err
				}
				n.items = append(n.items, val)
			}
		}
	case ClassError:
		if err := s.errorNode(o, &n); err != nil {
			return 0, err
		}
	case ClassArray:
		n.kind = snArray
		l, err := s.r.getProp(o, atomLength, v)
		if err != nil {
			return 0, err
		}
		length, err := s.r.toUint32(l)
		if err != nil {
			return 0, err
		}
		n.length = length
		if n.props, err = s.properties(o); err != nil {
			return 0, err
		}
	case ClassObject, ClassMathObject, ClassJSONObject:
		// Math and JSON are ordinary objects, as far as a clone can tell.
		if o.class == ClassObject && o.data != nil && !branded {
			return 0, s.uncloneable(o)
		}
		n.kind = snObject
		var err error
		if n.props, err = s.properties(o); err != nil {
			return 0, err
		}
	default:
		return 0, s.uncloneable(o)
	}
	s.out.nodes[i] = n
	return i, nil
}

// uncloneable is the DataCloneError for an object with internal slots
// nothing knows how to serialize, named as V8 names it.
func (s *serializer) uncloneable(o *Object) error {
	return &DataCloneError{s.r.describeReceiver(o) + " could not be cloned."}
}

// describeReceiver names an object as V8 does where it may run no code: by
// its constructor, "#<Promise>", when it would be converted to a string by
// Object.prototype.toString, and otherwise as that would, "[object
// Generator]" -- in both cases reading data properties only.
func (r *Runtime) describeReceiver(o *Object) string {
	data := func(key Atom) Value {
		for p := o; p != nil; p = p.proto {
			if proxyOf(p) != nil {
				return Undefined
			}
			if d := p.getOwnVisible(key); d != nil {
				if d.isAccessor() {
					return Undefined
				}
				return d.value
			}
		}
		return Undefined
	}
	if ts := data(r.atoms.intern("toString")); ts.IsObject() && ts.Object() == r.objectToStringFn {
		// The constructor's own name, as it was made, whatever its name
		// property now says.
		if c := data(atomConstructor); c.IsObject() {
			if fd := c.Object().fn(); fd != nil && fd.name != "" {
				return "#<" + fd.name + ">"
			}
		}
	}
	tag, err := r.builtinTag(o)
	if err != nil {
		tag = "Object"
	}
	if t := data(r.atoms.internSymbol(r.wellKnown.toStringTag)); t.IsString() {
		tag = t.String().Go()
	}
	return "[object " + tag + "]"
}

// properties serializes an object's own enumerable string-keyed properties,
// each read as a script would read it.
func (s *serializer) properties(o *Object) ([]sprop, error) {
	keys, err := s.r.ownKeysOf(o, false)
	if err != nil {
		return nil, err
	}
	// Which keys are enumerable is settled once, before any is read, as
	// EnumerableOwnProperties settles it: a getter that makes a later key
	// non-enumerable does not take it out.
	var enumerable []Atom
	for _, k := range keys {
		e, err := s.r.isEnumerable(o, k)
		if err != nil {
			return nil, err
		}
		if e {
			enumerable = append(enumerable, k)
		}
	}
	var props []sprop
	for _, k := range enumerable {
		// A getter run for an earlier property may have removed this one.
		if !s.r.hasOwnProp(o, k) {
			continue
		}
		v, err := s.r.getProp(o, k, Obj(o))
		if err != nil {
			return nil, err
		}
		vi, err := s.value(v)
		if err != nil {
			return nil, err
		}
		props = append(props, sprop{key: s.r.atoms.name(k), val: vi})
	}
	return props, nil
}

// clonedErrorKinds are the errors a clone keeps the kind of; any other is an
// Error.
var clonedErrorKinds = map[string]errorKind{
	"Error": errError, "EvalError": errEval, "RangeError": errRange,
	"ReferenceError": errReference, "SyntaxError": errSyntax,
	"TypeError": errType, "URIError": errURI,
}

// errorNode serializes an error: its kind as its name says, and its message,
// stack and cause.
func (s *serializer) errorNode(o *Object, n *snode) error {
	n.kind = snError
	this := Obj(o)
	name, err := s.r.getProp(o, atomName, this)
	if err != nil {
		return err
	}
	if name.IsString() {
		n.errKind = clonedErrorKinds[name.String().Go()]
	}
	if p := o.getOwn(atomMessage); p != nil && !p.isAccessor() {
		msg, err := s.r.toString(p.value)
		if err != nil {
			return err
		}
		n.hasMessage, n.str = true, msg.Go()
	}
	stack, err := s.r.getProp(o, atomStack, this)
	if err != nil {
		return err
	}
	if stack.IsString() {
		n.hasStack, n.stack = true, stack.String().Go()
	}
	causeKey := s.r.atoms.intern("cause")
	if p := o.getOwn(causeKey); p != nil && !p.isAccessor() {
		ci, err := s.value(p.value)
		if err != nil {
			return err
		}
		n.hasCause, n.items = true, []int{ci}
	}
	return nil
}

func boolString(v Value) string {
	if v.Kind() == KindBool && v.BoolValue() {
		return "true"
	}
	return ""
}

// --- Deserializing -------------------------------------------------------------

// deserializer is one deserialization in progress.
type deserializer struct {
	r     *Runtime
	codec *Codec
	in    *Serialized
	made  []Value
	done  []bool
}

// Deserialize makes in this runtime, in the realm running now, the value s
// holds. A serialized value is deserialized once: the bytes of a transferred
// buffer go with it.
func (r *Runtime) Deserialize(s *Serialized, codec *Codec) (Value, error) {
	d := &deserializer{r: r, codec: codec, in: s, made: make([]Value, len(s.nodes)), done: make([]bool, len(s.nodes))}
	return d.value(s.root)
}

func (d *deserializer) value(i int) (Value, error) {
	if d.done[i] {
		return d.made[i], nil
	}
	n := &d.in.nodes[i]
	r := d.r
	var o *Object
	switch n.kind {
	case snPrimitive:
		v := primitiveValue(n)
		d.made[i], d.done[i] = v, true
		return v, nil
	case snBooleanObject, snNumberObject, snStringObject, snBigIntObject:
		w, err := r.toObject(primitiveValue(n))
		if err != nil {
			return Undefined, err
		}
		o = w
	case snDate:
		o = newObject(r.proto.date, ClassDate)
		o.data = r.newDate(n.num)
	case snRegExp:
		v, err := r.newRegExp(n.str, n.flag)
		if err != nil {
			return Undefined, err
		}
		o = v.Object()
	case snArrayBuffer:
		o = newObject(r.arrayBufferProto, ClassArrayBuffer)
		bytes := n.bytes
		if bytes == nil {
			bytes = []byte{}
		}
		o.data = &arrayBufferData{bytes: bytes, resizable: n.resizable, maxByteLength: n.max}
		// Moved bytes belong to this buffer now, and nothing else.
		n.bytes = nil
	case snSharedArrayBuffer:
		o = r.NewSharedArrayBuffer(n.mem).Object()
	case snHost:
		if d.codec == nil || d.codec.Revive == nil {
			return Undefined, &DataCloneError{"the value cannot be deserialized here"}
		}
		v, err := d.codec.Revive(n.token)
		if err != nil {
			return Undefined, err
		}
		d.made[i], d.done[i] = v, true
		return v, nil
	case snView:
		bv, err := d.value(n.buffer)
		if err != nil {
			return Undefined, err
		}
		buf := bv.Object()
		if n.dataView {
			o = newObject(r.intrinsicNamed("DataView"), ClassDataView)
			o.data = &dataViewData{buffer: buf, byteOffset: n.offset, byteLength: n.count, tracking: n.tracking}
		} else {
			o = newObject(r.typedArrayProtos[n.elem], ClassTypedArray)
			o.data = &typedArrayData{buffer: buf, kind: n.elem, byteOffset: n.offset, fixedLength: n.count, tracking: n.tracking}
		}
	case snError:
		o = newErrorObject(r.proto.nativeErrors[n.errKind])
		if n.hasStack {
			o.setOwnRaw(atomStack, Str(NewString(n.stack)), propWritable|propConfigurable)
		}
		if n.hasMessage {
			o.setOwnRaw(atomMessage, Str(NewString(n.str)), propWritable|propConfigurable)
		}
	case snMap:
		o = newObject(r.proto.mapProto, ClassMap)
		o.data = newJSMap()
	case snSet:
		o = newObject(r.proto.setProto, ClassSet)
		o.data = newJSMap()
	case snArray:
		o = r.newArrayOfLength(0)
	default:
		o = newObject(r.proto.object, ClassObject)
	}
	d.made[i], d.done[i] = Obj(o), true

	// What an object holds is made once the object is, so that a cycle back
	// to it finds it.
	switch n.kind {
	case snError:
		if n.hasCause {
			c, err := d.value(n.items[0])
			if err != nil {
				return Undefined, err
			}
			o.setOwnRaw(r.atoms.intern("cause"), c, propWritable|propConfigurable)
		}
	case snMap, snSet:
		m := o.data.(*jsMap)
		step := 1
		if n.kind == snMap {
			step = 2
		}
		for j := 0; j < len(n.items); j += step {
			k, err := d.value(n.items[j])
			if err != nil {
				return Undefined, err
			}
			v := k
			if step == 2 {
				if v, err = d.value(n.items[j+1]); err != nil {
					return Undefined, err
				}
			}
			m.set(r, k, v)
		}
	case snArray:
		if _, err := r.setProp(o, atomLength, Float(float64(n.length)), Obj(o), true); err != nil {
			return Undefined, err
		}
		fallthrough
	case snObject:
		for _, p := range n.props {
			v, err := d.value(p.val)
			if err != nil {
				return Undefined, err
			}
			if err := r.createDataProperty(o, r.atoms.intern(p.key), v, propDefault); err != nil {
				return Undefined, err
			}
		}
	}
	return Obj(o), nil
}

// primitiveValue is the primitive a node holds.
func primitiveValue(n *snode) Value {
	switch n.prim {
	case KindUndefined:
		return Undefined
	case KindNull:
		return Null
	case KindBool:
		return Bool(n.str == "true")
	case KindNumber:
		return Float(n.num)
	case KindString:
		return Str(NewString(n.str))
	case KindBigInt:
		b := &BigInt{}
		b.V.Set(n.big)
		return Big(b)
	}
	return Undefined
}

// primitiveOf is the primitive a wrapper object holds.
func primitiveOf(o *Object) Value {
	switch d := o.data.(type) {
	case *String:
		return Str(d)
	case float64:
		return Float(d)
	case bool:
		return Bool(d)
	case *Symbol:
		return Sym(d)
	case *BigInt:
		return Big(d)
	}
	return Undefined
}

// functionText is what Function.prototype.toString says of a function.
func functionText(o *Object) string {
	fd := o.fn()
	switch {
	case fd == nil:
		return "function () { [native code] }"
	case fd.closure != nil && fd.closure.fn.Text != "":
		return fd.closure.fn.Text
	}
	return "function " + nativeFunctionName(fd.nameOr("")) + "() { [native code] }"
}

// intrinsicNamed is the current realm's intrinsic prototype of a name.
func (r *Runtime) intrinsicNamed(name string) *Object {
	for _, in := range r.names {
		if in.name == name {
			return in.proto
		}
	}
	return nil
}
