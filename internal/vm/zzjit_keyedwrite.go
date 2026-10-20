//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// KeyedWrite is ssa.KeyedWriteFeedback's (jitKeyedWriteSeen).
func (fb *jitFeedback) KeyedWrite(pc int) bool {
	e := fb.ownEntry()
	return e != nil && slices.Contains(e.keyedWrites, int32(pc))
}

// jitKeyedWriteSeen notes a write of an element, set_index at pc, that left
// e's code with its receiver, key and value on the VM's stack below sp: one
// to an object native code does not store to that Go would store
// (jitKeyedWrite), which Go stores from native code from then on
// (ssa.GoKeyedWrite), its code compiled again on the existing tiers'
// budget, as an element read by a name is (jitIndexSeen). An array's or a
// typed array's element, which native code stores, is not.
func (r *Runtime) jitKeyedWriteSeen(e *jitEntry, pc, sp int) {
	if p := e.ssaProg; p == nil || pc >= len(p.Code) || p.Code[pc].Op != ir.ArrayWrite || slices.Contains(e.keyedWrites, int32(pc)) || sp < 3 || sp > len(r.stack) {
		return
	}
	o := r.stack[sp-3]
	if !o.IsObject() || o.Object().class == ClassArray || o.Object().class == ClassTypedArray || !r.jitKeyedWrite(o, r.stack[sp-2], r.stack[sp-1], false) {
		return
	}
	e.keyedWrites = append(e.keyedWrites, int32(pc))
	e.reopt = true
}

// jitKeyedWrite is o[k] = v where storing it runs nothing and throws
// nothing (ssa.GoKeyedWrite), made if store is set, or only asked: an
// array's or a typed array's element as native code's call of Go stores it
// (jitStoreElem); or an ordinary object's property by a key that is a
// number, a string or a symbol -- converted with nothing called -- as
// setIndexed stores it: its own element or writable data property, or a
// new property of an object that may be extended, where nothing up its
// chain -- ordinary objects and arrays, no proxy among them -- has the key
// as an accessor or a property not to be written, or an element or a
// synthesized length to say anything about it. It reports false for any
// other, which it leaves as it was, and Go stores after an exit.
func (r *Runtime) jitKeyedWrite(o, k, v Value, store bool) bool {
	if !o.IsObject() {
		return false
	}
	obj := o.Object()
	if obj.class == ClassArray || obj.class == ClassTypedArray {
		return store && k.IsNumber() && jitStoreElem(obj, k.num, v)
	}
	if obj.class != ClassObject || !k.IsNumber() && !k.IsString() && !k.IsSymbol() {
		return false
	}
	key, err := r.toPropertyKey(k)
	if err != nil {
		return false
	}
	const plain = propAccessor | propPrivate | propDeleted | propUninit | propWritable
	if key.IsIndex() {
		if i := key.Index(); uint(i) < uint(len(obj.elemsOf())) && !isHole(obj.elemsOf()[i]) {
			if store {
				obj.extOf().elems[i] = v
			}
			return true
		}
	}
	if i := obj.findOwn(key); i >= 0 {
		if obj.shape.keys[i].flags&plain != propWritable {
			return false
		}
		if store {
			obj.vals[i] = v
		}
		return true
	}
	if obj.flags&objExtensible == 0 {
		return false
	}
	for p := obj.proto; p != nil; p = p.proto {
		if p.class != ClassObject && p.class != ClassArray {
			return false
		}
		if key.IsIndex() {
			if len(p.elemsOf()) != 0 || !p.noIndexKeys() {
				return false
			}
			continue
		}
		if p.class == ClassArray && key == atomLength {
			return false
		}
		if i := p.findOwn(key); i >= 0 {
			if p.shape.keys[i].flags&plain != propWritable {
				return false
			}
			// An inherited writable data property, which the new one
			// shadows.
			break
		}
	}
	if !store {
		return true
	}
	// What createOwnProp adds to an ordinary object: an element of the
	// dense storage it has, or a property, its keyed store past the soft
	// limit making it a dictionary (shapeAdded), as setIndexed's does.
	if key.IsIndex() && len(obj.elemsOf()) > 0 && uint(key.Index()) <= uint(len(obj.elemsOf())) && obj.setElem(key.Index(), v) {
		return true
	}
	if t := r.shapes; t != nil && !key.IsIndex() {
		was := t.keyed
		t.keyed = obj
		obj.setOwnRaw(key, v, propDefault)
		t.keyed = was
		return true
	}
	obj.setOwnRaw(key, v, propDefault)
	return true
}
