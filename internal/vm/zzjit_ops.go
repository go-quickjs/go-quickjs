//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

// jitOpPlain makes an operation Go makes for native code (abi.GoCallOp,
// the lowering's GoOperation) without a frame, where that runs no script
// and throws nothing (jitOpFrameless), as V8's CloneObject and
// CopyDataProperties builtins take their fast paths: it writes the result
// to the call's keep cell and reports whether it made it. What it does not
// make it leaves as it was, for jitGoCall to make with the code's frame.
func (r *Runtime) jitOpPlain(ctx *abi.Context) bool {
	// The instruction as the call's word has it, its opcode and its A
	// (jitGoCallInstr): the code's closure and its bytecode, three loads
	// one after another, are read only for what needs them.
	in, n := jitGoCallInstr(ctx.GoOp)
	if n > abi.MaxGoArgs || n != jitcompile.GoOperands(in) {
		return false
	}
	var f *frame
	var cl *closure
	switch in.Op {
	case bytecode.OpTemplateObject, bytecode.OpPushCallee:
		if f, cl = r.jitOpCode(ctx, in); cl == nil {
			return false
		}
	}
	args := unsafe.Slice((*Value)(unsafe.Pointer(&ctx.GoArgs[0])), n)
	v, ok := r.jitOpFrameless(f, cl, in, args)
	if !ok {
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitOpCode is the frame and the closure of the code that makes in, an
// operation Go makes for native code in ctx (jitOpPlain): the run's frame
// and its closure, or a native callee's closure, or an inlined callee's,
// with no frame -- or nil where the instruction at the call's PC is not
// in.
func (r *Runtime) jitOpCode(ctx *abi.Context, in bytecode.Instr) (*frame, *closure) {
	s, idx := r.jit, int(ctx.Level)
	if idx >= len(s.ssaCtxs) || s.ssaCtxs[idx] != ctx {
		return nil, nil
	}
	var cl *closure
	var f *frame
	switch ctx.Live {
	case 0:
		if idx >= len(s.goRuns) || s.goRuns[idx].f == nil {
			return nil, nil
		}
		f = s.goRuns[idx].f
		cl = f.cl
	case abi.LiveCall:
		cl = (*closure)(ctx.Closure)
	}
	pc := int(ctx.ExitPC)
	if st := jitGoCallState(ctx); st != nil && st.Inline != 0 {
		cl, pc, f = r.jitGoInlined(ctx, cl, st.Inline), int(st.InlinePC), nil
	}
	if cl == nil || pc >= len(cl.fn.Code) || cl.fn.Code[pc] != in {
		return nil, nil
	}
	return f, cl
}

// jitOpFrameless is in's result, of its operands args, where making it
// runs no script and throws nothing, as the interpreter makes it; f is the
// frame of the code that makes it, nil for a native callee's: object
// spread and rest of what has no getter and is no proxy (jitCopyPlain), of
// keys that are primitives; array spread of a dense array that iterates as
// Array.prototype does; an element appended; a literal's __proto__; a key
// that is a primitive, converted, or defined on an ordinary object; a
// tagged template's strings; the frame's function. It reports false for
// the rest, having changed nothing.
func (r *Runtime) jitOpFrameless(f *frame, cl *closure, in bytecode.Instr, args []Value) (Value, bool) {
	if r.meter != nil {
		// What they allocate is charged to the memory limit, which may
		// throw.
		return Undefined, false
	}
	switch in.Op {
	case bytecode.OpCopyDataProps:
		target, src := args[0], args[1]
		if !target.IsObject() || src.IsNullish() {
			return target, true
		}
		t := target.Object()
		if t.class != ClassObject {
			return Undefined, false
		}
		if !src.IsObject() {
			// A string's characters, or nothing.
			return target, r.copyDataProps(t, src) == nil
		}
		o := src.Object()
		if r.jitCopyFast(t, o) {
			return target, true
		}
		if !jitCopyPlain(o) {
			return Undefined, false
		}
		from, n := t.shape, len(t.vals)
		// Nothing such a copy does throws; were it to, the properties it
		// defined are defined again, the same, with the frame.
		if r.copyDataProps(t, src) != nil {
			return target, false
		}
		r.jitCopyLearn(t, o, from, n)
		return target, true
	case bytecode.OpArraySpread, bytecode.OpSpreadIter:
		target, src := args[0], args[1]
		els, ok := r.jitDenseElems(src)
		if !ok {
			return Undefined, false
		}
		if target.IsObject() {
			t := target.Object()
			t.extOf().elems = append(t.elemsOf(), els...)
		}
		return target, true
	case bytecode.OpArrayPush:
		if arr := args[0]; arr.IsObject() {
			o := arr.Object()
			o.extOf().elems = append(o.elemsOf(), args[1])
		}
		return args[0], true
	case bytecode.OpSetProtoOf:
		obj, val := args[0], args[1]
		if obj.IsObject() {
			switch {
			case val.IsObject():
				obj.Object().proto = val.Object()
			case val.IsNull():
				obj.Object().proto = nil
			}
		}
		return obj, true
	case bytecode.OpObjectRest:
		src, excluded := args[0], args[1:]
		if src.IsObject() && !jitCopyPlain(src.Object()) {
			return Undefined, false
		}
		for _, k := range excluded {
			if k.IsObject() {
				return Undefined, false
			}
		}
		if src.IsObject() {
			if out, ok := r.jitRestFast(src.Object(), excluded); ok {
				return Obj(out), true
			}
		}
		v, err := r.objectRest(src, excluded)
		return v, err == nil
	case bytecode.OpTemplateObject:
		return Obj(r.templateObject(cl.fn, int(in.A))), true
	case bytecode.OpPushCallee:
		if f == nil {
			return Undefined, false
		}
		if f.callee == nil {
			return Undefined, true
		}
		return Obj(f.callee), true
	case bytecode.OpDefineIndex:
		obj, key, val := args[0], args[1], args[2]
		if key.IsObject() {
			return Undefined, false
		}
		if !obj.IsObject() {
			return obj, true
		}
		o := obj.Object()
		if o.class != ClassObject && o.class != ClassFunction {
			return Undefined, false
		}
		k, err := r.toPropertyKey(key)
		if err != nil {
			return Undefined, false
		}
		flags := memberFlags(in.A)
		if o.class == ClassObject && o.flags&objExtensible != 0 && o != r.global {
			// An ordinary object's, as a literal's: CreateDataProperty of a
			// key it has not, or has as a property it may redefine, defines
			// it as defineOwnProp does (defineProperty's installProperty).
			if i := o.findOwn(k); i < 0 || o.shape.keys[i].flags&propConfigurable != 0 {
				return obj, r.defineOwnProp(o, k, val, flags) == nil
			}
		}
		ok, err := r.defineProperty(o, k, &propDesc{
			value: val, hasValue: true,
			writable: flags&propWritable != 0, hasWritable: true,
			enumerable: flags&propEnumerable != 0, hasEnumerable: true,
			configurable: flags&propConfigurable != 0, hasConfigurable: true,
		})
		// One it refused changed nothing: Go throws its TypeError with the
		// frame.
		return obj, ok && err == nil
	case bytecode.OpDefineField:
		// A literal's field, as define_field defines it: along the
		// object's transition, or as any own property, which runs nothing
		// on an ordinary extensible object.
		obj, val := args[0], args[1]
		if !obj.IsObject() {
			return obj, true
		}
		o := obj.Object()
		if o.class != ClassObject || o.flags&objExtensible == 0 {
			return Undefined, false
		}
		// The key's atom, which the call's word has in place of its name's
		// index (the VM's NativeCalls).
		key := Atom(in.A)
		if next := o.transition(key, propDefault); next != nil {
			o.appendTransition(val, next)
			return obj, true
		}
		return obj, r.defineOwnProp(o, key, val, propDefault) == nil
	case bytecode.OpToPropertyKey:
		v := args[0]
		if v.IsString() || v.IsSymbol() {
			return v, true
		}
		if v.IsObject() {
			return Undefined, false
		}
		k, err := r.toPropertyKey(v)
		if err != nil {
			return Undefined, false
		}
		return r.keyToValue(k), true
	}
	return Undefined, false
}

// jitDenseElems is plainDenseElems: an array's elements where reading them
// is spreading it, as its iteration would -- an array of no hole whose
// iteration is Array.prototype's -- told, for an array of the realm's
// prototype with no property of its own, by the validity of the chains of
// Array.prototype and %ArrayIteratorPrototype% and the two methods' values
// there, as V8's code checks its ArrayIteratorProtector, rather than by
// looking them up at each spread.
func (r *Runtime) jitDenseElems(src Value) ([]Value, bool) {
	if !src.IsObject() {
		return nil, false
	}
	o := src.Object()
	if o.class != ClassArray || o.proto != r.proto.array || len(o.vals) != 0 || o.flags&objHasSparseElements != 0 || !r.jitArrayIterates() {
		return r.plainDenseElems(src)
	}
	els := o.elemsOf()
	for i := range els {
		if holeAt(&els[i]) {
			return nil, false
		}
	}
	return els, true
}

// jitArrayIter is where an array of the realm's prototype with no
// property of its own finds its iteration's methods (jitArrayIterates):
// the validity cells of the chains from Array.prototype and from
// %ArrayIteratorPrototype%, and the places there of Symbol.iterator and
// of next.
type jitArrayIter struct {
	array, iter       *protoCell
	valuesAt, nexts   *Object
	values, next      int32
	proto             *Object
	valuesFn, nextFns *Object
}

// Its layout is what native code reads (abi.IterCellArray).
var _ [0]struct{} = [unsafe.Offsetof(jitArrayIter{}.array) - abi.IterCellArray]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitArrayIter{}.iter) - abi.IterCellIter]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitArrayIter{}.valuesAt) - abi.IterValuesObj]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitArrayIter{}.nexts) - abi.IterNextObj]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitArrayIter{}.values) - abi.IterValuesAt]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitArrayIter{}.next) - abi.IterNextAt]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitArrayIter{}.proto) - abi.IterProto]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitArrayIter{}.valuesFn) - abi.IterValuesFn]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitArrayIter{}.nextFns) - abi.IterNextFn]struct{}{}

