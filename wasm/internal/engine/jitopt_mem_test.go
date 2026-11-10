//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// optSameAsInterpreter is sameAsInterpreter with every function the
// optimizing tier accepts optimized at its first compile, each function it
// calls having to be optimized.
func optSameAsInterpreter(t *testing.T, src string, calls []jitCall) {
	t.Helper()
	sameAsInterpreterIn(t, tierInstance(t, src, 0), src, calls, optimized)
}

// The optimizing tier's accesses of every kind of memory -- memories other
// than 0, of i64 addresses, shared ones -- at every offset, atomic ones
// too, check each memory's own size,
// read where it is at each access, and reach its own bytes, after grows by
// the function, by its callees and by Go, as the baseline's do.
func TestOptMemory(t *testing.T) {
	for name, run := range map[string]func(*testing.T, func(*testing.T, string, []jitCall)){
		"Memory64Bounds":  memory64Bounds,
		"LastByte":        memoryLastByte,
		"MultiMemory":     multiMemory,
		"MultiMemoryGrow": multiMemoryGrow,
		"Atomics":         atomicsMatchInterpreter,
		"AtomicTraps":     atomicTraps,
	} {
		t.Run(name, func(t *testing.T) { run(t, optSameAsInterpreter) })
	}
}

// Random functions of loads and stores of four memories -- memory 0, an
// unshared one, one of i64 addresses and a shared one -- at addresses near
// each memory's end and offsets near 2^31 and 2^32 (and 2^64 for i64
// addresses) too, atomic accesses among them, with more values live than
// registers, the values
// stored constants, locals and values spilled, compute what the
// interpreter computes and leave the memories as it does, traps included.
func TestOptMemoryRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	mems := []struct {
		decl, idx string
		size      uint64
	}{
		{"(memory $m0 1)", "i32", 65536},
		{"(memory $m1 2 4)", "i32", 131072},
		{"(memory $m2 i64 1 2)", "i64", 65536},
		{"(memory $m3 1 2 shared)", "i32", 65536},
	}
	loads := []struct {
		op, t string
		size  uint64
	}{
		{"i32.load", "i32", 4}, {"i64.load", "i64", 8}, {"f32.load", "f32", 4}, {"f64.load", "f64", 8},
		{"i32.load8_s", "i32", 1}, {"i32.load8_u", "i32", 1}, {"i32.load16_s", "i32", 2}, {"i64.load16_u", "i64", 2},
		{"i64.load32_s", "i64", 4}, {"i64.load32_u", "i64", 4},
	}
	stores := []struct {
		op, t string
		size  uint64
	}{
		{"i32.store", "i32", 4}, {"i64.store", "i64", 8}, {"f32.store", "f32", 4}, {"f64.store", "f64", 8},
		{"i32.store8", "i32", 1}, {"i32.store16", "i32", 2}, {"i64.store8", "i64", 1}, {"i64.store32", "i64", 4},
	}
	for iter := range 60 {
		var b strings.Builder
		b.WriteString("(module\n")
		for _, m := range mems {
			b.WriteString("  " + m.decl + "\n")
		}
		// Sixteen i64 locals, live to the end, keep the registers busy.
		b.WriteString("  (func (export \"run\") (param $p i32) (param $s i64) (result i64)\n    (local $acc i64)")
		for k := range 16 {
			fmt.Fprintf(&b, " (local $l%d i64)", k)
		}
		b.WriteString("\n")
		for k := range 16 {
			fmt.Fprintf(&b, "    (local.set $l%d (i64.mul (local.get $s) (i64.const %d)))\n", k, 2*k+1)
		}
		for range 24 {
			mi := rng.IntN(len(mems))
			m := mems[mi]
			// An address near the end: from the parameter, or a constant.
			var addr string
			if rng.IntN(3) == 0 {
				addr = fmt.Sprintf("(%s.const %d)", m.idx, m.size-uint64(rng.IntN(12)))
			} else {
				a := fmt.Sprintf("(i32.add (local.get $p) (i32.const %d))", rng.IntN(8))
				if m.idx == "i64" {
					a = fmt.Sprintf("(i64.extend_i32_u %s)", a)
				}
				addr = a
			}
			if rng.IntN(4) == 0 {
				// An atomic access, aligned but at the memory's end.
				a := "(i32.and (i32.add (local.get $p) (i32.const 8)) (i32.const -8))"
				if m.idx == "i64" {
					a = fmt.Sprintf("(i64.extend_i32_u %s)", a)
				}
				aoff := []int{0, 8, 16}[rng.IntN(3)]
				var val string
				switch k := rng.IntN(6); k {
				case 0:
					val = fmt.Sprintf("(i64.atomic.rmw.add $m%d offset=%d %s (local.get $l%d))", mi, aoff, a, rng.IntN(16))
				case 1:
					val = fmt.Sprintf("(i64.extend_i32_u (i32.atomic.rmw.cmpxchg $m%d offset=%d %s (i32.wrap_i64 (local.get $acc)) (i32.wrap_i64 (local.get $l%d))))", mi, aoff, a, rng.IntN(16))
				case 2:
					val = fmt.Sprintf("(i64.atomic.rmw8.xor_u $m%d offset=%d %s (local.get $acc))", mi, aoff+1, a)
				case 3:
					val = fmt.Sprintf("(i64.atomic.load32_u $m%d offset=%d %s)", mi, aoff+4, a)
				case 4:
					val = fmt.Sprintf("(i64.atomic.rmw16.or_u $m%d offset=%d %s (i64.const %d))", mi, aoff+2, a, rng.IntN(70000))
				default:
					fmt.Fprintf(&b, "    (i32.atomic.store16 $m%d offset=%d %s (i32.wrap_i64 (local.get $l%d)))\n", mi, aoff+6, a, rng.IntN(16))
					continue
				}
				fmt.Fprintf(&b, "    (local.set $acc (i64.add (i64.rotl (local.get $acc) (i64.const 7)) %s))\n", val)
				continue
			}
			off := []uint64{0, 0, 1, 4, 16, 255, 0, 1, 4, 16, 255, 0x7ffffff9, 0x80000000, 0xfffffff0}[rng.IntN(14)]
			if m.idx == "i64" && rng.IntN(20) == 0 {
				off = 0xfffffffffffffff8
			}
			if rng.IntN(2) == 0 {
				l := loads[rng.IntN(len(loads))]
				val := fmt.Sprintf("(%s $m%d offset=%d %s)", l.op, mi, off, addr)
				switch l.t {
				case "i32":
					val = fmt.Sprintf("(i64.extend_i32_u %s)", val)
				case "f32":
					val = fmt.Sprintf("(i64.extend_i32_u (i32.reinterpret_f32 %s))", val)
				case "f64":
					val = fmt.Sprintf("(i64.reinterpret_f64 %s)", val)
				}
				fmt.Fprintf(&b, "    (local.set $acc (i64.add (i64.rotl (local.get $acc) (i64.const 5)) %s))\n", val)
				continue
			}
			s := stores[rng.IntN(len(stores))]
			var val string
			switch rng.IntN(3) {
			case 0:
				val = fmt.Sprintf("(%s.const %d)", s.t, rng.IntN(1000)-500)
				if s.t == "f32" || s.t == "f64" {
					val = fmt.Sprintf("(%s.const %d.5)", s.t, rng.IntN(1000))
				}
			case 1:
				val = fmt.Sprintf("(local.get $l%d)", rng.IntN(16))
			default:
				val = "(local.get $acc)"
			}
			switch s.t {
			case "i32":
				if !strings.HasPrefix(val, "(i32.const") {
					val = fmt.Sprintf("(i32.wrap_i64 %s)", val)
				}
			case "f32":
				if !strings.HasPrefix(val, "(f32.const") {
					val = fmt.Sprintf("(f32.convert_i64_s %s)", val)
				}
			case "f64":
				if !strings.HasPrefix(val, "(f64.const") {
					val = fmt.Sprintf("(f64.reinterpret_i64 %s)", val)
				}
			}
			fmt.Fprintf(&b, "    (%s $m%d offset=%d %s %s)\n", s.op, mi, off, addr, val)
		}
		b.WriteString("    (local.get $acc)")
		for k := range 16 {
			fmt.Fprintf(&b, " (local.get $l%d) (i64.xor)", k)
		}
		b.WriteString("))\n")
		src := b.String()
		var calls []jitCall
		for _, p := range []uint64{0, 100, 65000, 65520, 65530, 131000, 131060} {
			calls = append(calls, jitCall{name: "run", args: []uint64{p, rng.Uint64()}})
		}
		t.Run(fmt.Sprint(iter), func(t *testing.T) { sameAsInterpreterTraps(t, src, calls) })
	}
}

