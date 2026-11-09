package engine

import (
	"sync"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// GC objects.
//
// Every object begins with a header of two words, laid out as a Ref is: its
// type's runtime information and its kind. A struct is a single allocation
// of Ref-sized cells, the first its header: a number field is held in a
// cell's payload, a reference in the whole cell, a v128 in two cells. An
// array is an Array header and its elements, packed: an i8 array takes a
// byte an element.

// rtt is a type's runtime information: its canonical index, the canonical
// indices of its declared supertypes by depth, ending with its own, and the
// layout of its fields or elements.
type rtt struct {
	canon  module.CanonicalIndex
	def    *module.CanonicalType
	supers []module.CanonicalIndex
	// cells holds the cell of each struct field; ncells is the number of
	// cells a struct of the type takes, header included.
	cells  []int
	ncells int
	// elemSize is an array's element size in bytes; refElems says its
	// elements are references.
	elemSize int
	refElems bool
}

var rtts struct {
	sync.RWMutex
	m map[module.CanonicalIndex]*rtt
}

// rttOf returns the runtime information of canonical type c.
func rttOf(c module.CanonicalIndex) *rtt {
	rtts.RLock()
	r := rtts.m[c]
	rtts.RUnlock()
	if r != nil {
		return r
	}
	def := module.Canonical(c)
	r = &rtt{canon: c, def: def}
	if def.Super != module.NoCanonical {
		sup := rttOf(def.Super)
		r.supers = append(append([]module.CanonicalIndex(nil), sup.supers...), c)
	} else {
		r.supers = []module.CanonicalIndex{c}
	}
	switch def.Kind {
	case module.TypeStruct:
		r.cells = make([]int, len(def.Struct.Fields))
		n := 1
		for i, f := range def.Struct.Fields {
			r.cells[i] = n
			if f.Type.Kind == module.KindV128 {
				n += 2
			} else {
				n++
			}
		}
		r.ncells = n
	case module.TypeArray:
		t := def.Array.Elem.Type
		r.refElems = t.IsRef()
		r.elemSize = t.Size()
	}
	rtts.Lock()
	if rtts.m == nil {
		rtts.m = make(map[module.CanonicalIndex]*rtt)
	}
	if prev := rtts.m[c]; prev != nil {
		r = prev
	} else {
		rtts.m[c] = r
	}
	rtts.Unlock()
	return r
}

// isSubRTT reports whether type r is the canonical type c or declares it,
// directly or not, as a supertype.
func (r *rtt) isSub(c module.CanonicalIndex, depth int) bool {
	return depth < len(r.supers) && r.supers[depth] == c
}

// objHeader returns the header of the object r points to.
func objHeader(p unsafe.Pointer) *header { return (*header)(p) }

// newStruct allocates a struct of type t, its fields zero.
func newStruct(t *rtt) Ref {
	cells := make([]Ref, t.ncells)
	h := (*header)(unsafe.Pointer(&cells[0]))
	h.rtt = t
	h.kind = ObjStruct
	return Ref{p: unsafe.Pointer(&cells[0])}
}

// structCells returns the cells of struct s, header included.
func structCells(p unsafe.Pointer) []Ref {
	h := (*header)(p)
	return unsafe.Slice((*Ref)(p), h.rtt.ncells)
}

// cell returns cell i of the struct p points to.
func cellAt(p unsafe.Pointer, i int) *Ref {
	return (*Ref)(unsafe.Add(p, uintptr(i)*unsafe.Sizeof(Ref{})))
}

// Array is a GC array.
type Array struct {
	hdr header
	n   int
	// b holds number elements, elemSize bytes each; r reference elements.
	b []byte
	r []Ref
}

// maxArrayLength returns the length an array of elements of size bytes may
// have at most: the size of an object, header included, must fit in 2^30-1
// bytes, the length rounded down to a multiple of 8.
func maxArrayLength(size int) int {
	const header = 24
	return ((1<<30 - 1) - header) / size &^ 7
}

// newArray allocates an array of type t and length n, its elements zero,
// charging it to b for a call running on x.
func newArray(t *rtt, n uint32, b *Budget, x *exec) *Array {
	size := t.elemSize
	if t.refElems {
		size = 8
	}
	if uint64(n) > uint64(maxArrayLength(size)) {
		trap(TrapArrayTooLarge)
	}
	b.chargeGC(x, arraySize(t, n))
	a := &Array{hdr: header{rtt: t, kind: ObjArray}, n: int(n)}
	if t.refElems {
		a.r = make([]Ref, n)
	} else {
		a.b = make([]byte, int(n)*t.elemSize)
	}
	return a
}

// ArrayRef returns the reference to a.
func ArrayRef(a *Array) Ref { return Ref{p: unsafe.Pointer(a)} }

// asArray returns the array r points to.
func asArray(r Ref) *Array { return (*Array)(r.p) }

// RefIsSub reports whether non-null reference r is of the type the heap type
// h of module m names. Abstract heap types are answered by what r points
// to; a type index by r's type's supertypes.
func RefIsSub(r Ref, h module.HeapType, m *module.Module) bool {
	if !h.IsIndex() {
		return refIsAbstract(r, h)
	}
	if r.IsI31() {
		return false
	}
	want := rttOf(m.Canonical[h])
	got := objHeader(r.p).rtt
	if got == nil {
		return false
	}
	return got.isSub(want.canon, len(want.supers)-1)
}

// refIsAbstract reports whether non-null reference r is of abstract heap
// type h. A reference is only ever tested against types of its own
// hierarchy, so a top type holds for any of them, a value converted between
// any and extern included.
func refIsAbstract(r Ref, h module.HeapType) bool {
	switch h {
	case module.HeapAny, module.HeapExtern, module.HeapFunc, module.HeapExn:
		return true
	}
	if r.IsI31() {
		return h == module.HeapEq || h == module.HeapI31
	}
	switch r.Kind() {
	case ObjStruct:
		return h == module.HeapEq || h == module.HeapStruct
	case ObjArray:
		return h == module.HeapEq || h == module.HeapArray
	}
	return false
}
