//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

// BigInt64Arrays and BigUint64Arrays read natively: an element read that
// has met one (jitIndexSeen), and the loops of their forEach, reduce,
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

// TypedBigRead is ssa.BigElementFeedback's: the element read at pc has met
// a BigInt64Array, or a BigUint64Array (unsigned) (jitIndexSeen).
func (fb *jitFeedback) TypedBigRead(pc int) (unsigned, ok bool) {
	e := fb.ownEntry()
	if e == nil {
		return false, false
	}
	m := jitBigReadAt(e.typedReads, pc)
	return m&jitBigReadUnsigned != 0, m != 0
}
