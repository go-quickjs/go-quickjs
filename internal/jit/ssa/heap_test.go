package ssa

import (
	"math"
	"math/rand/v2"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// testHeap is what handles 0 to 3 name in the differential tests: an array
// of numbers, an array with holes and other tags among them, an object that
// is not an array, and an array whose length runs past its dense elements.
type testHeap []testArray

type testArray struct {
	cells  []uint64 // number words
	length uint64   // the array's length
	array  bool     // whether a view lets the array be read
}

const (
	testTagBase = 0xFFF8000000000000
	testHole    = testTagBase | 8
	testTrue    = testTagBase | 1<<8 | 3
)

func randomHeap(r *rand.Rand) testHeap {
	h := make(testHeap, 4)
	for i := range h {
		a := testArray{cells: make([]uint64, r.IntN(6)), array: i != 2}
		for j := range a.cells {
			switch k := r.IntN(6); {
			case i == 1 && k == 0:
				a.cells[j] = testHole
			case i == 1 && k == 1:
				a.cells[j] = testTrue
			default:
				a.cells[j] = math.Float64bits(float64(r.IntN(9) - 3))
			}
		}
		a.length = uint64(len(a.cells))
		if i == 3 {
			a.length += uint64(r.IntN(4))
		}
		h[i] = a
	}
	return h
}

// heapInstance is a heap's memory, which evaluators read and write: two
// words per element, as the VM's values are, the number and then a pointer
// (here always nil).
type heapInstance struct {
	views []ir.ArrayView
	cells [][]uint64
}

func (h testHeap) instance() heapInstance {
	in := heapInstance{views: make([]ir.ArrayView, ir.MaxSlots)}
	for i, a := range h {
		mem := make([]uint64, 2*len(a.cells))
		for j, c := range a.cells {
			mem[2*j] = c
		}
		in.cells = append(in.cells, mem)
		v := ir.ArrayView{DenseLength: uint64(len(a.cells)), Length: a.length}
		if len(mem) > 0 {
			v.Data = unsafe.Pointer(&mem[0])
		}
		if a.array {
			v.NumberLimit = testTagBase
		}
		in.views[i] = v
	}
	return in
}

func (in heapInstance) same(other heapInstance) bool {
	return slices.EqualFunc(in.cells, other.cells, slices.Equal)
}
