package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The Go handlers' fast paths that the native handlers have too
// (nativegen), for the Go table, the fallback of every other
// architecture and of QJS_NONATIVE: in a file of their own, sorting after
// the handlers zzreg_table.go holds, whose entries this file's init
// replaces, so that what is added here moves no handler there
// (docs/register-interpreter.md, "The loop").
func init() {
	regTable[bytecode.RBin], regSlowTable[bytecode.RBin] = rhBin, rsBin
	regTable[bytecode.RSetIndexI], regSlowTable[bytecode.RSetIndexI] = rhSetIndexITyped, rsSetIndexITyped
	regTable[bytecode.RSetIndex] = rhSetIndexTyped
	regTable[bytecode.RGetPrivate], regSlowTable[bytecode.RGetPrivate] = rhGetPrivateSite, rhGetPrivate
	regTable[bytecode.RSetPrivate], regSlowTable[bytecode.RSetPrivate] = rhSetPrivateSite, rhSetPrivate
	regTable[bytecode.RIn], regSlowTable[bytecode.RIn] = rhIn, rsIn
	regSlowTable[bytecode.RGetMethodW] = rsGetMethodWPrim
	regSlowTable[bytecode.RSetProp] = rsSetPropLength
}

// rhBin is bin's fast path, as native code's (nativegen's bin): a
// division of two numbers, and a remainder of two int32s, the first above
// zero and the second not zero, as jsMod takes them, into a register that
// holds no pointer. Any other operator, operand or remainder is rsBin's.
func rhBin(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && b.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			switch bytecode.Op(w.X()) {
			case bytecode.OpDiv:
				p.num = regNum(a.Number() / b.Number()).num
				return pc + 1
			case bytecode.OpMod:
				if x, y := int32(a.Number()), int32(b.Number()); float64(x) == a.Number() && float64(y) == b.Number() && x > 0 && y != 0 {
					p.num = float64(x % y)
					return pc + 1
				}
			}
		}
	}
	return ^pc
}

// rsSetIndexITyped is set_index_i's slow path: a number stored into a
// typed array's element as set_index's miss stores it (regSetMiss, by
// regTypedAt and regTypedStore), a Uint8ClampedArray's clamped as
// typedStore clamps it, a BigInt held in its Value into a
// BigInt64Array's or a BigUint64Array's (regTypedBigStore), and
// rsSetIndexI's otherwise.
func rsSetIndexITyped(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if obj, v := regs[w.A()], regs[w.C()]; obj.IsObject() {
		if v.IsNumber() {
			if t, p := regTypedAt(st.r, obj.object(), float64(w.B())); t != nil {
				if regTypedStore(t, p, v.num) {
					return pc + 1
				}
				if t.kind == elemUint8Clamped && !t.readonly {
					// Clamped and rounded to even, as typedStore stores it.
					*(*uint8)(p) = clampUint8(v.num)
					return pc + 1
				}
			}
		} else if regTypedBigStore(st.r, obj.object(), float64(w.B()), v) {
			return pc + 1
		}
	}
	return rsSetIndexI(regs, w, pc, st)
}

// regTypedBigStore stores v, a BigInt held in its Value, as element k of
// o, a BigInt64Array's or a BigUint64Array's, as native code stores it:
// its two's complement's low 64 bits, which ToBigInt64 and ToBigUint64
// both keep, from the view's data pointer while its epoch is the
// runtime's. False -- another object or value, a BigInt on the heap, a
// key that is no index in the view, a stale or read-only view -- leaves
// the store to the slow path.
func regTypedBigStore(r *Runtime, o *Object, k float64, v Value) bool {
	if o.class != ClassTypedArray || !v.isShortBig() {
		return false
	}
	t, ok := o.dataOf().(*typedArrayData)
	if !ok || t.epoch != r.typedEpoch || t.kind < elemBigInt64 || t.readonly {
		return false
	}
	if i := uint32(k); float64(i) == k && uint(i) < uint(t.length) {
		*(*uint64)(unsafe.Add(t.data, uintptr(i)<<3)) = v.shortBigLow()
		return true
	}
	return false
}

// rhGetPrivateSite is get_private's fast path, as native code's: the
// member at the site's own entry (Aux's high 16 bits) for an object of
// its shape, where the key there is the private name in R[C], a symbol
// the runtime's atoms interned. Anything else is rhGetPrivate's, which
// fills the site.
func rhGetPrivateSite(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if i, ok := regPrivateSlot(regs[w.B()], regs[w.C()], pc, st); ok {
		regs[w.A()] = regs[w.B()].object().vals()[i]
		return pc + 1
	}
	return ^pc
}

// rhSetPrivateSite is set_private's fast path, as rhGetPrivateSite's: a
// writable field's, which the site remembers only of a writable field.
func rhSetPrivateSite(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if i, ok := regPrivateSlot(regs[w.A()], regs[w.C()], pc, st); ok {
		regs[w.A()].object().vals()[i] = regs[w.B()]
		return pc + 1
	}
	return ^pc
}

