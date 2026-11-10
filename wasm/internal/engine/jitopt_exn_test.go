//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// optExnInstance is exnInstance natively compiled, every function the
// optimizing tier accepts optimized at its first compile.
func optExnInstance(t *testing.T, src string) *Instance {
	t.Helper()
	inst := exnInstance(t, src, true)
	inst.Code.SetOptThreshold(0)
	inst.Code.SetOptBackground(false)
	return inst
}

// The optimizing tier's code catches what the interpreter catches, with the
// same values, and lets the rest go on out -- with try_table and throw_ref,
// and with the legacy try, catch, rethrow and delegate -- idle and with the
// collector marking.
func TestOptExceptions(t *testing.T) {
	for _, mod := range []struct {
		name, src string
		cases     func() []jitCase
		opt       []string
	}{
		{"exn", exnWAT, exnCases, []string{"catch", "nested", "loop", "rethrow", "tail", "uncaught"}},
		{"legacy", legacyWAT, legacyCases, []string{"legacy", "relegacy", "delegated", "delegateout", "outer"}},
	} {
		want, werr := runCases(t, exnInstance(t, mod.src, false), mod.cases())
		for _, mode := range []string{"idle", "marking", "clobbered"} {
			t.Run(mod.name+"/"+mode, func(t *testing.T) {
				if mode != "idle" {
					jitMarkingForTest(t, mode == "clobbered")
				}
				inst := optExnInstance(t, mod.src)
				got, gerr := runCases(t, inst, mod.cases())
				cases := mod.cases()
				for i := range want {
					var xi, xj *Exception
					if errors.As(werr[i], &xi) != errors.As(gerr[i], &xj) {
						t.Fatalf("%s%v: %v, %v", cases[i].name, cases[i].args, werr[i], gerr[i])
					}
				}
				sameOutcomes(t, "optimized", cases, want, werr, got, gerr)
				for _, name := range mod.opt {
					if !optimized(inst, name) {
						t.Errorf("%s is not optimized", name)
					}
				}
			})
		}
	}
	// The catches leave the frames as the interpreter would: a call after
	// many catches runs at the depth it began at.
	inst := optExnInstance(t, exnWAT)
	for k := 0; k < 3000; k++ {
		if res, err := refCall(t, inst, "catch", Val{N: 1}, Val{N: 5}); err != nil || res[0].N != 1011 {
			t.Fatalf("catch(1, 5) = %v, %v; want 1011", res, err)
		}
	}
}

