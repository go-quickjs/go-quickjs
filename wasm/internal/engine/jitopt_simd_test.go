//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// Random functions of the SIMD loads and stores of four memories -- memory
// 0, an unshared one, one of i64 addresses and a shared one -- at addresses
// near each memory's end and offsets near 2^31 and 2^32 too, with more
// v128s live than registers, compute in the optimizing tier what the
// interpreter computes and leave the memories as it does, traps included.
func TestOptSIMDMemoryRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 10))
	mems := []struct {
		decl, idx string
		size      uint64
	}{
		{"(memory $m0 1)", "i32", 65536},
		{"(memory $m1 2 4)", "i32", 131072},
		{"(memory $m2 i64 1 2)", "i64", 65536},
		{"(memory $m3 1 2 shared)", "i32", 65536},
	}
	loads := []string{"v128.load", "v128.load8x8_s", "v128.load8x8_u", "v128.load16x4_s", "v128.load16x4_u",
		"v128.load32x2_s", "v128.load32x2_u", "v128.load8_splat", "v128.load16_splat", "v128.load32_splat",
		"v128.load64_splat", "v128.load32_zero", "v128.load64_zero"}
	lanes := []struct {
		op    string
		lanes int
	}{{"8", 16}, {"16", 8}, {"32", 4}, {"64", 2}}
	for iter := range 40 {
		var b strings.Builder
		b.WriteString("(module\n")
		for _, m := range mems {
			b.WriteString("  " + m.decl + "\n")
		}
		// Sixteen v128 locals, live to the end, keep the registers busy.
		b.WriteString("  (func (export \"run\") (param $p i32) (param $s i64) (result i64)\n    (local $acc v128)")
		for k := range 16 {
			fmt.Fprintf(&b, " (local $v%d v128)", k)
		}
		b.WriteString("\n")
		for k := range 16 {
			fmt.Fprintf(&b, "    (local.set $v%d (i64x2.mul (i64x2.splat (local.get $s)) (v128.const i64x2 %d %d)))\n", k, 2*k+1, 3*k+7)
		}
		for range 20 {
			mi := rng.IntN(len(mems))
			m := mems[mi]
			var addr string
			if rng.IntN(3) == 0 {
				addr = fmt.Sprintf("(%s.const %d)", m.idx, m.size-uint64(rng.IntN(20)))
			} else {
				a := fmt.Sprintf("(i32.add (local.get $p) (i32.const %d))", rng.IntN(16))
				if m.idx == "i64" {
					a = fmt.Sprintf("(i64.extend_i32_u %s)", a)
				}
				addr = a
			}
			off := []uint64{0, 0, 1, 4, 16, 255, 0, 3, 8, 15, 0x7ffffff9, 0x80000000, 0xfffffff0}[rng.IntN(13)]
			v := fmt.Sprintf("(local.get $v%d)", rng.IntN(16))
			switch k := rng.IntN(5); k {
			case 0, 1:
				val := fmt.Sprintf("(%s $m%d offset=%d %s)", loads[rng.IntN(len(loads))], mi, off, addr)
				fmt.Fprintf(&b, "    (local.set $acc (i8x16.add (i32x4.shl (local.get $acc) (i32.const 3)) %s))\n", val)
			case 2:
				l := lanes[rng.IntN(len(lanes))]
				val := fmt.Sprintf("(v128.load%s_lane $m%d offset=%d %d %s %s)", l.op, mi, off, rng.IntN(l.lanes), addr, v)
				fmt.Fprintf(&b, "    (local.set $acc (v128.xor (local.get $acc) %s))\n", val)
			case 3:
				fmt.Fprintf(&b, "    (v128.store $m%d offset=%d %s (v128.xor (local.get $acc) %s))\n", mi, off, addr, v)
			default:
				l := lanes[rng.IntN(len(lanes))]
				fmt.Fprintf(&b, "    (v128.store%s_lane $m%d offset=%d %d %s (local.get $acc))\n", l.op, mi, off, rng.IntN(l.lanes), addr)
			}
		}
		b.WriteString("    (local.get $acc)")
		for k := range 16 {
			fmt.Fprintf(&b, " (local.get $v%d) (i16x8.add)", k)
		}
		b.WriteString(" (local.set $acc)\n")
		b.WriteString("    (i64.xor (i64x2.extract_lane 0 (local.get $acc)) (i64x2.extract_lane 1 (local.get $acc)))")
		for mi, m := range mems {
			// What each memory holds near its end.
			fmt.Fprintf(&b, " (i64.load $m%d (%s.const %d)) (i64.add)", mi, m.idx, m.size-8)
			fmt.Fprintf(&b, " (i64.load $m%d (%s.const %d)) (i64.add)", mi, m.idx, m.size-24)
		}
		b.WriteString("))\n")
		src := b.String()
		var calls []jitCall
		for _, p := range []uint64{0, 100, 65000, 65510, 65520, 65530, 131000, 131050} {
			calls = append(calls, jitCall{name: "run", args: []uint64{p, rng.Uint64()}})
		}
		t.Run(fmt.Sprint(iter), func(t *testing.T) { sameAsInterpreterTraps(t, src, calls) })
	}
}