// regPrivateSlot is the index of private name name's member in o's values
// where the site of the instruction at pc remembers it for o's shape:
// regGetPrivate's first check, with the name's atom read from the symbol
// the runtime's atoms interned rather than looked up.
func regPrivateSlot(o, name Value, pc int, st *regState) (int, bool) {
	if !o.IsObject() || !name.IsSymbol() {
		return 0, false
	}
	s := name.Symbol()
	site := st.c.rc.Aux[pc] >> 16
	if site == 0 || s.atoms != st.r.atoms {
		return 0, false
	}
	ob, e := o.object(), &st.cl.ic[site]
	if ob.shape != e.ownShape || ob.shape.keys[e.idx].key != s.atom {
		return 0, false
	}
	return int(e.idx), true
}

// rhIn is in's fast path, R[A] = R[B] in R[C], as native code's: a
// string key's answer the realm's in cache remembers of the object's
// layout and chain (hasCached), and an index's of an array and its chain
// (hasIndexQuick), neither of which throws or runs code. Anything else is
// rsIn's, which fills the cache.
func rhIn(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if obj := regs[w.C()]; obj.IsObject() {
		key, o := regs[w.B()], obj.Object()
		if i := uint32(key.num); float64(i) == key.num && i < atomIndexTag {
			if has, ok := hasIndexQuick(o, i); ok {
				regs[w.A()] = Bool(has)
				return pc + 1
			}
		} else if has, ok := hasCached(st.r, key, o); ok {
			regs[w.A()] = Bool(has)
			return pc + 1
		}
	}
	return ^pc
}

// rsGetMethodWPrim is get_method_w's slow path: a string's or a number's
// method, from the realm's cache of their prototypes' data properties
// (primProps), as native code reads it and regGetMethod does, with no
// pc kept, which a hit does not need; rsGetMethodW's otherwise.
func rsGetMethodWPrim(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	o := regs[w.B()]
	var p *Object
	if o.IsString() {
		p = st.r.proto.str
	} else if o.IsNumber() {
		p = st.r.proto.number
	}
	if p != nil {
		name := st.cl.names[st.c.siteName[w.C()]]
		if e := &st.r.primProps[name&63]; e.key == name && e.proto == p && e.shape == p.shape {
			if verifyShapes {
				checkShape(p)
			}
			win := (*[2]Value)(unsafe.Pointer(&regs[w.A()]))
			win[0], win[1] = p.vals()[e.idx], o
			return pc + 1
		}
	}
	return rsGetMethodW(regs, w, pc, st)
}

// rsSetPropLength is set_prop's slow path: an array's own length written
// with a number that is a length, on a dense array whose length may be
// written, as setProp's first case writes it, which runs nothing and
// throws nothing; rsSetProp's otherwise.
func rsSetPropLength(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if o, v := regs[w.A()], regs[w.B()]; o.IsObject() && v.IsNumber() {
		if obj := o.object(); obj.class == ClassArray && st.cl.names[st.c.siteName[w.C()]] == atomLength &&
			obj.flags&(objArrayLengthWritable|objHasSparseElements) == objArrayLengthWritable && obj.noIndexKeys() {
			if n := uint32(v.num); float64(n) == v.num {
				obj.setArrayLength(n)
				return pc + 1
			}
		}
	}
	return rsSetProp(regs, w, pc, st)
}

// rhSetIndexTyped is set_index's fast path, as native code's: an element of
// dense storage written over, as rhSetIndex writes it, or a number stored
// into a typed array's element (regTypedAt, regTypedStore), rather than
// through set_index's slow path (regSetMiss) for each store. Anything
// else is rsSetIndex's.
func rhSetIndexTyped(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	obj, key := regs[w.A()], regs[w.B()]
	if obj.IsObject() && key.IsNumber() {
		o := obj.Object()
		if i := uint32(key.Number()); float64(i) == key.Number() &&
			o.flags&(objHasExt|objMappedArguments|objSharedElems) == objHasExt {
			if es := o.extAt().elems; uint(i) < uint(len(es)) && !isHole(es[i]) {
				es[i] = regs[w.C()]
				return pc + 1
			}
		}
		if v := regs[w.C()]; o.class == ClassTypedArray && v.IsNumber() {
			if t, p := regTypedAt(st.r, o, key.Number()); t != nil && regTypedStore(t, p, v.num) {
				return pc + 1
			}
		}
	}
	return ^pc
}

// rhSetIndexITyped is set_index_i's fast path, as rhSetIndexTyped is
// set_index's: a dense element, or a typed array's number.
func rhSetIndexITyped(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if obj := regs[w.A()]; obj.IsObject() {
		o := obj.Object()
		if i := uint(w.B()); i < uint(len(o.elemsOf())) && o.flags&(objMappedArguments|objSharedElems) == 0 && !isHole(o.elemsOf()[i]) {
			o.extOf().elems[i] = regs[w.C()]
			return pc + 1
		}
		if v := regs[w.C()]; o.class == ClassTypedArray && v.IsNumber() {
			if t, p := regTypedAt(st.r, o, float64(w.B())); t != nil && regTypedStore(t, p, v.num) {
				return pc + 1
			}
		}
	}
	return ^pc
}
