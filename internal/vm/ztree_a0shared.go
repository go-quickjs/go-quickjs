package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Array literals of constants whose arrays share their elements.
//
// An array literal whose elements are all constants -- numbers, strings,
// true, false, null, undefined -- is compiled with a cache site (bytecode's
// OpNewArray, B). The tree tier makes each array of such a literal as an
// array of no storage of its own, over the constants that every array of
// the site shares, marked objSharedElems: V8's copy-on-write elements of a
// literal's boilerplate. An array a program builds and only reads -- a
// table, a payload, Splay's [0, 1, ..., 9] at each leaf of its trees -- is
// then a header of 64 bytes, rather than one with room for its elements,
// and nothing is copied into it.
//
// The first write gives the array a copy of its own (ownElems): whatever
// writes an element, or changes the storage in place, tests the flag first,
// as it tests objMappedArguments, whose indices must not be written in
// place either; reading tests nothing. A site whose arrays keep being
// written stops sharing (sharedGiveUp): a literal that is a scratch buffer
// or an accumulator is made as it was before, in one allocation, rather
// than made and then copied.
//
// The shared storage is a site's, per runtime: the constants are the
// runtime's values, held in the site's cache entry (propCache.p1, an array
// of the storage that is never handed out), so nothing is shared between
// runtimes and its count needs no atomics. The storage has a word before
// its elements, at index -1, which counts the copies made of it, and whose
// pointer is sharedMark: an array marked shared always has the storage's
// elements as they were made, never a slice of them, so the word is always
// there.
//
// The interpreter makes these literals as it did, every array its own: its
// case of OpNewArray is unchanged. Native code takes such a literal's
// arrays from a pool Go fills with them (sharedLiteralArray, ssa's
// LiteralSite.Shared), where the tree tier has made the site's storage
// before the code was compiled; it writes none of their elements. Every
// tier tests the flag where it writes.
//
// The code is closures assigned in this file's init, whose closures are laid
// out after every other closure in the package (see ztree_zkself.go), so
// that no hot code moves for it.

// sharedGiveUp is how many copies of a site's arrays the site makes before
// it stops sharing their elements.
const sharedGiveUp = 8

// sharedMark is what the pointer word of a shared storage's count is: an
// address of no other use, which ownElems checks.
var sharedMark byte

// sharedArrayNode builds new_array's node for a literal of constants (in.B
// a site): it takes the elements' entries, which it evaluates only to make
// the site's storage, and pushes the node. It reports false where it does
// not build one, which leaves new_array to build as any other.
var sharedArrayNode func(b *tbuilder, in bytecode.Instr) bool

// unshareElems gives o, an array objSharedElems marks, a copy of its
// elements of its own, counting the copy in the shared storage's count.
var unshareElems func(o *Object)

// sharedLiteralSite reports whether the array literal at pc of cl's
// function shares its constants: a literal of constants (OpNewArray's B)
// that has not given sharing up, its storage made here from the constants
// the code pushes before it where the tree tier has not made it -- native
// code may be compiled for a literal only the interpreter has run. Native
// code is compiled to take such a literal's arrays as Go makes them
// (sharedLiteralArray).
var sharedLiteralSite func(r *Runtime, cl *closure, pc int) bool

// sharedLiteralArray is an array of the literal at pc of cl's function, one
// sharedLiteralSite reported shares its constants: over its storage, or, a
// site that has given sharing up since, with a copy of its own -- whole
// either way, as native code takes it.
var sharedLiteralArray func(r *Runtime, cl *closure, pc int) *Object

// ownElems makes o's elements its own if they are shared: what writes an
// element in place, or changes the storage, calls it first.
func (o *Object) ownElems() {
	if o.flags&objSharedElems != 0 {
		unshareElems(o)
	}
}

// The helpers of the closures below, package variables rather than the
// init's locals: an init closure that captures one of its init's locals is
// laid out apart from the init closures, before them all, and would move
// every tree node's closure.

// sharedCount is a shared storage's count word, before its first element.
var sharedCount func(e []Value) *Value

// sharedMade is a site's storage of the constants vals, and the array that
// holds it for the site.
var sharedMade func(r *Runtime, vals []Value) *Object

// sharedShare is an array over t's storage, the site ic's, or one of its
// own where the site has given sharing up.
var sharedShare func(r *Runtime, ic *propCache, t *Object) *Object

// sharedSite is the cache entry of the array literal at pc of cl's
// function, if it is a literal of constants.
var sharedSite func(cl *closure, pc int) *propCache

