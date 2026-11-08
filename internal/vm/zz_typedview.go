package vm

import "unsafe"

// A typed array's view's copy of where its elements are and how many,
// which native code reads (typedArrayData's data and length). In a file of
// its own, sorting after the interpreter's, so that what is here moves
// none of their code.

// cacheView fills in what native code reads of t: where its elements are
// and how many, as its buffer has them now, and whether it may be written,
// holding as of epoch, the runtime's typedEpoch.
func (t *typedArrayData) cacheView(epoch uint64) {
	b := t.storage()
	n := t.countIn(b)
	t.data = nil
	if n > 0 {
		t.data = unsafe.Pointer(&b.bytes[t.byteOffset])
	}
	t.length = n
	t.readonly = b != nil && b.immutable
	t.noLength = b == nil || b.detached || t.byteOffset > len(b.bytes) ||
		!t.tracking && t.byteOffset+t.fixedLength*elemInfos[t.kind].size > len(b.bytes) ||
		t.tracking && b.block != nil
	t.epoch = epoch
}

// cacheView is t.cacheView as of r's typedEpoch.
func (r *Runtime) cacheView(t *typedArrayData) *typedArrayData {
	t.cacheView(r.typedEpoch)
	return t
}

// bufferChanged records that a buffer's bytes moved or changed length, or
// that it was detached: every view's copy of them (cacheView) is stale.
func (r *Runtime) bufferChanged() {
	r.typedEpoch++
}
