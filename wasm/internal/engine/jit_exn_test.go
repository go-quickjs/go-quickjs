//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// exnWAT throws and catches in native code: in one function and across
// native frames, which the catch drops, references in them; from a host
// function; into a loop's label; past a try_table whose clauses do not
// match; through throw_ref; and out of native code to the caller.
const exnWAT = `(module
  (import "env" "boom" (func $boom (param i32)))
  (tag $e (export "e") (param i32))
  (tag $f (param f64 i32))
  (tag $r (param externref))
  (tag $g (param i32))
  (type $s (struct (field (mut i32))))
  (func $thrower (param $k i32) (result i32)
    (local $o (ref null $s))
    (local.set $o (struct.new $s (local.get $k)))
    (if (i32.eq (local.get $k) (i32.const 1)) (then (throw $e (i32.const 11))))
    (if (i32.eq (local.get $k) (i32.const 2)) (then (throw $f (f64.const 2.5) (i32.const 22))))
    (if (i32.eq (local.get $k) (i32.const 3)) (then (throw $r (ref.null extern))))
    (if (i32.eq (local.get $k) (i32.const 4)) (then (call $boom (i32.const 44))))
    (if (i32.eq (local.get $k) (i32.const 5)) (then (unreachable)))
    (struct.get $s 0 (local.get $o)))
  (func $deep (param $k i32) (param $n i32) (result i32)
    (local $o (ref null $s))
    (local.set $o (struct.new $s (local.get $n)))
    (if (result i32) (i32.eqz (local.get $n))
      (then (call $thrower (local.get $k)))
      (else (i32.add (struct.get $s 0 (local.get $o))
        (call $deep (local.get $k) (i32.sub (local.get $n) (i32.const 1)))))))
  (func (export "catch") (param $k i32) (param $n i32) (result i32)
    (local $o (ref null $s)) (local $t i32)
    (local.set $o (struct.new $s (i32.const 1000)))
    (block $all (result exnref)
      (block $cf (result f64 i32)
        (block $ce (result i32)
          (return (i32.add (struct.get $s 0 (local.get $o))
            (try_table (result i32) (catch $e $ce) (catch $f $cf) (catch_all_ref $all)
              (call $deep (local.get $k) (local.get $n))))))
        (return (i32.add (struct.get $s 0 (local.get $o)))))
      (local.set $t)
      (return (i32.add (i32.trunc_f64_s) (i32.add (local.get $t) (i32.const 2000)))))
    (drop)
    (i32.const -1))
  (func (export "nested") (param $k i32) (result i32)
    (block $outer (result i32)
      (try_table (result i32) (catch $e $outer)
        (block $inner (result i32)
          (try_table (result i32) (catch $g $inner)
            (call $thrower (local.get $k)))))))
  (func (export "loop") (param $n i32) (result i32)
    (local $i i32)
    (i32.const 0)
    (loop $l (param i32) (result i32)
      (drop)
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (try_table (result i32) (catch $e $l)
        (if (i32.lt_u (local.get $i) (local.get $n)) (then (throw $e (local.get $i))))
        (local.get $i))))
  (func (export "rethrow") (param $k i32) (result i32)
    (block $out (result i32)
      (try_table (result i32) (catch $e $out)
        (block $c (result i32 exnref)
          (try_table (result i32) (catch_ref $e $c)
            (call $thrower (local.get $k)))
          (return))
        (throw_ref))))
  (func (export "rethrownull") (result i32)
    (throw_ref (ref.null exn)))
  ;; The tail call's exceptions are not the try_table's to catch.
  (func $tail (param $k i32) (result i32)
    (block $c (result exnref)
      (try_table (result i32) (catch_all_ref $c)
        (return_call $thrower (local.get $k)))
      (return))
    (drop)
    (i32.const -5))
  (func (export "tail") (param $k i32) (result i32)
    (block $c (result i32)
      (try_table (result i32) (catch $e $c)
        (call $tail (local.get $k)))))
  (func (export "uncaught") (param $k i32) (result i32)
    (call $thrower (local.get $k))))`

