package bytecode

import "testing"

// TestEveryOpNamed pins that every instruction has its name, which the
// disassembly prints; without one it shows only as op(n).
func TestEveryOpNamed(t *testing.T) {
	for op := Op(0); op < opCount; op++ {
		if int(op) >= len(opNames) || opNames[op] == "" {
			t.Errorf("instruction %d has no name", op)
		}
	}
}