// jitArrayIterates reports whether an array of the realm's prototype with
// no property of its own iterates as Array.prototype's intrinsics do
// (usesIntrinsicArrayIterator): while neither chain's layout has changed
// since it was found so, its two methods are where they were, holding the
// intrinsics still.
func (r *Runtime) jitArrayIterates() bool {
	c := &r.jit.arrayIter
	if c.array == nil || !c.array.valid || !c.iter.valid {
		*c = jitArrayIter{}
		ap, ip := r.proto.array, r.proto.arrayIter
		key := r.atoms.internSymbol(r.wellKnown.iterator)
		values, next := ap.findOwn(key), ip.findOwn(atomNext)
		if values < 0 || next < 0 || ap.shape.keys[values].flags&propAccessor != 0 || ip.shape.keys[next].flags&propAccessor != 0 {
			return false
		}
		array, iter := r.chainCell(ap), r.chainCell(ip)
		if array == nil || iter == nil {
			return false
		}
		*c = jitArrayIter{array: array, iter: iter, values: values, next: next, valuesAt: ap, nexts: ip,
			proto: ap, valuesFn: r.arrayValuesFn, nextFns: r.arrayIterNextFn}
	}
	return c.valuesAt.vals[c.values].ref == unsafe.Pointer(r.arrayValuesFn) && c.nexts.vals[c.next].ref == unsafe.Pointer(r.arrayIterNextFn)
}

