package engine

import (
	"errors"
	"testing"
)

// The package's tests verify every body as it is compiled.
func init() { regVerify = true }

// TestRegVerifierRejects checks that the verifier passes the compiler's
// code and refuses each kind of broken code the handlers would trust.
func TestRegVerifierRejects(t *testing.T) {
	inst := instantiateMode(t, regKernels, true)
	for _, name := range []string{"loop", "switch", "fib", "globals", "mem", "sel", "pair"} {
		if _, err := call(t, inst, name, 1, 1, 1); err != nil {
			var tr *Trap
			if !errors.As(err, &tr) {
				t.Fatal(err)
			}
		}
	}
	m := inst.Code.Module
	rec := func(name string) *compiled {
		ext, _ := inst.Export(name)
		return inst.Code.fns[ext.Func.Index].Load()
	}
	// mutate returns a copy of name's code, changed by f.
	mutate := func(name string, f func(rc *regCode, p *compiled)) error {
		p := rec(name)
		rc := *p.reg
		rc.code = append([]uint64(nil), rc.code...)
		rc.offs = append([]uint32(nil), rc.offs...)
		rc.sites = append([]regSite(nil), rc.sites...)
		rc.tables = append([][]int32(nil), rc.tables...)
		f(&rc, p)
		return rc.verify(m, p.nslots, p.rslots)
	}
	for _, name := range []string{"loop", "switch", "fib", "globals", "mem", "sel", "pair"} {
		if err := mutate(name, func(*regCode, *compiled) {}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// find returns the pc of the first instruction of op in name's code.
	find := func(name string, op regOp) int {
		for pc, w := range rec(name).reg.code {
			if uint8(w) == op {
				return pc
			}
		}
		t.Fatalf("%s has no operation %d", name, op)
		return 0
	}
	setField := func(w uint64, i int, v uint16) uint64 {
		sh := 16 + 16*i
		return w&^(0xffff<<sh) | uint64(v)<<sh
	}
	for _, c := range []struct {
		what string
		name string
		f    func(rc *regCode, p *compiled)
	}{
		{"a destination past the frame", "loop", func(rc *regCode, p *compiled) {
			pc := find("loop", rI32Add)
			rc.code[pc] = setField(rc.code[pc], 0, uint16(p.nslots))
		}},
		{"an operand past the frame", "sel", func(rc *regCode, p *compiled) {
			pc := find("sel", rSelect)
			rc.code[pc] = setField(rc.code[pc], 2, uint16(p.nslots+3))
		}},
		{"a branch past the code", "loop", func(rc *regCode, p *compiled) {
			pc := find("loop", rJmp)
			rc.code[pc] = rc.code[pc]&0xffffffff | uint64(uint32(len(rc.code)))<<32
		}},
		{"a 16-bit branch past the code", "loop", func(rc *regCode, p *compiled) {
			pc := find("loop", rBrI32GeU)
			rc.code[pc] = setField(rc.code[pc], 2, 0x7000)
		}},
		{"a branch into an instruction's second word", "mem", func(rc *regCode, p *compiled) {
			pc := find("mem", rConst64)
			rc.code = append(rc.code[:len(rc.code)-1], uint64(rJmp)|uint64(uint32(int32(pc+1-(len(rc.code)-1))))<<32)
		}},
		{"a branch table entry past the code", "switch", func(rc *regCode, p *compiled) {
			rc.tables[0] = append([]int32(nil), rc.tables[0]...)
			rc.tables[0][1] = int32(len(rc.code) + 5)
		}},
		{"a branch table past the code's", "switch", func(rc *regCode, p *compiled) {
			pc := find("switch", rBrTable)
			rc.code[pc] = rc.code[pc]&0xffffffff | uint64(len(rc.tables))<<32
		}},
		{"a code cut short", "mem", func(rc *regCode, p *compiled) {
			pc := find("mem", rConst64)
			rc.code, rc.offs = rc.code[:pc+1], rc.offs[:pc+1]
		}},
		{"a last instruction that goes on", "loop", func(rc *regCode, p *compiled) {
			rc.code[len(rc.code)-1] = uint64(rMov)
		}},
		{"an operation with no handler", "loop", func(rc *regCode, p *compiled) {
			rc.code[0] = rc.code[0]&^0xff | 255
		}},
		{"an escape with no handler", "loop", func(rc *regCode, p *compiled) {
			rc.code[0] = uint64(rExt) | 250<<8
		}},
		{"a SIMD escape with no handler", "loop", func(rc *regCode, p *compiled) {
			rc.code[0] = uint64(rSIMD) | 250<<8
		}},
		{"a site past the code's", "fib", func(rc *regCode, p *compiled) {
			pc := find("fib", rCall)
			rc.code[pc] = rc.code[pc]&0xffffffff | uint64(len(rc.sites))<<32
		}},
		{"a site's argument past the frame", "fib", func(rc *regCode, p *compiled) {
			s := rc.sites[0]
			s.args = append([]regMove(nil), s.args...)
			s.args[0].from = uint32(p.nslots)
			rc.sites[0] = s
		}},
		{"a call of a function past the module's", "fib", func(rc *regCode, p *compiled) {
			rc.sites[0].fn = uint32(len(m.Functions))
		}},
		{"a global past the module's", "globals", func(rc *regCode, p *compiled) {
			pc := find("globals", rGlobalGet32)
			rc.code[pc] = rc.code[pc]&0xffffffff | uint64(len(m.Globals))<<32
		}},
		{"a global of another kind", "globals", func(rc *regCode, p *compiled) {
			pc := find("globals", rGlobalGet32)
			rc.code[pc] = uint64(rExt) | uint64(xGlobalGetRef)<<8 | rc.code[pc]&^0xffff
		}},
		{"a far prefix's slot past the frame", "loop", func(rc *regCode, p *compiled) {
			pc := find("loop", rI32Add)
			w := rc.code[pc]
			rc.code = append(rc.code[:pc:pc], append([]uint64{uint64(rFar) | 1<<16, uint64(p.nslots), 0, setField(w, 0, 0)}, rc.code[pc+1:]...)...)
			rc.offs = append(rc.offs[:pc:pc], append([]uint32{0, 0, 0}, rc.offs[pc:]...)...)
		}},
	} {
		if err := mutate(c.name, c.f); err == nil {
			t.Errorf("%s: the verifier passed it", c.what)
		} else {
			t.Logf("%s: %v", c.what, err)
		}
	}
}