// sharedConstants are the values the n instructions before pc push, each
// a constant's push, as the interpreter pushes them.
var sharedConstants func(cl *closure, pc, n int) ([]Value, bool)

func init() {
	sharedCount = func(e []Value) *Value {
		return (*Value)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(e)), -int(unsafe.Sizeof(Value{}))))
	}
	unshareElems = func(o *Object) {
		e := o.extAt().elems
		w := sharedCount(e)
		if w.ref != unsafe.Pointer(&sharedMark) {
			panic("vm: an array's shared elements are not the storage they were made")
		}
		w.num++
		own := make([]Value, len(e))
		copy(own, e)
		o.extAt().elems = own
		o.flags &^= objSharedElems
	}
	sharedMade = func(r *Runtime, vals []Value) *Object {
		store := make([]Value, len(vals)+1)
		store[0] = Value{ref: unsafe.Pointer(&sharedMark)}
		copy(store[1:], vals)
		t := newObject(r.proto.array, ClassArray)
		// Marked as its arrays are, so that the memory meter, which
		// reaches it through the function's caches, counts the storage
		// once for it and them. Nothing writes it.
		t.flags |= objSharedElems
		t.extAt().elems = store[1:len(store):len(store)]
		return t
	}
	sharedShare = func(r *Runtime, ic *propCache, t *Object) *Object {
		e := t.extAt().elems
		if ic.idx < 0 || sharedCount(e).num >= sharedGiveUp {
			ic.idx = -1
			return r.newArrayFrom(e)
		}
		o := newObject(r.proto.array, ClassArray)
		o.flags |= objSharedElems
		o.extAt().elems = e
		return o
	}
	sharedSite = func(cl *closure, pc int) *propCache {
		if pc < 0 || pc >= len(cl.fn.Code) {
			return nil
		}
		in := cl.fn.Code[pc]
		if in.Op != bytecode.OpNewArray || in.B == 0 || int(in.B) >= len(cl.ic) {
			return nil
		}
		return &cl.ic[in.B]
	}
	sharedConstants = func(cl *closure, pc, n int) ([]Value, bool) {
		if n == 0 || pc < n {
			return nil, false
		}
		vals := make([]Value, n)
		for i, in := range cl.fn.Code[pc-n : pc] {
			switch in.Op {
			case bytecode.OpPushInt:
				vals[i] = Int32(int32(in.A))
			case bytecode.OpPushConst:
				if int(in.A) >= len(cl.consts) {
					return nil, false
				}
				if v := cl.consts[in.A]; v.IsNumber() || v.IsString() {
					vals[i] = v
				} else {
					return nil, false
				}
			case bytecode.OpPushTrue:
				vals[i] = True
			case bytecode.OpPushFalse:
				vals[i] = False
			case bytecode.OpPushNull:
				vals[i] = Null
			case bytecode.OpPushUndef:
				vals[i] = Undefined
			case bytecode.OpPushEmptyString:
				vals[i] = Str(emptyString)
			default:
				return nil, false
			}
		}
		return vals, true
	}
	sharedLiteralSite = func(r *Runtime, cl *closure, pc int) bool {
		ic := sharedSite(cl, pc)
		if ic == nil {
			return false
		}
		if ic.p1 == nil {
			vals, ok := sharedConstants(cl, pc, int(cl.fn.Code[pc].A))
			if !ok {
				return false
			}
			ic.p1 = sharedMade(r, vals)
		}
		return ic.idx >= 0 && sharedCount(ic.p1.extAt().elems).num < sharedGiveUp
	}
	sharedLiteralArray = func(r *Runtime, cl *closure, pc int) *Object {
		ic := sharedSite(cl, pc)
		return sharedShare(r, ic, ic.p1)
	}
	sharedArrayNode = func(b *tbuilder, in bytecode.Instr) bool {
		n, site := int(in.A), in.B
		if n == 0 || n > b.depth() {
			return false
		}
		vals := make([]tval, n)
		for i := n - 1; i >= 0; i-- {
			vals[i] = b.pop().tree()
		}
		b.push(func(c *tctx) Value {
			ic := &c.ic[site]
			t := ic.p1
			if t == nil {
				v := make([]Value, n)
				for i, f := range vals {
					v[i] = f(c)
				}
				t = sharedMade(c.r, v)
				ic.p1 = t
			}
			return Obj(sharedShare(c.r, ic, t))
		})
		return true
	}
}