// legacyWAT is exnWAT's thrower under the legacy try: catches by tag and
// all, a rethrow from a catch in a catch, and delegates past a try whose
// catch would take the exception, and out of the function.
const legacyWAT = `(module
  (import "env" "boom" (func $boom (param i32)))
  (tag $e (export "e") (param i32))
  (tag $f (param f64 i32))
  (tag $r (param externref))
  (tag $g (param i32))
  (type $s (struct (field (mut i32))))
  (func $thrower (param $k i32) (result i32)
    (local $o (ref null $s))
    (local.set $o (struct.new $s (local.get $k)))
    (if (i32.eq (local.get $k) (i32.const 1)) (then (throw $e (i32.const 11))))
    (if (i32.eq (local.get $k) (i32.const 2)) (then (throw $f (f64.const 2.5) (i32.const 22))))
    (if (i32.eq (local.get $k) (i32.const 3)) (then (throw $r (ref.null extern))))
    (if (i32.eq (local.get $k) (i32.const 4)) (then (call $boom (i32.const 44))))
    (if (i32.eq (local.get $k) (i32.const 5)) (then (unreachable)))
    (struct.get $s 0 (local.get $o)))
  ;; The legacy try: catches by tag and all, a rethrow from a catch in a
  ;; catch, and a delegate past a try whose catch would take it.
  (func (export "legacy") (param $k i32) (result i32)
    (try (result i32)
      (do (call $thrower (local.get $k)))
      (catch $e (i32.add (i32.const 100)))
      (catch $g (i32.add (i32.const 200)))
      (catch_all (i32.const 300))))
  (func (export "relegacy") (param $k i32) (result i32)
    (try (result i32)
      (do
        (try (result i32)
          (do (call $thrower (local.get $k)))
          (catch $e
            (drop)
            (try (result i32)
              (do (throw $g (i32.const 5)))
              (catch $g (drop) (rethrow 1))))))
      (catch $e (i32.add (i32.const 1000)))
      (catch_all (i32.const 2000))))
  (func (export "delegated") (param $k i32) (result i32)
    (try (result i32)
      (do
        (try (result i32)
          (do
            (try (result i32)
              (do (call $thrower (local.get $k)))
              (delegate 1)))
          (catch $e (i32.add (i32.const 10)))))
      (catch $e (i32.add (i32.const 20)))
      (catch_all (i32.const 30))))
  (func (export "delegateout") (param $k i32) (result i32)
    (try (result i32)
      (do
        (try (result i32)
          (do (call $thrower (local.get $k)))
          (delegate 1)))
      (catch $e (i32.add (i32.const 10)))))
  (func (export "outer") (param $k i32) (result i32)
    (try (result i32)
      (do (call $delegateout (local.get $k)))
      (catch $e (i32.add (i32.const 40)))
      (catch_all (i32.const 50))))
  (func $delegateout (param $k i32) (result i32)
    (try (result i32)
      (do
        (try (result i32)
          (do (call $thrower (local.get $k)))
          (delegate 1)))
      (catch $e (i32.add (i32.const 10))))))`

// exnInstance instantiates src, exnWAT or legacyWAT, its host function
// throwing the module's own tag $e, interpreted or natively compiled.
func exnInstance(t *testing.T, src string, native bool) *Instance {
	t.Helper()
	var inst *Instance
	boom := NewHostFunction(module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}}), "boom",
		func(_ context.Context, _ *Instance, args, _ []Val) error {
			return &Exception{hdr: header{kind: ObjExn}, Tag: inst.Tags[0], Vals: []Val{{N: args[0].N + 1}}}
		})
	imp := Extern{Kind: module.ExternFunc, Func: boom}
	if native {
		inst = nativeInstance(t, src, imp)
	} else {
		inst = thresholdInstance(t, src, 1<<30, imp)
	}
	return inst
}

