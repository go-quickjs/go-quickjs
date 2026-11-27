package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// chainSalt makes each chain chainModule writes a chain of types of its
// own, so that a test measures the types it makes rather than finding them
// made by another.
var chainSalt atomic.Uint32

// chainModule returns a module of n struct types, each but the first
// declaring the one before it as its supertype, each of ten fields, i32
// or i64 by the bits of salt.
func chainModule(n int, salt uint32) []byte {
	fields := []byte{10}
	for i := range 10 {
		fields = append(fields, 0x7f-byte(salt>>i&1), 0)
	}
	var sec []byte
	sec = binary.AppendUvarint(sec, uint64(n))
	sec = append(sec, 0x50, 0, 0x5f)
	sec = append(sec, fields...)
	for i := 1; i < n; i++ {
		sec = append(sec, 0x50, 1)
		sec = binary.AppendUvarint(sec, uint64(i-1))
		sec = append(sec, 0x5f)
		sec = append(sec, fields...)
	}
	b := []byte{0, 'a', 's', 'm', 1, 0, 0, 0, 1}
	b = binary.AppendUvarint(b, uint64(len(sec)))
	return append(b, sec...)
}

// rttChainBytes returns the bytes allocated making the runtime types of a
// chain of n struct types, from its deepest, and that type's.
func rttChainBytes(t *testing.T, n int) (uint64, *rtt) {
	t.Helper()
	m, err := decoder.Decode(chainModule(n, chainSalt.Add(1)), decoder.CoreLimits())
	if err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := rttOf(m.Canonical[n-1])
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, r
}

// TestRTTChainLinear checks that the runtime types of a chain of types
// take memory in proportion to its length, not to its length's square:
// each type's supertypes are not a copy of its chain.
func TestRTTChainLinear(t *testing.T) {
	sizes := []int{4_000, 16_000}
	if !testing.Short() {
		sizes = []int{100_000, 1_000_000}
	}
	var per []float64
	for _, n := range sizes {
		b, r := rttChainBytes(t, n)
		t.Logf("%d types: %d bytes, %.1f a type", n, b, float64(b)/float64(n))
		per = append(per, float64(b)/float64(n))
		if !r.isSub(r.canon, n-1) {
			t.Errorf("the deepest type of %d is not a subtype of itself", n)
		}
	}
	if per[1] > 1.5*per[0] || per[1] > 512 {
		t.Errorf("bytes a type grew from %.1f to %.1f", per[0], per[1])
	}
}

// supersType is a type of a tree supersTree makes: its supertype's index,
// -1 for none, and its fields past its supertype's.
type supersType struct {
	super  int
	fields []string
	extra  int
}

// supersTree returns a module of n struct types in a random tree whose
// chains are long -- most types extend the deepest before them -- some types
// declared again the same, and for each type j functions that test, cast
// and branch on a cast of an anyref to (ref $tj), and one making a struct
// of the type.
func supersTree(rng *rand.Rand, n int) (string, []int) {
	types := make([]supersType, n)
	children := make([]int, n)
	parent := make([]int, n)
	depth := make([]int, n)
	tip := 0
	var b strings.Builder
	b.WriteString("(module\n")
	for i := range types {
		ty := supersType{super: -1}
		switch {
		case i == 0:
		case i > 2 && rng.Intn(20) == 0:
			// The same type as one before it.
			ty = types[rng.Intn(i)]
		default:
			p := tip
			if rng.Intn(10) < 2 {
				p = rng.Intn(i)
			}
			ty.super = p
			ty.fields = append([]string(nil), types[p].fields...)
			for range children[p] {
				ty.fields = append(ty.fields, "(field i8)")
			}
			children[p]++
		}
		types[i] = ty
		parent[i] = ty.super
		if ty.super >= 0 {
			depth[i] = depth[ty.super] + 1
		}
		if depth[i] > depth[tip] {
			tip = i
		}
		if ty.super < 0 {
			fmt.Fprintf(&b, "  (type $t%d (sub (struct %s)))\n", i, strings.Join(ty.fields, " "))
		} else {
			fmt.Fprintf(&b, "  (type $t%d (sub $t%d (struct %s)))\n", i, ty.super, strings.Join(ty.fields, " "))
		}
	}
	for j := range n {
		fmt.Fprintf(&b, `  (func (export "new%[1]d") (result anyref) (struct.new_default $t%[1]d))
  (func (export "test%[1]d") (param anyref) (result i32) (ref.test (ref $t%[1]d) (local.get 0)))
  (func (export "testnull%[1]d") (param anyref) (result i32) (ref.test (ref null $t%[1]d) (local.get 0)))
  (func (export "cast%[1]d") (param anyref) (result i32) (drop (ref.cast (ref $t%[1]d) (local.get 0))) (i32.const 1))
  (func (export "br%[1]d") (param anyref) (result i32)
    (drop (block $y (result (ref $t%[1]d)) (drop (br_on_cast $y anyref (ref $t%[1]d) (local.get 0))) (return (i32.const 0))))
    (i32.const 1))
  (func (export "brfail%[1]d") (param anyref) (result i32)
    (drop (block $n (result anyref) (drop (br_on_cast_fail $n anyref (ref $t%[1]d) (local.get 0))) (return (i32.const 0))))
    (i32.const 1))
`, j)
	}
	b.WriteString(")")
	return b.String(), parent
}