// jitCopyEntries is how many object spreads the runtime remembers how to
// make by their layouts (jitState's copies), which native code reads too
// (abi.CopyBits).
const jitCopyEntries = 1 << abi.CopyBits

// jitCopyEntry is an object spread Go made by the layouts, as V8's
// CloneObjectIC remembers a source's map and the result's: a source of
// layout src copied into a target of layout from, which the copy took to
// to, appending the source's values at slots, in order -- its enumerable
// own keys' (shapeKeys), none of which the target had.
type jitCopyEntry struct {
	src, from, to *shape
	slots         []int32
}

// The table's layout is what native code reads (abi.CopyEntrySize).
var _ [0]struct{} = [unsafe.Sizeof(jitCopyEntry{}) - abi.CopyEntrySize]struct{}{}
var _ [0]struct{} = [abi.CopyEntrySize - unsafe.Sizeof(jitCopyEntry{})]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitCopyEntry{}.src) - abi.CopySrc]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitCopyEntry{}.from) - abi.CopyFrom]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitCopyEntry{}.to) - abi.CopyTo]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(jitCopyEntry{}.slots) - abi.CopySlots]struct{}{}

// jitCopySlot is where an object spread of a source of layout src into a
// target of layout from is remembered (jitState's copies), as native code
// finds it (abi.CopyMul).
func jitCopySlot(src, from *shape) int {
	h := uint64(uintptr(unsafe.Pointer(src)))>>4 ^ uint64(uintptr(unsafe.Pointer(from)))>>6
	return int((h * abi.CopyMul) >> (64 - abi.CopyBits))
}

