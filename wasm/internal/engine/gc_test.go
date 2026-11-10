package engine

import (
	"runtime"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// TestHeaderIsRefSized pins the layout a struct's first cell relies on.
func TestHeaderIsRefSized(t *testing.T) {
	if unsafe.Sizeof(header{}) != unsafe.Sizeof(Ref{}) ||
		unsafe.Offsetof(header{}.rtt) != unsafe.Offsetof(Ref{}.p) ||
		unsafe.Offsetof(header{}.kind) != unsafe.Offsetof(Ref{}.i) {
		t.Fatal("header and Ref differ in layout")
	}
}

// TestArrayIsTwoCells pins the layout a reference array's block of cells
// relies on: the header's cell, then where the elements are and the
// length, in a Ref's pointer and payload.
func TestArrayIsTwoCells(t *testing.T) {
	if unsafe.Sizeof(Array{}) != 2*unsafe.Sizeof(Ref{}) ||
		unsafe.Offsetof(Array{}.e) != unsafe.Sizeof(Ref{})+unsafe.Offsetof(Ref{}.p) ||
		unsafe.Offsetof(Array{}.n) < unsafe.Sizeof(Ref{})+unsafe.Offsetof(Ref{}.i) {
		t.Fatal("Array is not two cells")
	}
}

// Every class's block holds its header and as many elements as the class
// is for, the elements after the header within the block, and every
// element of every storage type is written and read back, the lengths
// past the blocks' included; the blocks' sizes are the allocator's.
func TestArrayBlocks(t *testing.T) {
	for k, room := range arrayRooms {
		if k > 0 && room <= arrayRooms[k-1] || room%16 != 0 {
			t.Fatalf("class %d: room for %d bytes after %d", k, room, arrayRooms[k-1])
		}
	}
	for bytes := 0; bytes <= arrayBlockBytes; bytes++ {
		k := arrayClass(bytes)
		if int(arrayRooms[k]) < bytes || k > 0 && int(arrayRooms[k-1]) >= bytes {
			t.Fatalf("%d bytes of elements: class %d, room for %d", bytes, k, arrayRooms[k])
		}
	}
	types := []module.ValueType{module.I8, module.I16, module.I32, module.I64,
		module.F32, module.F64, module.V128, module.Ref(module.HeapI31)}
	for _, et := range types {
		rt := &rtt{elemSize: et.Size(), refElems: et.IsRef()}
		for n := 0; n <= arrayBlockBytes/rt.elemBytes()+3; n++ {
			a := makeArray(rt, n)
			if a.n != n || a.hdr.rtt != rt || a.hdr.kind != ObjArray {
				t.Fatalf("%v[%d]: header %+v", et, n, *a)
			}
			if bytes := n * rt.elemBytes(); (bytes <= arrayBlockBytes || rt.refElems) && bytes > 0 &&
				uintptr(a.e) != uintptr(unsafe.Pointer(a))+uintptr(arrayHeader) {
				t.Fatalf("%v[%d]: the elements are not after the header", et, n)
			}
			for i := range n {
				arraySet(a, et, i, elemVal(et, i))
			}
			for i := range n {
				if got, want := arrayGet(a, et, i, false), elemVal(et, i); got != want {
					t.Fatalf("%v[%d]: element %d is %+v, want %+v", et, n, i, got, want)
				}
			}
		}
	}
}

// elemVal is a value of storage type t for element i, its storage's
// bytes only.
func elemVal(t module.ValueType, i int) Val {
	x := uint64(i)*0x9e3779b97f4a7c15 + 1
	switch t.Kind {
	case module.KindI8:
		return Val{N: x & 0xff}
	case module.KindI16:
		return Val{N: x & 0xffff}
	case module.KindI32, module.KindF32:
		return Val{N: x & 0xffff_ffff}
	case module.KindV128:
		return Val{N: x, Hi: ^x}
	case module.KindRef:
		return Val{R: I31(uint32(i))}
	}
	return Val{N: x}
}

// The collector sees a reference array's elements, in a block of a class
// and past them: structs only an array holds survive collections, and
// the memory freed meanwhile does not overwrite them.
func TestArrayElementsKept(t *testing.T) {
	st := &rtt{ncells: 2, cells: []int{1}}
	at := &rtt{elemSize: 8, refElems: true}
	var arrays []*Array
	for _, n := range []int{1, 2, 7, 29, 30, 31, 100} {
		a := makeArray(at, n)
		for i := range n {
			s := newStruct(st)
			cellAt(s.p, 1).i = uint64(n*1000 + i)
			*(*Ref)(unsafe.Add(a.e, uintptr(i)*unsafe.Sizeof(Ref{}))) = s
		}
		arrays = append(arrays, a)
	}
	for range 3 {
		runtime.GC()
		// Structs of the same size class, over what was freed.
		for range 10000 {
			cellAt(newStruct(st).p, 1).i = 0xa5a5
		}
	}
	for _, a := range arrays {
		for i, r := range a.refs() {
			if got := cellAt(r.p, 1).i; got != uint64(a.n*1000+i) {
				t.Fatalf("[%d] element %d holds %d", a.n, i, got)
			}
		}
	}
}