func exnCases() []jitCase {
	var cases []jitCase
	for k := uint64(0); k < 6; k++ {
		for _, n := range []uint64{0, 1, 5} {
			cases = append(cases, jitCase{"catch", []Val{{N: k}, {N: n}}})
		}
		cases = append(cases, jitCase{"nested", []Val{{N: k}}}, jitCase{"rethrow", []Val{{N: k}}},
			jitCase{"tail", []Val{{N: k}}}, jitCase{"uncaught", []Val{{N: k}}})
	}
	return append(cases, jitCase{"loop", []Val{{N: 100}}}, jitCase{"rethrownull", nil})
}

func legacyCases() []jitCase {
	var cases []jitCase
	for k := uint64(0); k < 6; k++ {
		for _, name := range []string{"legacy", "relegacy", "delegated", "delegateout", "outer"} {
			cases = append(cases, jitCase{name, []Val{{N: k}}})
		}
	}
	return cases
}

// Native code catches what the interpreter catches, with the same values,
// and lets the rest go on out: with try_table and throw_ref, and with the
// legacy try, catch, rethrow and delegate.
func TestJITExceptions(t *testing.T) {
	for _, mod := range []struct {
		name, src string
		cases     func() []jitCase
		natives   []string
	}{
		{"exn", exnWAT, exnCases, []string{"catch", "nested", "loop", "rethrow", "tail", "uncaught"}},
		{"legacy", legacyWAT, legacyCases, []string{"legacy", "relegacy", "delegated", "delegateout", "outer"}},
	} {
		t.Run(mod.name, func(t *testing.T) {
			want, werr := runCases(t, exnInstance(t, mod.src, false), mod.cases())
			inst := exnInstance(t, mod.src, true)
			got, gerr := runCases(t, inst, mod.cases())
			cases := mod.cases()
			for i := range want {
				var xi, xj *Exception
				if errors.As(werr[i], &xi) != errors.As(gerr[i], &xj) {
					t.Fatalf("%s%v: %v, %v", cases[i].name, cases[i].args, werr[i], gerr[i])
				}
			}
			sameOutcomes(t, "native", cases, want, werr, got, gerr)
			for _, name := range mod.natives {
				if !nativeRan(inst, name) {
					t.Errorf("%s has no native code", name)
				}
			}
		})
	}
	// The catches leave the frames as the interpreter would: a call after
	// many catches runs at the depth it began at.
	inst := exnInstance(t, exnWAT, true)
	for k := 0; k < 3000; k++ {
		if res, err := refCall(t, inst, "catch", Val{N: 1}, Val{N: 5}); err != nil || res[0].N != 1011 {
			t.Fatalf("catch(1, 5) = %v, %v; want 1011", res, err)
		}
	}
}

// A loop in a try_table's body has no entry from the interpreter, whose
// recover around the body would outlive the native code's run of it; a
// loop out of one goes on natively, and the native code then catches what
// a try_table after it throws.
func TestOSRAndTryTable(t *testing.T) {
	src := `(module
  (tag $e (param i32))
  (func (export "inside") (param $n i32) (result i32) (local $i i32)
    (block $h (result i32)
      (try_table (result i32) (catch $e $h)
        (loop $l
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (br_if $l (i32.lt_s (local.get $i) (local.get $n))))
        (throw $e (local.get $i)))))
  (func (export "before") (param $n i32) (result i32) (local $i i32)
    (loop $l
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_s (local.get $i) (local.get $n))))
    (block $h (result i32)
      (try_table (result i32) (catch $e $h)
        (throw $e (i32.mul (local.get $i) (i32.const 2)))))))`
	if n := checkOSR(t, src, "inside", 100, 100); n != 0 {
		t.Fatalf("inside: %d calls went on natively from a loop, want 0", n)
	}
	if n := checkOSR(t, src, "before", 200, 100); n != 1 {
		t.Fatalf("before: %d calls went on natively from a loop, want 1", n)
	}
}