// walkSub reports whether canonical type sub is super or has it among its
// supertypes, walking sub's chain a link at a time.
func walkSub(sub, super module.CanonicalIndex) bool {
	for c := sub; c != module.NoCanonical; c = module.Canonical(c).Super {
		if c == super {
			return true
		}
	}
	return false
}

// supersRunner is a way of running a supersTree module's functions.
type supersRunner struct {
	name string
	inst func(t *testing.T, src string) *Instance
	// run runs f, its calls made the runner's way.
	run func(inst *Instance, f func())
}

// checkSupers runs every test, cast and branch of a supersTree module of n
// types on objects of each of its types, null and an i31, and compares
// each answer with a walk of the canonical types' chains.
func checkSupers(t *testing.T, n int, seed int64, runners []supersRunner) {
	src, _ := supersTree(rand.New(rand.NewSource(seed)), n)
	bin, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m, derr := decoder.Decode(bin, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	depth := 0
	for i := range n {
		depth = max(depth, int(module.Canonical(m.Canonical[i]).Depth))
	}
	t.Logf("%d types, %d deep", n, depth)
	for _, rn := range runners {
		t.Run(rn.name, func(t *testing.T) {
			inst := rn.inst(t, src)
			fns := map[string]*Function{}
			for _, e := range inst.Module.Exports {
				fns[e.Name] = inst.Funcs[e.Index]
			}
			invoke := func(name string, arg Ref) (uint64, error) {
				fn := fns[name]
				if fn == nil {
					t.Fatalf("no export %q", name)
				}
				res, err := Invoke(context.Background(), fn, []Val{{R: arg}})
				if err != nil {
					return 0, err
				}
				return res[0].N, nil
			}
			rn.run(inst, func() {
				objs := make([]Ref, 0, n+2)
				canons := make([]module.CanonicalIndex, 0, n+2)
				for i := range n {
					res, err := Invoke(context.Background(), fns[fmt.Sprintf("new%d", i)], nil)
					if err != nil {
						t.Fatal(err)
					}
					objs = append(objs, res[0].R)
					canons = append(canons, m.Canonical[i])
				}
				objs = append(objs, Null, I31(5))
				for oi, o := range objs {
					for j := range n {
						want, wantNull := false, false
						switch {
						case oi < n:
							want = walkSub(canons[oi], m.Canonical[j])
							wantNull = want
							if got := RefIsCanonicalSub(o, m.Canonical[j]); got != want {
								t.Fatalf("RefIsCanonicalSub(t%d, t%d) = %v, want %v", oi, j, got, want)
							}
						case oi == n:
							wantNull = true
						}
						b2u := func(b bool) uint64 {
							if b {
								return 1
							}
							return 0
						}
						for _, c := range []struct {
							fn   string
							want uint64
						}{
							{"test", b2u(want)}, {"testnull", b2u(wantNull)}, {"br", b2u(want)}, {"brfail", b2u(!want)},
						} {
							got, err := invoke(fmt.Sprintf("%s%d", c.fn, j), o)
							if err != nil || got != c.want {
								t.Fatalf("%s%d of object %d: %d, %v; want %d", c.fn, j, oi, got, err, c.want)
							}
						}
						got, err := invoke(fmt.Sprintf("cast%d", j), o)
						var trap *Trap
						if want {
							if err != nil || got != 1 {
								t.Fatalf("cast%d of object %d: %d, %v", j, oi, got, err)
							}
						} else if !errors.As(err, &trap) || trap.Code != TrapIllegalCast {
							t.Fatalf("cast%d of object %d: %v, want an illegal cast", j, oi, err)
						}
					}
				}
			})
		})
	}
}

// interpRunners run the register code by the native handlers and by the
// Go table.
func interpRunners() []supersRunner {
	inst := func(t *testing.T, src string) *Instance {
		inst, err := instantiateWAT(t, src)
		if err != nil {
			t.Fatal(err)
		}
		return inst
	}
	return []supersRunner{
		{"native", inst, func(inst *Instance, f func()) { withNative(inst, true, f) }},
		{"go", inst, func(inst *Instance, f func()) { withNative(inst, false, f) }},
	}
}

// TestSupersAnswers holds the interpreter's tests and casts against types
// of every depth of trees of types whose chains run past the depths
// native code tests in a step to a walk of the chains.
func TestSupersAnswers(t *testing.T) {
	n := 260
	if testing.Short() {
		n = 140
	}
	for seed := int64(1); seed <= 2; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			checkSupers(t, n, seed, interpRunners())
		})
	}
}

