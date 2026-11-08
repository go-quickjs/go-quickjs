package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestOpFactsStores holds opFacts' stores to bytecode.WrittenLocals, the
// interpreter's list of the instructions that store to a local:
// recordFacts asks WrittenLocals of an instruction only where opFacts says
// it may store (factStores), and takes the store to be to A's slot alone
// where it says that (factStoresA).
func TestOpFactsStores(t *testing.T) {
	probes := []bytecode.Instr{{A: 1, B: 4}, {A: 7, B: 1 << 10}, {A: 64, B: 65 << 2}, {A: 0, B: 3}}
	for op := range 256 {
		stores, inA := false, true
		for _, in := range probes {
			in.Op = bytecode.Op(op)
			lo, hi := bytecode.WrittenLocals(in)
			if lo < hi {
				stores = true
			}
			inA = inA && lo == int(in.A) && hi == int(in.A)+1
		}
		k := opFacts[op]
		if got := k&(factStores|factStoresA) != 0; got != stores {
			t.Errorf("%v: opFacts says stores %v, WrittenLocals %v", bytecode.Op(op), got, stores)
		}
		if k&factStores != 0 && k&factStoresA != 0 {
			t.Errorf("%v: both factStores and factStoresA", bytecode.Op(op))
		}
		if k&factStoresA != 0 && !inA {
			t.Errorf("%v: factStoresA, but WrittenLocals stores elsewhere than A's slot", bytecode.Op(op))
		}
	}
}
