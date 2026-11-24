//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// arrayFillWAT makes array.new of every storage type, of lengths given at
// run time and of constants past what native code stores one by one, each
// summed back element by element: from the pools' classes, past them, and
// too long, a trap.
var arrayFillWAT = func() string {
	var b strings.Builder
	b.WriteString(`(module
  (type $box (struct (field i32)))
  (global $keep (mut anyref) (ref.null any))
`)
	for _, t := range []struct{ name, storage, val, get, ext string }{
		{"i8", "i8", "i32", "array.get_s", "(i64.extend_i32_s %s)"},
		{"u8", "i8", "i32", "array.get_u", "(i64.extend_i32_u %s)"},
		{"i16", "i16", "i32", "array.get_s", "(i64.extend_i32_s %s)"},
		{"i32", "i32", "i32", "array.get", "(i64.extend_i32_s %s)"},
		{"i64", "i64", "i64", "array.get", "%s"},
		{"f32", "f32", "f32", "array.get", "(i64.extend_i32_u (i32.reinterpret_f32 %s))"},
		{"f64", "f64", "f64", "array.get", "(i64.reinterpret_f64 %s)"},
	} {
		get := fmt.Sprintf(t.ext, fmt.Sprintf("(%s $%s (local.get $a) (local.get $i))", t.get, t.name))
		fmt.Fprintf(&b, "  (type $%s (array (mut %s)))\n", t.name, t.storage)
		for _, f := range []struct{ suffix, params, length string }{
			{"", "(param $n i32) ", "(local.get $n)"}, {"_17", "", "(i32.const 17)"}, {"_100", "", "(i32.const 100)"},
		} {
			fmt.Fprintf(&b, `  (func (export "%[1]s%[5]s") %[6]s(param $v %[3]s) (result i64)
    (local $a (ref null $%[1]s)) (local $i i32) (local $s i64)
    (local.set $a (array.new $%[1]s (local.get $v) %[7]s))
    (global.set $keep (local.get $a))
    (local.set $s (i64.extend_i32_u (array.len (local.get $a))))
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (array.len (local.get $a))))
      (local.set $s (i64.add (i64.mul (local.get $s) (i64.const 31)) %[4]s))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (local.get $s))
`, t.name, t.storage, t.val, get, f.suffix, f.params, f.length)
		}
	}
	b.WriteString(`  (type $refs (array (mut anyref)))
  (func (export "refs") (param $n i32) (param $k i32) (result i64)
    (local $a (ref null $refs)) (local $i i32) (local $s i64)
    (local.set $a (array.new $refs
      (if (result anyref) (i32.and (local.get $k) (i32.const 1))
        (then (struct.new $box (local.get $k)))
        (else (ref.i31 (local.get $k))))
      (local.get $n)))
    (global.set $keep (local.get $a))
    (local.set $s (i64.extend_i32_u (array.len (local.get $a))))
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (array.len (local.get $a))))
      (local.set $s (i64.add (i64.mul (local.get $s) (i64.const 31))
        (i64.extend_i32_u
          (if (result i32) (ref.test (ref i31) (array.get $refs (local.get $a) (local.get $i)))
            (then (i31.get_u (ref.cast (ref i31) (array.get $refs (local.get $a) (local.get $i)))))
            (else (struct.get $box 0 (ref.cast (ref $box) (array.get $refs (local.get $a) (local.get $i)))))))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (local.get $s))
  (func (export "nulls") (param $n i32) (result i32)
    (ref.is_null (array.get $refs (array.new $refs (ref.null any) (local.get $n)) (i32.const 0)))))`)
	return b.String()
}()

// arrayFillCases are arrayFillWAT's calls: lengths 0, 1, 16, 17, each side
// of the largest class -- 480 bytes of elements -- and past it, and too
// long.
func arrayFillCases() []jitCase {
	var cs []jitCase
	sizes := map[string]uint64{"i8": 1, "u8": 1, "i16": 2, "i32": 4, "i64": 8, "f32": 4, "f64": 8}
	vals := map[string]uint64{"i8": 0xfffffff3, "u8": 0x1234567f, "i16": 0xffff8001, "i32": 0x89abcdef,
		"i64": 0x8123456789abcdef, "f32": 0x7fc00001, "f64": 0x7ff8000000000003}
	for _, name := range []string{"i8", "u8", "i16", "i32", "i64", "f32", "f64"} {
		edge := 480 / sizes[name]
		for _, n := range []uint64{0, 1, 2, 15, 16, 17, 31, edge - 1, edge, edge + 1, 1000, 5000, 0x7fffffff, 0xffffffff} {
			cs = append(cs, jitCase{name, []Val{{N: n}, {N: vals[name]}}})
		}
		cs = append(cs, jitCase{name + "_17", []Val{{N: vals[name]}}}, jitCase{name + "_100", []Val{{N: vals[name]}}})
	}
	for _, n := range []uint64{0, 1, 16, 17, 29, 30, 31, 1000, 0x7fffffff} {
		cs = append(cs, jitCase{"refs", []Val{{N: n}, {N: 6}}}, jitCase{"refs", []Val{{N: n}, {N: 7}}})
	}
	return append(cs, jitCase{"nulls", []Val{{N: 1}}}, jitCase{"nulls", []Val{{N: 100}}})
}