// castDepths are the depths of the types BenchmarkCastDepth tests against.
var castDepths = []int{10, 40, 63, 100, 1000}

// castBenchWAT is a chain of 1,200 struct types, a function making an
// object of its type at depth 1,100, and for each of castDepths a function
// testing a reference against the chain's type at that depth n times.
func castBenchWAT() string {
	var b strings.Builder
	b.WriteString("(module\n  (type $t0 (sub (struct (field i64) (field i32) (field i64))))\n")
	for i := 1; i < 1200; i++ {
		fmt.Fprintf(&b, "  (type $t%d (sub $t%d (struct (field i64) (field i32) (field i64))))\n", i, i-1)
	}
	b.WriteString("  (func (export \"new\") (result anyref) (struct.new_default $t1100))\n")
	for _, d := range castDepths {
		fmt.Fprintf(&b, `  (func (export "run%d") (param $o anyref) (param $n i32) (result i32) (local $s i32)
    (loop $l
      (local.set $s (i32.add (local.get $s) (ref.test (ref $t%d) (local.get $o))))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
    (local.get $s))
`, d, d)
	}
	b.WriteString(")")
	return b.String()
}

// benchCasts runs castBenchWAT's tests on inst, each run f's way, a
// benchmark for each depth, a test an operation.
func benchCasts(b *testing.B, inst *Instance, run func(inst *Instance, f func())) {
	newExt, _ := inst.Export("new")
	res, err := Invoke(context.Background(), newExt.Func, nil)
	if err != nil {
		b.Fatal(err)
	}
	o := res[0].R
	for _, d := range castDepths {
		b.Run(fmt.Sprint(d), func(b *testing.B) {
			ext, _ := inst.Export(fmt.Sprintf("run%d", d))
			run(inst, func() {
				if _, err := Invoke(context.Background(), ext.Func, []Val{{R: o}, {N: 1000}}); err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				res, err := Invoke(context.Background(), ext.Func, []Val{{R: o}, {N: uint64(max(b.N, 1))}})
				if err != nil {
					b.Fatal(err)
				}
				if res[0].N != uint64(max(b.N, 1)) {
					b.Fatalf("%d of %d tests held", res[0].N, b.N)
				}
			})
		})
	}
}

// BenchmarkCastDepth times ref.test against types of a chain at depths
// from 10 to 1,000 in the interpreter, by the native handlers and by the
// Go table.
func BenchmarkCastDepth(b *testing.B) {
	src := castBenchWAT()
	for _, rn := range interpRunners() {
		b.Run(rn.name, func(b *testing.B) {
			bin, err := wat.Compile([]byte(src))
			if err != nil {
				b.Fatal(err)
			}
			m, derr := decoder.Decode(bin, decoder.CoreLimits())
			if derr != nil {
				b.Fatal(derr)
			}
			if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
				b.Fatal(derr)
			}
			inst, err := Instantiate(context.Background(), NewCode(m), nil)
			if err != nil {
				b.Fatal(err)
			}
			benchCasts(b, inst, rn.run)
		})
	}
}
