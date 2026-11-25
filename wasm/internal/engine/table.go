package engine

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// MaxTableSize is the engine's limit on a table's size, in elements.
const MaxTableSize = 10_000_000

// Table is a table of references.
type Table struct {
	Elems []Ref
	// Type is the element type, Module the module whose types it names.
	Type   module.ValueType
	Module *module.Module
	// Max is the maximum size: the declared maximum, or the engine's limit
	// without one.
	Max    uint64
	HasMax bool
	Is64   bool

	// OnGrow, if set, is called after the table grows.
	OnGrow func()

	// charge is what the table holds of a budget, if it is charged to one.
	charge *charge
}

// refBytes is what an element of a table takes.
const refBytes = int64(unsafe.Sizeof(Ref{}))

// NewTableBudget makes a table as NewTable does, charged to b -- its
// elements, and its growth from then on -- charging b first, so that a
// table that does not fit is refused, returning nil and false, before its
// elements are allocated.
func NewTableBudget(t module.ValueType, m *module.Module, initial, max uint64, hasMax, is64 bool, init Ref, b *Budget) (*Table, bool) {
	c, ok := reserveCharge(b, int64(initial)*refBytes)
	if !ok {
		return nil, false
	}
	tab := NewTable(t, m, initial, max, hasMax, is64, init)
	tab.charge = c
	keepCharge(c, tab)
	return tab, true
}

// NewTable makes a table of initial elements, each init.
func NewTable(t module.ValueType, m *module.Module, initial, max uint64, hasMax, is64 bool, init Ref) *Table {
	if !hasMax || max > MaxTableSize {
		max = MaxTableSize
	}
	tab := &Table{Type: t, Module: m, Max: max, HasMax: hasMax, Is64: is64}
	tab.Elems = make([]Ref, initial)
	if !init.IsNull() {
		for i := range tab.Elems {
			tab.Elems[i] = init
		}
	}
	return tab
}

// Size returns the table's size.
func (t *Table) Size() uint64 { return uint64(len(t.Elems)) }

// Grow grows the table by delta elements, each init, returning its old size,
// or -1 if it would pass its maximum.
func (t *Table) Grow(delta uint64, init Ref) int64 {
	old := uint64(len(t.Elems))
	if delta > t.Max-old {
		return -1
	}
	if delta == 0 {
		return int64(old)
	}
	n := old + delta
	if !t.charge.take(int64(delta) * refBytes) {
		return -1
	}
	if n <= uint64(cap(t.Elems)) {
		t.Elems = t.Elems[:n]
	} else {
		grown := make([]Ref, n, max(n, min(2*old, t.Max)))
		copy(grown, t.Elems)
		t.Elems = grown
	}
	for i := old; i < n; i++ {
		t.Elems[i] = init
	}
	if t.OnGrow != nil {
		t.OnGrow()
	}
	return int64(old)
}
