package engine

import (
	"sync"
	"sync/atomic"
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
// byte an element, a reference array a cell. A short array, and any array
// of references, is a single allocation, its elements after its header.

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

// typeRTT returns the runtime information of type ti of the code's
// module, rttOf's without its lock once asked for.
func (c *Code) typeRTT(ti uint32) *rtt {
	t := c.rtts.Load()
	if t == nil {
		n := make([]atomic.Pointer[rtt], len(c.Module.Types))
		if !c.rtts.CompareAndSwap(nil, &n) {
			t = c.rtts.Load()
		} else {
			t = &n
		}
	}
	e := &(*t)[ti]
	if r := e.Load(); r != nil {
		return r
	}
	r := rttOf(c.Module.Canonical[ti])
	e.Store(r)
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
	p := newCells(t.ncells)
	h := (*header)(p)
	h.rtt = t
	h.kind = ObjStruct
	return Ref{p: p}
}

// newCells allocates n zero cells: an array of a size known to the
// compiler for the structs of a few fields, which Go allocates without
// a slice's checks.
func newCells(n int) unsafe.Pointer {
	switch n {
	case 1:
		return unsafe.Pointer(new([1]Ref))
	case 2:
		return unsafe.Pointer(new([2]Ref))
	case 3:
		return unsafe.Pointer(new([3]Ref))
	case 4:
		return unsafe.Pointer(new([4]Ref))
	case 5:
		return unsafe.Pointer(new([5]Ref))
	case 6:
		return unsafe.Pointer(new([6]Ref))
	case 7:
		return unsafe.Pointer(new([7]Ref))
	case 8:
		return unsafe.Pointer(new([8]Ref))
	}
	return unsafe.Pointer(&make([]Ref, n)[0])
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

// Array is a GC array's header, laid out as two cells: its type and kind,
// then where its elements are and its length, a word. Its elements follow
// it in the same block, packed, but for a number array whose elements take
// more than arrayBlockBytes, whose elements are an allocation of their own:
// a reference array is a block of cells, the elements a cell each; a short
// number array a block of one of the allocator's sizes (arrayRooms), the
// header followed by bytes. Native code reads e and n at their offsets
// (Layout).
type Array struct {
	hdr header
	// e is the first element, nil where the block has room for none: the
	// second cell's pointer word. n is in its payload word, of which it
	// takes the high half where an int is 4 bytes.
	e unsafe.Pointer
	_ [8 - unsafe.Sizeof(int(0))]byte
	n int
}

// arrayHeader is the bytes of an array's header, where a block's elements
// begin.
const arrayHeader = int(unsafe.Sizeof(Array{}))

var (
	_ [unsafe.Sizeof(Array{}) - 2*unsafe.Sizeof(Ref{})]struct{}
	_ [2*unsafe.Sizeof(Ref{}) - unsafe.Sizeof(Array{})]struct{}
)

// arrayBlockBytes is the most bytes of elements a number array of a block
// of a class has: with its header, at most 512 bytes, which the allocator
// holds without a header of the block's type.
const arrayBlockBytes = 480

// arrayRooms are the bytes of elements the block of each class of short
// arrays has room for: with the header, the allocator's size classes up to
// 512 bytes, where a word is 8 bytes. An array whose elements take bytes is
// made in the first class with room for them (arrayClass).
var arrayRooms = [...]uint16{0, 16, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208,
	224, 256, 288, 320, 352, 384, 416, 448, arrayBlockBytes}

// arrayClassOf holds, for elements of 16*u bytes or fewer, more than
// 16*(u-1), the class of the block they are made in.
var arrayClassOf = func() (t [arrayBlockBytes/16 + 1]uint8) {
	for u := range t {
		for 16*u > int(arrayRooms[t[u]]) {
			t[u]++
		}
	}
	return t
}()

// arrayClass returns the class of the block of an array whose elements take
// bytes, at most arrayBlockBytes.
func arrayClass(bytes int) int { return int(arrayClassOf[(bytes+15)>>4]) }

// numBlock makes a block of a number array's: a header and room for
// elements of T's size.
func numBlock[T any]() unsafe.Pointer {
	return unsafe.Pointer(new(struct {
		a Array
		e T
	}))
}

// numBlocks makes the blocks of number arrays by class.
var numBlocks = [len(arrayRooms)]func() unsafe.Pointer{
	func() unsafe.Pointer { return unsafe.Pointer(new(Array)) },
	numBlock[[16]byte], numBlock[[32]byte], numBlock[[48]byte], numBlock[[64]byte],
	numBlock[[80]byte], numBlock[[96]byte], numBlock[[112]byte], numBlock[[128]byte],
	numBlock[[144]byte], numBlock[[160]byte], numBlock[[176]byte], numBlock[[192]byte],
	numBlock[[208]byte], numBlock[[224]byte], numBlock[[256]byte], numBlock[[288]byte],
	numBlock[[320]byte], numBlock[[352]byte], numBlock[[384]byte], numBlock[[416]byte],
	numBlock[[448]byte], numBlock[[arrayBlockBytes]byte],
}

// elemBytes returns the bytes an element of array type t takes: a
// reference's cell, or its number's.
func (t *rtt) elemBytes() int {
	if t.refElems {
		return int(unsafe.Sizeof(Ref{}))
	}
	return t.elemSize
}

// maxArrayLength returns the length an array of elements of size bytes may
// have at most: the size of an object, header included, must fit in 2^30-1
// bytes, the length rounded down to a multiple of 8.
func maxArrayLength(size int) int {
	const header = 24
	return ((1<<30 - 1) - header) / size &^ 7
}

// newArray allocates an array of type t and length n, its elements zero,
// charging it to b for a call running on x, for the instruction at offset
// at: 0 for a constant expression's.
func newArray(t *rtt, n uint32, b *Budget, x *exec, at uint32) *Array {
	size := t.elemSize
	if t.refElems {
		size = 8
	}
	if uint64(n) > uint64(maxArrayLength(size)) {
		trapAt(TrapArrayTooLarge, at)
	}
	b.chargeGC(x, arraySize(t, n))
	return makeArray(t, int(n))
}

// makeArray makes an array of type t and length n, its elements zero: a
// reference array a block of cells, a number array a block of a class or,
// longer, a header and its elements.
func makeArray(t *rtt, n int) *Array {
	if t.refElems {
		a := refBlock(t, n)
		a.n = n
		return a
	}
	bytes := n * t.elemSize
	if bytes <= arrayBlockBytes {
		a := arrayBlock(t, arrayClass(bytes))
		a.n = n
		return a
	}
	a := new(Array)
	a.hdr = header{rtt: t, kind: ObjArray}
	a.e = unsafe.Pointer(&make([]byte, bytes)[0])
	a.n = n
	return a
}

// arrayBlock makes an array of type t, its length 0, in a block of class
// k.
func arrayBlock(t *rtt, k int) *Array {
	if t.refElems {
		return refBlock(t, int(arrayRooms[k])/int(unsafe.Sizeof(Ref{})))
	}
	p := numBlocks[k]()
	a := (*Array)(p)
	a.hdr = header{rtt: t, kind: ObjArray}
	if k > 0 {
		a.e = unsafe.Add(p, arrayHeader)
	}
	return a
}

// refBlock makes a reference array of type t, its length 0, in a block of
// cells with room for n elements.
func refBlock(t *rtt, n int) *Array {
	p := newCells(2 + n)
	a := (*Array)(p)
	a.hdr = header{rtt: t, kind: ObjArray}
	if n > 0 {
		a.e = unsafe.Add(p, arrayHeader)
	}
	return a
}

// bytes returns the elements of number array a.
func (a *Array) bytes() []byte {
	return unsafe.Slice((*byte)(a.e), a.n*a.hdr.rtt.elemSize)
}

// refs returns the elements of reference array a.
func (a *Array) refs() []Ref { return unsafe.Slice((*Ref)(a.e), a.n) }

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
