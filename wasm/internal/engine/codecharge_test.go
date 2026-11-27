package engine

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
)

// manyCallsModule returns a module of a function g returning its argument,
// n functions each passing their argument through g calls times, and an
// export run passing its argument through each of them: register code some
// 25 times the module's bytes, compiled as run first calls each.
func manyCallsModule(n, calls int) []byte {
	sec := func(b []byte, id byte, body []byte) []byte {
		b = append(b, id)
		b = binary.AppendUvarint(b, uint64(len(body)))
		return append(b, body...)
	}
	fn := func(callees func(func(uint32))) []byte {
		body := []byte{0}
		callees(func(i uint32) {
			body = append(body, 0x20, 0, 0x10)
			body = binary.AppendUvarint(body, uint64(i))
			body = append(body, 0x21, 0)
		})
		body = append(body, 0x20, 0, 0x0b)
		return append(binary.AppendUvarint(nil, uint64(len(body))), body...)
	}
	b := []byte{0, 'a', 's', 'm', 1, 0, 0, 0}
	b = sec(b, 1, []byte{1, 0x60, 1, 0x7f, 1, 0x7f})
	funcs := binary.AppendUvarint(nil, uint64(n+2))
	for range n + 2 {
		funcs = append(funcs, 0)
	}
	b = sec(b, 3, funcs)
	exp := []byte{1, 3, 'r', 'u', 'n', 0}
	b = sec(b, 7, binary.AppendUvarint(exp, uint64(n+1)))
	code := binary.AppendUvarint(nil, uint64(n+2))
	code = append(code, fn(func(func(uint32)) {})...)
	for range n {
		code = append(code, fn(func(call func(uint32)) {
			for range calls {
				call(0)
			}
		})...)
	}
	code = append(code, fn(func(call func(uint32)) {
		for i := range n {
			call(uint32(i + 1))
		}
	})...)
	return sec(b, 10, code)
}

// chargeMeter is an outer meter that counts what it is charged.
type chargeMeter struct{ charged int64 }

func (m *chargeMeter) TryReserve(n int64) bool { m.charged += n; return true }
func (m *chargeMeter) Charge(n int64) error    { m.charged += n; return nil }
func (m *chargeMeter) Exceeded() error         { return ErrMemoryLimit }

// newHas returns a walk's has: whether a pointer was counted, marking it.
func newHas() func(unsafe.Pointer) bool {
	seen := map[unsafe.Pointer]bool{}
	return func(p unsafe.Pointer) bool {
		if seen[p] {
			return true
		}
		seen[p] = true
		return false
	}
}

// TestCodeChargedOnce checks that the register code a call compiles, and
// the records of the functions it runs, are charged to the outer meter of
// the budget of the instance whose call compiled them, as they are
// compiled, and that an instance's footprint counts its code once however
// many instances share it: two instances of one compiled module, each of a
// budget of its own, as two runtimes sharing a module have.
func TestCodeChargedOnce(t *testing.T) {
	bin := manyCallsModule(40, 200)
	m, derr := decoder.Decode(bin, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
		t.Fatal(derr)
	}
	// The interpreter's, whatever WASM_JIT says: native code compiles no
	// register code.
	code := testCode(t, NewCode(m))
	code.EnableJIT(math.MaxInt32)
	var meters [2]chargeMeter
	var insts [2]*Instance
	for i := range insts {
		var err error
		if insts[i], err = InstantiateBudget(context.Background(), code, nil, NewOuterBudget(&meters[i])); err != nil {
			t.Fatal(err)
		}
	}
	held0 := code.held.Load()
	for i, inst := range insts {
		ext, _ := inst.Export("run")
		before := meters[i].charged
		res, err := Invoke(context.Background(), ext.Func, []Val{{N: 7}})
		if err != nil || res[0].N != 7 {
			t.Fatalf("run on instance %d: %v, %v", i, res, err)
		}
		held := code.held.Load()
		if i == 0 {
			if got, want := meters[0].charged-before, held-held0; got != want || want <= int64(len(bin)) {
				t.Fatalf("the first instance's run charged %d bytes, compiled %d, of a module of %d", got, want, len(bin))
			}
			t.Logf("register code and records of a module of %d bytes: %d bytes", len(bin), held)
		} else if got := meters[1].charged - before; got != 0 || held != code.held.Load() {
			t.Fatalf("the second instance's run, which compiled nothing, charged %d bytes", got)
		}
	}
	codeBytes := code.MemoryFootprint(newHas())
	both := newHas()
	total := insts[0].MemoryFootprint(both) + insts[1].MemoryFootprint(both)
	without := newHas()
	without(unsafe.Pointer(code))
	if got := total - (insts[0].MemoryFootprint(without) + insts[1].MemoryFootprint(without)); got != codeBytes {
		t.Fatalf("two instances' footprints count %d bytes of code, want %d once", got, codeBytes)
	}
}
