//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"encoding/binary"
	"math"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// BigInt64Arrays and BigUint64Arrays read natively: an element read that
// has met one (jitRegIndexSeen), and the loops of their forEach, reduce,
// filter, some, every, find and findIndex, read the element as a BigInt
// held in its value (ssa.OpBigElemRead), its type's word, which native
// code boxes where a use needs a value, as V8's code reads a
// BigInt64Array's element as a machine word. Each had been a call of Go
// at every element, or an exit, the loops Go's: a sum of a BigInt64Array
// by reduce took half as long again with the JIT as without.
//
// An element read is learned in the entry's typedReads, its PC marked
// jitBigRead -- and jitBigReadUnsigned for a BigUint64Array's -- which no
// read of numbers' PC is, so that TypedRead never takes it for one, and
// what keeps and carries typedReads keeps and carries it too.

const (
	jitBigRead         = 1 << 30
	jitBigReadUnsigned = 1 << 29
)

// jitBigTyped is o's BigInt type, if o is a BigInt64Array (jitBigRead) or
// a BigUint64Array (jitBigRead|jitBigReadUnsigned) -- 0 for any other.
func jitBigTyped(o Value) int32 {
	if !o.IsObject() || o.Object().class != ClassTypedArray {
		return 0
	}
	t, ok := o.Object().dataOf().(*typedArrayData)
	switch {
	case !ok:
	case t.kind == elemBigInt64:
		return jitBigRead
	case t.kind == elemBigUint64:
		return jitBigRead | jitBigReadUnsigned
	}
	return 0
}

// jitBigReadAt is the mark the element read at pc has in reads, an
// entry's typedReads (jitBigRead, with jitBigReadUnsigned), or 0.
func jitBigReadAt(reads []int32, pc int) int32 {
	for _, x := range reads {
		if x&jitBigRead != 0 && x&^(jitBigRead|jitBigReadUnsigned) == int32(pc) {
			return x &^ int32(pc)
		}
	}
	return 0
}

// jitStoreBigElem stores BigInt v in element i of o, a BigInt64Array or a
// BigUint64Array of a fixed length inside its buffer, as setElem stores
// it -- its low 64 bits -- for native code's store of any value
// (ssa.GoElemStore), which runs nothing; false for any other array or
// index, which Go stores after an exit. Each such store had left native
// code: a BigInt64Array filled in a loop took 65 times as long with the
// JIT as without.
func jitStoreBigElem(o *Object, i uint32, v Value) bool {
	t, ok := o.dataOf().(*typedArrayData)
	if !ok || t.kind != elemBigInt64 && t.kind != elemBigUint64 || t.tracking || uint(i) >= uint(t.fixedLength) {
		return false
	}
	st, _ := t.buffer.dataOf().(*arrayBufferData)
	if st == nil || st.immutable || t.byteOffset+t.fixedLength<<3 > len(st.bytes) {
		return false
	}
	binary.LittleEndian.PutUint64(st.bytes[t.byteOffset+int(i)<<3:], bigLow64(v))
	return true
}

// jitBigStoreSeen learns, at a store of a BigInt that native code had Go
// make (ssa.GoElemStore) into o, the element type of o where it is a
// BigInt64Array or a BigUint64Array: its bit set in cell, the site's (nil
// for none), and the code compiled again to store it natively
// (ssa.OpBigElemWrite) where the cell had no bit of it, as jitTypedSeen
// learns a number's. Native code's store of any value finds no case of
// those types: it stored each such element by a call of Go.
func (r *Runtime) jitBigStoreSeen(ctx *abi.Context, o *Object, cell *Value) {
	if cell == nil || jitBigTyped(Obj(o)) == 0 || !jitTypedCellSet(cell, o) {
		return
	}
	if e := r.jitRegCtxEntry(ctx); e != nil {
		r.jitRegReopt(nil, e)
	}
}

// jitTypedWriteSeen learns, at a host exit of the element write at
// register pc rpc of c, the element type of o where it is a typed array:
// its bit set in the site's cell (jitRegFeedback's TypedKind), and true
// where it had none, for the code to be compiled again with it. A write
// compiled for BigInt64Arrays alone, or BigUint64Arrays alone
// (ssa.OpBigElemWrite), reads no cell as it runs, as one of numbers does
// (ssa.GoTypedSeen): its store of another type's element leaves for Go,
// which learns the type here, rather than at every such store.
func (r *Runtime) jitTypedWriteSeen(c *regCode, rpc int, o Value) bool {
	if !o.IsObject() || o.Object().class != ClassTypedArray {
		return false
	}
	l := r.jit.lowered[weak.Make(c.fn)]
	if l == nil {
		return false
	}
	cell := l.typedCells[-1-int32(rpc)]
	return cell != nil && jitTypedCellSet(cell, o.Object())
}

// jitTypedCellSet sets the bit of o's element type in cell, a site's
// (jitRegFeedback's TypedKind), and reports whether it had none.
func jitTypedCellSet(cell *Value, o *Object) bool {
	t, ok := o.dataOf().(*typedArrayData)
	if !ok || t.kind > abi.ElemBigUint64 {
		return false
	}
	bits := math.Float64bits(cell.num)
	bit := uint64(1) << t.kind
	if bits&bit != 0 {
		return false
	}
	cell.num = math.Float64frombits(bits | bit)
	return true
}