// The values live across a call that throws -- more than registers, of
// two types, some of them changed on every turn of a loop -- are what a
// landing pad and the code after its label read, whatever registers the
// allocator gave them, for the clauses of a try_table and a legacy try
// within it, the exceptions' values among them.
func TestOptExceptionsLive(t *testing.T) {
	call1 := "(i64.extend_i32_u (call $thrower (i32.and (i32.add (local.get $k) (local.get $i)) (i32.const 3))))"
	call2 := "(i64.extend_i32_u (call $thrower (i32.and (i32.add (local.get $k) (i32.mul (local.get $i) (i32.const 3))) (i32.const 3))))"
	// The exception's values, on the stack, added to $acc.
	const takeE = `local.set $t3 local.set $t2 local.set $t1
        (local.set $acc (i64.add (local.get $acc) (i64.add (i64.add (local.get $t3) (i64.trunc_f64_s (local.get $t2))) (i64.extend_i32_u (local.get $t1)))))`
	const takeG = `i64.trunc_f32_s local.get $acc i64.add local.set $acc`
	turns := map[string]string{
		// A try_table within a try_table.
		"table": `(block $done
        (block $ce (result i32 f64 i64)
          (block $cg (result f32)
            (try_table (result i64) (catch $e $ce) (catch $g $cg)
              (i64.add
                (block $ig (result i64)
                  (drop (block $cg2 (result f32)
                    (br $ig (try_table (result i64) (catch $g $cg2) ` + call1 + `))))
                  (i64.const 1000))
                ` + call2 + `))
            local.get $acc i64.add local.set $acc
            br $done)
          ` + takeG + `
          br $done)
        ` + takeE + `)`,
		// A legacy try within a legacy try.
		"legacy": `(try
        (do
          (i64.add
            (try (result i64) (do ` + call1 + `) (catch $g (drop) (i64.const 1000)))
            ` + call2 + `)
          local.get $acc i64.add local.set $acc)
        (catch $e ` + takeE + `)
        (catch $g ` + takeG + `))`,
	}
	for name, turn := range turns {
		var sb strings.Builder
		sb.WriteString(`(module
  (tag $e (param i32 f64 i64))
  (tag $g (param f32))
  (func $thrower (param $k i32) (result i32)
    (if (i32.eq (local.get $k) (i32.const 1)) (then (throw $e (i32.const 7) (f64.const 2.5) (i64.const -9))))
    (if (i32.eq (local.get $k) (i32.const 2)) (then (throw $g (f32.const 1.25))))
    (local.get $k))
  (func (export "f") (param $k i32) (param $x i64) (result i64)
    (local $i i32) (local $acc i64) (local $t1 i32) (local $t2 f64) (local $t3 i64)
`)
		const n = 18
		for j := 0; j < n; j++ {
			fmt.Fprintf(&sb, "    (local $a%d i64) (local $b%d f64)\n", j, j)
		}
		for j := 0; j < n; j++ {
			fmt.Fprintf(&sb, "    (local.set $a%d (i64.add (local.get $x) (i64.const %d)))\n", j, 5*j+1)
			fmt.Fprintf(&sb, "    (local.set $b%d (f64.mul (f64.convert_i64_s (local.get $x)) (f64.const %d.25)))\n", j, j)
		}
		sb.WriteString("    (loop $l " + turn + `
      (local.set $a0 (i64.add (local.get $a0) (local.get $acc)))
      (local.set $b1 (f64.add (local.get $b1) (f64.const 0.5)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (i32.const 6))))
`)
		expr := "(local.get $acc)"
		for j := 0; j < n; j++ {
			expr = fmt.Sprintf("(i64.add %s (i64.add (local.get $a%d) (i64.trunc_f64_s (local.get $b%d))))", expr, j, j)
		}
		sb.WriteString("    " + expr + "))")
		src := sb.String()
		var calls []jitCall
		for k := uint64(0); k < 4; k++ {
			for _, x := range []uint64{0, 1, 1000, 0xffffffff} {
				calls = append(calls, jitCall{name: "f", args: []uint64{k, x}})
			}
		}
		for _, mode := range []string{"idle", "clobbered"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				if mode != "idle" {
					jitMarkingForTest(t, true)
				}
				sameAsInterpreterIn(t, tierInstance(t, src, 0), src, calls, optimized)
			})
		}
	}
}

// A frame of a function's baseline code that began before the optimizing
// tier replaced the code catches with the baseline code's handlers, and the
// optimized code's frames with its own: a recursion whose deeper calls run
// the optimized code passes an exception up through both, each frame
// catching it, adding what it holds, and throwing it again.
func TestOptExceptionsAcrossTiers(t *testing.T) {
	src := `(module
  (tag $e (param i32))
  (func $f (export "f") (param $n i32) (result i32)
    (local $a i32) (local $b i64) (local $c f64)
    (local.set $a (i32.mul (local.get $n) (i32.const 3)))
    (local.set $b (i64.extend_i32_u (i32.add (local.get $n) (i32.const 7))))
    (local.set $c (f64.convert_i32_u (i32.mul (local.get $n) (local.get $n))))
    (if (i32.eqz (local.get $n)) (then (throw $e (i32.const 1))))
    (block $h (result i32)
      (try_table (result i32) (catch $e $h)
        (call $f (i32.sub (local.get $n) (i32.const 1)))))
    (i32.add (local.get $a))
    (i32.add (i32.wrap_i64 (local.get $b)))
    (i32.add (i32.trunc_f64_u (local.get $c)))
    (throw $e))
  (func (export "run") (param $n i32) (result i32)
    (block $h (result i32)
      (try_table (result i32) (catch $e $h)
        (call $f (local.get $n))))))`
	var calls []jitCall
	for _, n := range []uint64{0, 3, 40, 40, 70} {
		calls = append(calls, jitCall{name: "run", args: []uint64{n}})
	}
	inst := tierInstance(t, src, 20)
	sameAsInterpreterIn(t, inst, src, calls, func(*Instance, string) bool { return true })
	if !optimized(inst, "f") {
		t.Fatal("f is not optimized")
	}
}