// TestJITArrayFill checks array.new of every storage type and of lengths
// known only as it runs, in the baseline's code and optimized, idle, with
// the collector marking and with every register clobbered across what the
// code leaves to Go: the results and traps are the interpreter's, and
// nothing but the traps leaves native code.
func TestJITArrayFill(t *testing.T) {
	want, werr := runCases(t, interpretedInstance(t, arrayFillWAT), arrayFillCases())
	for _, opt := range []bool{false, true} {
		tier := map[bool]string{false: "baseline", true: "opt"}[opt]
		for _, mode := range []string{"idle", "marking", "clobbered"} {
			t.Run(tier+"/"+mode, func(t *testing.T) {
				if mode != "idle" {
					jitMarkingForTest(t, mode == "clobbered")
				}
				inst := nativeInstance(t, arrayFillWAT)
				if opt {
					inst = optInstance(t, arrayFillWAT)
				}
				var got [][]Val
				var gerr []error
				var exits int64
				ops := countOps(t, func() {
					_, exits, _ = countAllocs(t, func() { got, gerr = runCases(t, inst, arrayFillCases()) })
				})
				sameOutcomes(t, tier, arrayFillCases(), want, werr, got, gerr)
				traps := 0
				for _, err := range gerr {
					var tr *Trap
					if errors.As(err, &tr) {
						traps++
					}
				}
				for _, name := range []string{"i8", "f64_17", "refs", "nulls"} {
					if opt && !optimized(inst, name) || !opt && !nativeRan(inst, name) {
						t.Errorf("%s is not %s code", name, tier)
					}
				}
				if int(exits) > traps || ops != 0 {
					t.Errorf("%d allocations left native code and %d instructions, %d traps", exits, ops, traps)
				}
			})
		}
	}
}

// TestJITArrayFillBudget checks fills that pass the instance's budget,
// arrays from the pools and past them kept in a list: they stop with
// ErrMemoryLimit natively as interpreted, and those that fit do not.
func TestJITArrayFillBudget(t *testing.T) {
	const src = `(module
  (type $a (array (mut i64)))
  (type $node (struct (field (ref null $node)) (field (ref null $a))))
  (global $keep (mut (ref null $node)) (ref.null $node))
  (func (export "hoard") (param $n i32) (param $len i32)
    (loop $l
      (global.set $keep (struct.new $node (global.get $keep) (array.new $a (i64.const 1) (local.get $len))))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1))))))
  (func (export "drop") (global.set $keep (ref.null $node))))`
	for _, tier := range []string{"interp", "baseline", "opt"} {
		for _, n := range []uint64{8, 30, 100, 5000} {
			inst := budgetTierInstance(t, src, NewBudget(256<<10), tier)
			if _, err := callExport(t, inst, "hoard", Val{N: 4}, Val{N: n}); err != nil {
				t.Fatalf("%s: a few arrays of %d: %v", tier, n, err)
			}
			if _, err := callExport(t, inst, "hoard", Val{N: 100_000}, Val{N: n}); !errors.Is(err, ErrMemoryLimit) {
				t.Fatalf("%s: arrays of %d: got %v, want ErrMemoryLimit", tier, n, err)
			}
		}
	}
}

// budgetTierInstance instantiates src under budget b, interpreted, in the
// baseline's code from the first call, or optimized from it.
func budgetTierInstance(t *testing.T, src string, b *Budget, tier string) *Instance {
	t.Helper()
	bin, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m, derr := decoder.Decode(bin, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
		t.Fatal(derr)
	}
	c := testCode(t, NewCode(m))
	switch tier {
	case "baseline":
		c.EnableJIT(0)
		c.SetOptThreshold(-1)
	case "opt":
		c.EnableJIT(0)
		c.SetOptThreshold(0)
		c.SetOptBackground(false)
	}
	inst, err := InstantiateBudget(context.Background(), c, nil, b)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}
