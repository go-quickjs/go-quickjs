//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// The globals' values forwarded from block to block and carried by loops
// are what the interpreter reads: random functions of reads and writes of
// mutable globals of every numeric type, in branches and nested loops,
// among calls -- direct and through a table -- of a function that writes
// them, traps partway, and loops that only read them or never touch them,
// compute what the interpreter computes and leave the globals as it does,
// at a trap too, at the trap's offset.
func TestOptGlobalsRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	for iter := range 400 {
		src := globalsFunc(rng)
		nat := tierInstance(t, src, 0)
		in := interpretedInstance(t, src)
		for _, n := range []uint64{0, 1, 3, 7, 20} {
			s := rng.Uint64()
			rn, en := call(t, nat, "run", n, s)
			ri, ei := call(t, in, "run", n, s)
			if !sameTraps(rn, en, ri, ei) {
				t.Fatalf("iteration %d, run(%d, %#x): optimized %v %v, interpreted %v %v\n%s", iter, n, s, rn, en, ri, ei, src)
			}
			dn, _ := call(t, nat, "dump")
			di, _ := call(t, in, "dump")
			if !sameTraps(dn, nil, di, nil) {
				t.Fatalf("iteration %d, run(%d, %#x): globals %v, interpreted %v\n%s", iter, n, s, dn, di, src)
			}
		}
		if !optimized(nat, "run") {
			t.Fatalf("iteration %d: not optimized (%v)\n%s", iter, ReadJITStats().OptRefused, src)
		}
	}
}

// globalsFunc is a random module whose run reads and writes its globals.
func globalsFunc(rng *rand.Rand) string {
	types := []string{"i32", "i64", "f64", "i32", "f32", "i64"}
	var sb strings.Builder
	sb.WriteString("(module\n  (type $mt (func (param i64)))\n  (table 1 funcref)\n  (elem (i32.const 0) $mutate)\n")
	for i, ty := range types {
		fmt.Fprintf(&sb, "  (global $g%d (mut %s) (%s.const %d))\n", i, ty, ty, i+1)
	}
	sb.WriteString("  (func $mutate (param $x i64)\n")
	for i, ty := range types {
		if rng.IntN(2) == 0 {
			fmt.Fprintf(&sb, "    (global.set $g%d %s)\n", i, conv("i64", ty, "(i64.add (local.get $x) (i64.const 5))"))
		}
	}
	sb.WriteString("  )\n")
	sb.WriteString("  (func (export \"dump\") (result i64) (local $h i64)\n")
	for i, ty := range types {
		fmt.Fprintf(&sb, "    (local.set $h (i64.add (i64.mul (local.get $h) (i64.const 31)) %s))\n", conv(ty, "i64", fmt.Sprintf("(global.get $g%d)", i)))
	}
	sb.WriteString("    (local.get $h))\n")
	sb.WriteString("  (func (export \"run\") (param $n i32) (param $s i64) (result i64)\n    (local $a i64) (local $b i64) (local $i i32) (local $j i32) (local $k i32)\n")
	// Room in the frame for the spills.
	sb.WriteString("    (local i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64)\n")
	sb.WriteString("    (local.set $a (local.get $s))\n")
	g := func() (int, string) { i := rng.IntN(len(types)); return i, types[i] }
	var stmts func(depth, n int) string
	stmt := func(depth int) string {
		switch rng.IntN(13) {
		case 0, 1, 2:
			i, ty := g()
			return fmt.Sprintf("(local.set $a (i64.add (local.get $a) %s))", conv(ty, "i64", fmt.Sprintf("(global.get $g%d)", i)))
		case 3, 4, 5:
			i, ty := g()
			return fmt.Sprintf("(global.set $g%d %s)", i, conv("i64", ty, fmt.Sprintf("(i64.xor (local.get $a) (i64.mul (local.get $b) (i64.const %d)))", rng.IntN(100))))
		case 6:
			i, ty := g()
			return fmt.Sprintf("(global.set $g%d %s)", i, binop(ty, fmt.Sprintf("(global.get $g%d)", i), conv("i64", ty, "(local.get $b)")))
		case 7:
			if rng.IntN(2) == 0 {
				return "(call $mutate (local.get $a))"
			}
			return "(call_indirect (type $mt) (local.get $b) (i32.const 0))"
		case 8:
			return fmt.Sprintf("(local.set $b (i64.add (local.get $b) (i64.rotl (local.get $a) (i64.const %d))))", 1+rng.IntN(60))
		case 9:
			return fmt.Sprintf("(if (i64.lt_u (local.get $a) (local.get $b)) (then %s) (else %s))", stmts(depth, 2), stmts(depth, 2))
		case 10:
			// A trap partway, where the globals are as they were then.
			return fmt.Sprintf("(if (i32.eq (local.get $n) (i32.const %d)) (then (if (i64.eq (i64.and (local.get $a) (i64.const 3)) (i64.const 1)) (then unreachable))))", rng.IntN(25))
		case 11, 12:
			if depth >= 2 {
				return "(nop)"
			}
			iv := []string{"$i", "$j", "$k"}[depth]
			return fmt.Sprintf("(local.set %s (i32.const 0)) (block $x%d (loop $l%d %s (local.set %s (i32.add (local.get %s) (i32.const 1))) (br_if $x%d (i64.eq (i64.and (local.get $a) (i64.const 63)) (i64.const 9))) (br_if $l%d (i32.lt_u (local.get %s) (local.get $n)))))",
				iv, depth, depth, stmts(depth+1, 1+rng.IntN(5)), iv, iv, depth, depth, iv)
		}
		return "(nop)"
	}
	stmts = func(depth, n int) string {
		var parts []string
		for range n {
			parts = append(parts, stmt(depth))
		}
		return strings.Join(parts, " ")
	}
	for range 3 + rng.IntN(5) {
		sb.WriteString("    " + stmt(0) + "\n")
	}
	sb.WriteString("    (i64.xor (local.get $a) (local.get $b)))\n)\n")
	return sb.String()
}

// conv converts expression e of type from to type to, exactly where it can.
func conv(from, to, e string) string {
	if from == to {
		return e
	}
	switch from + ">" + to {
	case "i64>i32":
		return "(i32.wrap_i64 " + e + ")"
	case "i32>i64":
		return "(i64.extend_i32_u " + e + ")"
	case "i64>f64":
		return "(f64.convert_i64_s " + e + ")"
	case "i64>f32":
		return "(f32.convert_i64_s " + e + ")"
	case "f64>i64":
		return "(i64.reinterpret_f64 " + e + ")"
	case "f32>i64":
		return "(i64.extend_i32_u (i32.reinterpret_f32 " + e + "))"
	}
	panic(from + ">" + to)
}

// binop is an operation of type ty on x and y.
func binop(ty, x, y string) string {
	if ty == "f32" || ty == "f64" {
		return fmt.Sprintf("(%s.add (%s.mul %s (%s.const 0.5)) %s)", ty, ty, x, ty, y)
	}
	return fmt.Sprintf("(%s.add %s %s)", ty, x, y)
}