// jitCopyFast makes object spread of o into t, an ordinary object, as an
// earlier spread of the same layouts made it (jitCopyLearn): the source's
// values appended, the target given the layout the copy took it to, with
// no key looked up -- as V8's CloneObjectIC copies a fast object's
// properties by its map. It reports whether it made it.
func (r *Runtime) jitCopyFast(t, o *Object) bool {
	c := r.jit.copies
	s, from := o.shape, t.shape
	if c == nil || s == nil || from == nil || o.class != ClassObject {
		return false
	}
	e := &c[jitCopySlot(s, from)]
	if e.src != s || e.from != from || len(t.vals) != int(from.n) || len(o.vals) != int(s.n) {
		return false
	}
	n := len(t.vals) + len(e.slots)
	if cap(t.vals) < n {
		t.reserveProps(n)
	}
	for _, i := range e.slots {
		t.vals = append(t.vals, o.vals[i])
	}
	t.shape = e.to
	if e.to.index == nil && len(t.vals) > linearScanLimit {
		t.buildIndex()
	}
	if verifyShapes {
		checkShape(t)
	}
	return true
}

// jitCopyLearn remembers how object spread of o into t made it, t having
// had layout from and n values before (jitCopyFast): where o is a plain
// object of a shared layout whose enumerable own keys are all data
// (shapeKeys), none of which t had -- each appended -- and t's layouts
// before and after are shared, the second the first's descendant by
// those keys, in their order, with the default attributes.
func (r *Runtime) jitCopyLearn(t, o *Object, from *shape, n int) {
	c := r.shapeKeys(o)
	to := t.shape
	if c == nil || !c.copyable || from == nil || to == nil || from.unique || from.loose || to.unique || to.loose ||
		n != int(from.n) || len(t.vals) != n+len(c.slots) || len(o.vals) != int(o.shape.n) ||
		to.index == nil && int(to.n) > linearScanLimit {
		// Native code makes it as remembered (abi.CopyBits), and builds no
		// index: one the layout needs it has, made by this copy.
		return
	}
	k := to
	for i := len(c.atoms) - 1; i >= 0; i-- {
		if k == nil || k.key != c.atoms[i] || k.flags != propDefault {
			return
		}
		k = k.parent
	}
	if k != from {
		return
	}
	if r.jit.copies == nil {
		r.jit.copies = new([jitCopyEntries]jitCopyEntry)
	}
	r.jit.copies[jitCopySlot(o.shape, from)] = jitCopyEntry{src: o.shape, from: from, to: to, slots: c.slots}
}

// jitRestFast is objectRest of o, a plain object of a shared layout whose
// enumerable own keys are all data under string keys (shapeKeys), less the
// keys excluded, strings: its values read where the layout has them, in
// its keys' order, each appended to the rest object, with no key looked up
// on o -- or false, for objectRest to make.
func (r *Runtime) jitRestFast(o *Object, excluded []Value) (*Object, bool) {
	c := r.shapeKeys(o)
	if c == nil || !c.copyable || len(excluded) > 8 {
		return nil, false
	}
	var skip [8]Atom
	for i, k := range excluded {
		if !k.IsString() {
			return nil, false
		}
		skip[i] = r.atoms.intern(k.String().Go())
	}
	out := newObject(r.proto.object, ClassObject)
	if len(c.atoms) > len(excluded) {
		out.reserveProps(len(c.atoms) - len(excluded))
	}
	for i, k := range c.atoms {
		if slices.Contains(skip[:len(excluded)], k) {
			continue
		}
		// A key of its own, none before it the same.
		out.appendProp(Property{key: k, flags: propDefault, value: o.vals[c.slots[i]]})
	}
	return out, true
}

// jitCopyPlain reports whether copying o's own enumerable properties, as
// object spread and rest do, runs no script: an ordinary object or array,
// none of whose enumerable properties is an accessor -- no proxy, whose
// traps would run, no module namespace, whose bindings may throw.
func jitCopyPlain(o *Object) bool {
	if o.class != ClassObject && o.class != ClassArray {
		return false
	}
	for _, k := range o.keys() {
		if k.flags&(propAccessor|propEnumerable|propDeleted) == propAccessor|propEnumerable {
			return false
		}
	}
	return true
}