// v128s go whole through the optimizing tier's exceptions -- a tag's v128
// thrown and caught, by a try_table and by a legacy catch, and v128s live
// across the throwing call in registers and read after the catch -- and
// across the allocations Go makes and the barrier's stores, idle and with
// every store through the barrier routine.
func TestOptExceptionsV128(t *testing.T) {
	tables := `(module
  (tag $v (param v128 i32))
  (type $s (struct (field (mut i32)) (field (mut anyref))))
  (func $thrower (param $k i32) (param $x v128) (result i32)
    (if (i32.eq (local.get $k) (i32.const 1)) (then (throw $v (i32x4.add (local.get $x) (v128.const i32x4 1 2 3 4)) (i32.const 9))))
    (local.get $k))
  (func (export "f") (param $k i32) (param $a i64) (result i64)
    (local $x v128) (local $y v128) (local $o (ref null $s)) (local $n i32)
    (local.set $x (i64x2.splat (local.get $a)))
    (local.set $y (i32x4.splat (i32.wrap_i64 (local.get $a))))
    (local.set $o (struct.new $s (i32.const 5) (ref.i31 (i32.const 6))))
    (block $c (result v128 i32)
      (drop (try_table (result i32) (catch $v $c)
        (call $thrower (local.get $k) (local.get $y))))
      (return (i64.add (i64x2.extract_lane 1 (local.get $x)) (i64.extend_i32_u (struct.get $s 0 (local.get $o))))))
    (local.set $n)
    (local.set $y (i32x4.add (local.get $y)))
    (struct.set $s 1 (local.get $o) (struct.new $s (i32.const 7) (ref.null any)))
    (i64.add (i64.add (i64x2.extract_lane 0 (local.get $y)) (i64x2.extract_lane 1 (local.get $x)))
      (i64.extend_i32_u (local.get $n))))
)`
	legacy := `(module
  (tag $v (param v128 i32))
  (func $thrower (param $k i32) (param $x v128) (result i32)
    (if (i32.eq (local.get $k) (i32.const 1)) (then (throw $v (i32x4.add (local.get $x) (v128.const i32x4 1 2 3 4)) (i32.const 9))))
    (local.get $k))
  (func (export "f") (param $k i32) (param $a i64) (result i64)
    (local $x v128) (local $y v128) (local $n i32)
    (local.set $x (i64x2.splat (local.get $a)))
    (local.set $y (i32x4.splat (i32.wrap_i64 (local.get $a))))
    (try (result i64)
      (do (i64.extend_i32_u (call $thrower (local.get $k) (local.get $y))))
      (catch $v
        (local.set $n)
        (local.set $y (i32x4.add (local.get $y)))
        (i64.add (i64x2.extract_lane 0 (local.get $y)) (i64.extend_i32_u (local.get $n)))))
    (i64.add (i64x2.extract_lane 1 (local.get $x)))))`
	var calls []jitCall
	for k := uint64(0); k < 3; k++ {
		for _, a := range []uint64{0, 1, 0x123456789abcdef, 0xffffffffffffffff} {
			calls = append(calls, jitCall{name: "f", args: []uint64{k, a}})
		}
	}
	for name, src := range map[string]string{"table": tables, "legacy": legacy} {
		for _, mode := range []string{"idle", "clobbered"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				if mode != "idle" {
					jitMarkingForTest(t, true)
				}
				sameAsInterpreterIn(t, tierInstance(t, src, 0), src, calls, optimized)
			})
		}
	}
}