// sameAsInterpreterTraps is optSameAsInterpreter where any call may trap,
// as the interpreter does.
func sameAsInterpreterTraps(t *testing.T, src string, calls []jitCall) {
	t.Helper()
	nat := tierInstance(t, src, 0)
	in := interpretedInstance(t, src)
	for _, cl := range calls {
		rn, en := call(t, nat, cl.name, cl.args...)
		ri, ei := call(t, in, cl.name, cl.args...)
		if !sameJITOutcome(rn, en, ri, ei) {
			t.Fatalf("%s%#x: optimized %v %v, interpreted %v %v\n%s", cl.name, cl.args, rn, en, ri, ei, src)
		}
		if !optimized(nat, cl.name) {
			t.Fatalf("%s not optimized", cl.name)
		}
		for i := range in.Memories {
			if string(nat.Memories[i].Bytes()) != string(in.Memories[i].Bytes()) {
				t.Fatalf("%s%#x: memory %d differs\n%s", cl.name, cl.args, i, src)
			}
		}
	}
}

// Goroutines running the optimizing tier's code on one shared memory update
// it atomically, as TestJITAtomicsConcurrent's run the baseline's.
func TestOptAtomicsConcurrent(t *testing.T) {
	atomicsConcurrent(t, func(t testing.TB, src string, imports ...Extern) *Instance {
		inst := thresholdInstance(t, src, 0, imports...)
		inst.Code.setOSRCalls(0)
		inst.Code.SetOptThreshold(0)
		inst.Code.SetOptBackground(false)
		return inst
	}, optimized)
}