// jitOpMade makes in, an operation Go makes for native code (abi.GoCallOp,
// the lowering's GoOperation), with f's frame, of its operands args, as
// the interpreter does: what it runs -- a getter, a proxy's trap, an
// iterator's next, a key's toString, a spread call's callee -- may run
// anything, and what it throws is the frame's.
func (r *Runtime) jitOpMade(f *frame, in bytecode.Instr, args []Value) (Value, error) {
	switch in.Op {
	case bytecode.OpCopyDataProps:
		target, src := args[0], args[1]
		if target.IsObject() && !src.IsNullish() {
			if err := r.copyDataProps(target.Object(), src); err != nil {
				return Undefined, err
			}
		}
		return target, nil
	case bytecode.OpArraySpread:
		target, src := args[0], args[1]
		if target.IsObject() {
			if err := r.spreadInto(target.Object(), src); err != nil {
				return Undefined, err
			}
		}
		return target, nil
	case bytecode.OpSpreadIter:
		target, src := args[0], args[1]
		vals, err := r.spreadToStack(src)
		if err != nil {
			return Undefined, err
		}
		if target.IsObject() {
			t := target.Object()
			t.extOf().elems = append(t.elemsOf(), vals...)
		}
		return target, nil
	case bytecode.OpObjectRest:
		return r.objectRest(args[0], args[1:])
	case bytecode.OpCallSpread, bytecode.OpNewSpread:
		argsVal := args[len(args)-1]
		var callArgs []Value
		if argsVal.IsObject() {
			callArgs = argsVal.Object().elemsOf()
		}
		if in.Op == bytecode.OpNewSpread {
			return r.construct(args[0], callArgs)
		}
		// A spread call has a receiver beneath its callee, undefined for
		// a plain call.
		return r.call(args[1], args[0], callArgs)
	case bytecode.OpDefineIndex:
		obj, key, val := args[0], args[1], args[2]
		k, err := r.toPropertyKey(key)
		if err != nil {
			return Undefined, err
		}
		if obj.IsObject() {
			if err := r.createDataProperty(obj.Object(), k, val, memberFlags(in.A)); err != nil {
				return Undefined, err
			}
		}
		return obj, nil
	case bytecode.OpToPropertyKey:
		v := args[0]
		if v.IsString() || v.IsSymbol() {
			return v, nil
		}
		k, err := r.toPropertyKey(v)
		if err != nil {
			return Undefined, err
		}
		return r.keyToValue(k), nil
	case bytecode.OpDefineField:
		if obj := args[0]; obj.IsObject() {
			if err := r.defineOwnProp(obj.Object(), f.cl.names[in.A], args[1], propDefault); err != nil {
				return Undefined, err
			}
		}
		return args[0], nil
	}
	if v, ok := r.jitOpFrameless(f, f.cl, in, args); ok {
		return v, nil
	}
	// What the frameless path makes always, it refuses only with a
	// memory limit: made here as it would be.
	switch in.Op {
	case bytecode.OpArrayPush:
		if arr := args[0]; arr.IsObject() {
			o := arr.Object()
			o.extOf().elems = append(o.elemsOf(), args[1])
		}
		return args[0], nil
	case bytecode.OpSetProtoOf:
		obj, val := args[0], args[1]
		if obj.IsObject() {
			switch {
			case val.IsObject():
				obj.Object().proto = val.Object()
			case val.IsNull():
				obj.Object().proto = nil
			}
		}
		return obj, nil
	case bytecode.OpTemplateObject:
		return Obj(r.templateObject(f.cl.fn, int(in.A))), nil
	case bytecode.OpPushCallee:
		if f.callee == nil {
			return Undefined, nil
		}
		return Obj(f.callee), nil
	}
	panic("jit: not an operation Go makes: " + in.Op.String())
}

// jitPushesCallee reports whether fn pushes its own function object
// (push_callee), which the frame Go makes has (frame's callee) and a
// native callee's context does not: its code has no call entry, and Go
// makes native callers' calls of it (ssa.Func's CallEntry).
func jitPushesCallee(fn *bytecode.Function) bool {
	for _, in := range fn.Code {
		if in.Op == bytecode.OpPushCallee {
			return true
		}
	}
	return false
}
