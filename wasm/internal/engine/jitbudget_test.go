//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// budgetCode returns the code of a module given in the text format, its
// functions compiled at their first call and charged to b, whatever
// WASM_JIT_MEMLIMIT says; or skips without native code.
func budgetCode(t testing.TB, src string, b *JITBudget) *Code {
	t.Helper()
	if !JITAvailable() {
		t.Skip("no native code in this build")
	}
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
	code := NewCode(m)
	if !code.EnableJIT(0) {
		t.Fatal("EnableJIT refused where JITAvailable")
	}
	cj := code.jit.Load()
	cj.mu.Lock()
	cj.joinBudget(b)
	cj.mu.Unlock()
	return code
}

func instantiateCode(t testing.TB, code *Code, imports ...Extern) *Instance {
	t.Helper()
	inst, err := Instantiate(context.Background(), code, imports)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// steppingClock has the JIT's clock advance an epoch at each reading, as
// WASM_JIT_CLOCK=step does, for the test.
func steppingClock(t *testing.T) { slowClock(t, 1) }

// slowClock has the JIT's clock advance an epoch every k readings.
func slowClock(t *testing.T, k uint64) {
	var n atomic.Uint64
	n.Store(1000 * k)
	jitEpochForTest = func() uint64 { return n.Add(1) / k }
	t.Cleanup(func() { jitEpochForTest = nil })
}

// checkAccounting checks that what the code says it holds is what its
// arena holds and what its records and tables are, and what its budget
// holds if the code is its only member.
func checkAccounting(t *testing.T, code *Code, b *JITBudget) JITMemory {
	t.Helper()
	cj := code.jit.Load()
	mem := code.JITMemory()
	_, live := cj.arena.Mapped()
	if mem.Code != int64(live) {
		t.Fatalf("code %d bytes, the arena holds %d", mem.Code, live)
	}
	cj.mu.Lock()
	seen := map[*placedCode]bool{}
	var code2, records int64
	for _, f := range cj.fns {
		if f == nil {
			continue
		}
		for p := f.native.Load(); p != nil; p = p.prev {
			if seen[p.place] {
				continue
			}
			seen[p.place] = true
			code2 += int64(p.end - p.base)
			records += recordBytes(p.loops, p.handlers)
		}
	}
	cj.mu.Unlock()
	cj.simd.Range(func(any, any) bool { records += simdExitBytes; return true })
	if code2 != mem.Code || records != mem.Records {
		t.Fatalf("the functions' codes are %d bytes and their records %d, JITMemory says %d and %d", code2, records, mem.Code, mem.Records)
	}
	if want := tableBytes(len(code.Module.Functions)); mem.Tables != want {
		t.Fatalf("tables %d bytes, want %d", mem.Tables, want)
	}
	if b != nil && b.Held() != mem.Held() {
		t.Fatalf("the budget holds %d, the code %d", b.Held(), mem.Held())
	}
	return mem
}

// What the JIT holds for a Code is its codes' bytes in the arena, exactly,
// their records, the SIMD instructions' code and the tables; its budget
// holds as much, and nothing once the Code is collected.
func TestJITMemoryAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		calls     []jitCall
		imports   []Extern
	}{
		{"calls and loops", `(module
  (func $fib (export "fib") (param i32) (result i32)
    (if (result i32) (i32.lt_u (local.get 0) (i32.const 2))
      (then (local.get 0))
      (else (i32.add (call $fib (i32.sub (local.get 0) (i32.const 1)))
                     (call $fib (i32.sub (local.get 0) (i32.const 2)))))))
  (func (export "sum") (param i32) (result i32) (local i32)
    (block $done (loop $l
      (br_if $done (i32.eqz (local.get 0)))
      (local.set 1 (i32.add (local.get 1) (local.get 0)))
      (local.set 0 (i32.sub (local.get 0) (i32.const 1)))
      (br $l)))
    (local.get 1)))`, []jitCall{{name: "fib", args: []uint64{15}}, {name: "sum", args: []uint64{1000}}}, nil},
		{"exceptions", exnWAT, []jitCall{{name: "catch", args: []uint64{1, 3}}, {name: "nested", args: []uint64{1}}}, []Extern{boomImport()}},
		{"simd", `(module
  (func (export "dot") (param i32) (result i32)
    (i32x4.extract_lane 0 (i32x4.add (i32x4.splat (local.get 0)) (v128.const i32x4 1 2 3 4)))))`,
			[]jitCall{{name: "dot", args: []uint64{5}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewJITBudget(0)
			code := budgetCode(t, tc.src, b)
			inst := instantiateCode(t, code, tc.imports...)
			if m := checkAccounting(t, code, b); m.Code != 0 || m.Functions != 0 {
				t.Fatalf("%+v before any call", m)
			}
			for _, cl := range tc.calls {
				call(t, inst, cl.name, cl.args...)
			}
			m := checkAccounting(t, code, b)
			if m.Code == 0 || m.Functions == 0 || m.Mapped < m.Code {
				t.Fatalf("%+v after the calls", m)
			}
			// The optimizing tier's code, beside the baseline's it
			// replaced.
			code.SetOptThreshold(1)
			for i := 0; i < 3; i++ {
				for _, cl := range tc.calls {
					call(t, inst, cl.name, cl.args...)
				}
			}
			checkAccounting(t, code, b)
			// Collected, the code gives back all it held.
			inst, code = nil, nil
			deadline := time.Now().Add(10 * time.Second)
			for b.Held() != 0 && time.Now().Before(deadline) {
				runtime.GC()
				time.Sleep(time.Millisecond)
			}
			if b.Held() != 0 {
				t.Fatalf("the budget holds %d once the code is collected", b.Held())
			}
		})
	}
}

// boomImport is exnWAT's import: it throws exnWAT's tag $e... as a host
// function cannot, so it returns an error.
func boomImport() Extern {
	host := NewHostFunction(module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}}), "boom",
		func(ctx context.Context, _ *Instance, args, results []Val) error { return errors.New("boom") })
	return Extern{Kind: module.ExternFunc, Func: host}
}

// manyWAT has n functions of some size each, f0 to f(n-1), f k returning
// x*x+k after a loop of x turns; rec recurses natively n deep and calls the
// host h at the bottom; ind calls f k through a table; and grow grows the
// memory, which exits to Go.
func manyWAT(n int) string {
	var b strings.Builder
	b.WriteString(`(module
  (import "env" "h" (func $h (param i32) (result i32)))
  (memory 1)
  (type $ft (func (param i32) (result i32)))
  (table $t ` + fmt.Sprint(n) + ` funcref)
`)
	for k := 0; k < n; k++ {
		fmt.Fprintf(&b, `  (func $f%d (export "f%d") (param $x i32) (result i32) (local $i i32) (local $s i32)
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (local.get $x)))
      (local.set $s (i32.add (local.get $s) (i32.mul (local.get $x) (i32.const %d))))
      (local.set $s (i32.xor (local.get $s) (i32.shl (local.get $i) (i32.const 3))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (i32.add (i32.mul (local.get $x) (local.get $x)) (i32.add (i32.const %d) (i32.and (local.get $s) (i32.const 0)))))
  (elem (table $t) (i32.const %d) func $f%d)
`, k, k, k+1, k, k, k)
	}
	b.WriteString(`  (func $rec (export "rec") (param $n i32) (result i32)
    (if (result i32) (i32.eqz (local.get $n))
      (then (call $h (i32.const 0)))
      (else (i32.add (i32.const 1) (call $rec (i32.sub (local.get $n) (i32.const 1)))))))
  (func (export "ind") (param $k i32) (param $x i32) (result i32)
    (i32.add (i32.const 1000) (call_indirect $t (type $ft) (local.get $x) (local.get $k))))
  (func (export "grow") (param $k i32) (result i32)
    (i32.add (call $h (local.get $k)) (memory.grow (i32.const 0))))
)`)
	return b.String()
}

// fk is what manyWAT's f k returns for x.
func fk(k, x uint64) uint64 { return uint32of(x*x + k) }

func uint32of(v uint64) uint64 { return uint64(uint32(v)) }

// unitSize is what one of manyWAT's functions holds compiled, with the
// tables of a module of n of them.
func unitSize(t *testing.T, n int) (unit, tables int64) {
	b := NewJITBudget(0)
	code := budgetCode(t, manyWAT(n), b)
	inst := instantiateCode(t, code, hostH(func(context.Context, uint64) uint64 { return 0 }))
	call(t, inst, "f0", 3)
	m := code.JITMemory()
	return m.Code + m.Records, m.Tables
}

func hostH(fn func(ctx context.Context, k uint64) uint64) Extern {
	host := NewHostFunction(module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}, Results: []module.ValueType{module.I32}}), "h",
		func(ctx context.Context, _ *Instance, args, results []Val) error {
			results[0] = Val{N: uint32of(fn(ctx, args[0].N))}
			return nil
		})
	return Extern{Kind: module.ExternFunc, Func: host}
}

// Past its budget, the JIT drops code no frame is in that was not called
// since the last pass, and its next call compiles it again; while all the
// code is in use, it refuses the compile, and the function runs in the
// interpreter. The results are the same throughout, and the code never
// holds more than the budget.
func TestJITDropsAndRefuses(t *testing.T) {
	const n = 24
	unit, tables := unitSize(t, n)
	b := NewJITBudget(tables + 4*unit + unit/2)
	code := budgetCode(t, manyWAT(n), b)
	inst := instantiateCode(t, code, hostH(func(context.Context, uint64) uint64 { return 0 }))

	// The clock stands still: the first four fit, the rest are refused,
	// none being old.
	jitEpochForTest = func() uint64 { return 10 }
	t.Cleanup(func() { jitEpochForTest = nil })
	before := ReadJITStats()
	for k := 0; k < n; k++ {
		res, err := call(t, inst, fmt.Sprintf("f%d", k), 7)
		if err != nil || res[0].N != fk(uint64(k), 7) {
			t.Fatalf("f%d(7) = %v, %v", k, res, err)
		}
	}
	after := ReadJITStats()
	if got := after.Compiled + after.Optimized - before.Compiled - before.Optimized; got != 4 {
		t.Fatalf("%d compiled within the budget, want 4", got)
	}
	if after.Refused[RefuseBudget]-before.Refused[RefuseBudget] == 0 {
		t.Fatal("nothing refused for want of room")
	}
	if held := b.Held(); held > b.Limit() {
		t.Fatalf("holds %d past the limit %d", held, b.Limit())
	}

	// Two epochs on, the four not called since are old: the next four
	// compile in their place, and the first four compile again after them.
	steppingClock(t)
	for round := 0; round < 3; round++ {
		for k := 0; k < n; k++ {
			res, err := call(t, inst, fmt.Sprintf("f%d", k), 7)
			if err != nil || res[0].N != fk(uint64(k), 7) {
				t.Fatalf("round %d: f%d(7) = %v, %v", round, k, res, err)
			}
			if held := b.Held(); held > b.Limit() {
				t.Fatalf("holds %d past the limit %d", held, b.Limit())
			}
		}
	}
	end := ReadJITStats()
	if end.Dropped-after.Dropped < n {
		t.Fatalf("%d functions' code dropped in three rounds of %d", end.Dropped-after.Dropped, n)
	}
	if code.JITMemory().Dropped == 0 {
		t.Fatal("the code counts no drop")
	}
	checkAccounting(t, code, b)
	// The arena reuses what dropped code left: it maps no more than a
	// chunk past what it holds.
	if m := code.JITMemory(); m.Mapped > m.Code+64<<10 {
		t.Fatalf("%d bytes mapped for %d of code", m.Mapped, m.Code)
	}
}

// Code a frame is in is never dropped: not a native return stack's frames
// below a call of the host, nor a frame that exited to Go for a call
// through a table, nor one a host call suspended on another goroutine,
// while the calls they make compile and drop other code of the same
// module.
func TestJITDropKeepsFrames(t *testing.T) {
	const n = 24
	unit, tables := unitSize(t, n)
	b := NewJITBudget(tables + 4*unit)
	steppingClock(t)
	var inst *Instance
	callAll := func(ctx context.Context) uint64 {
		var s uint64
		for k := 0; k < n; k++ {
			ext, _ := inst.Export(fmt.Sprintf("f%d", k))
			res, err := Invoke(ctx, ext.Func, []Val{{N: 5}})
			if err != nil {
				panic(err)
			}
			s += res[0].N
		}
		return s
	}
	var want uint64
	for k := 0; k < n; k++ {
		want += fk(uint64(k), 5)
	}
	code := budgetCode(t, manyWAT(n), b)
	inst = instantiateCode(t, code, hostH(func(ctx context.Context, k uint64) uint64 { return callAll(ctx) }))
	recIdx, _ := inst.Export("rec")
	before := ReadJITStats().Dropped
	for i := 0; i < 3; i++ {
		res, err := call(t, inst, "rec", 30)
		if err != nil || res[0].N != uint32of(30+want) {
			t.Fatalf("rec(30) = %v, %v; want %d", res, err, uint32of(30+want))
		}
	}
	if ReadJITStats().Dropped == before {
		t.Fatal("no code dropped below the host call")
	}
	if code.get(recIdx.Func.Index).native.Load() == nil {
		t.Fatal("rec, whose frames were below the host call, has no native code")
	}
	// A frame exited for a call through the table.
	for i := 0; i < 3; i++ {
		for k := 0; k < n; k++ {
			res, err := call(t, inst, "ind", uint64(k), 9)
			if err != nil || res[0].N != 1000+fk(uint64(k), 9) {
				t.Fatalf("ind(%d, 9) = %v, %v", k, res, err)
			}
		}
	}
	// A host call suspended on another goroutine while this one drops.
	entered, release := make(chan struct{}), make(chan struct{})
	b2 := NewJITBudget(tables + 4*unit)
	code2 := budgetCode(t, manyWAT(n), b2)
	inst2 := instantiateCode(t, code2, hostH(func(ctx context.Context, k uint64) uint64 {
		close(entered)
		<-release
		return 7
	}))
	inst3 := instantiateCode(t, code2, hostH(func(context.Context, uint64) uint64 { return 0 }))
	done := make(chan error)
	go func() {
		res, err := call(t, inst2, "rec", 20)
		if err == nil && res[0].N != 27 {
			err = fmt.Errorf("rec(20) = %d, want 27", res[0].N)
		}
		done <- err
	}()
	<-entered
	before = ReadJITStats().Dropped
	for i := 0; i < 3; i++ {
		for k := 0; k < n; k++ {
			res, err := call(t, inst3, fmt.Sprintf("f%d", k), 4)
			if err != nil || res[0].N != fk(uint64(k), 4) {
				t.Fatalf("f%d(4) = %v, %v", k, res, err)
			}
		}
	}
	if ReadJITStats().Dropped == before {
		t.Fatal("no code dropped while the other goroutine's call was suspended")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	checkAccounting(t, code2, b2)
}

// An exception a host function throws, after the calls it made dropped
// code of the module whose frames are below it, is caught by those frames,
// whose code was kept.
func TestJITDropKeepsCatchingFrames(t *testing.T) {
	const n = 16
	unit, tables := unitSize(t, n)
	interp := exnInstance(t, exnWAT, false)
	steppingClock(t)
	// Room for the exceptions' module and a few functions of the other.
	probe := NewJITBudget(0)
	probeCode := budgetCode(t, exnWAT, probe)
	probeInst := instantiateCode(t, probeCode, boomImport())
	for _, c := range exnCases() {
		refCall(t, probeInst, c.name, c.args...)
	}
	b := NewJITBudget(probe.Held() + tables + 3*unit)
	many := budgetCode(t, manyWAT(n), b)
	manyInst := instantiateCode(t, many, hostH(func(context.Context, uint64) uint64 { return 0 }))
	code := budgetCode(t, exnWAT, b)
	var inst *Instance
	boom := NewHostFunction(module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}}), "boom",
		func(ctx context.Context, _ *Instance, args, _ []Val) error {
			for k := 0; k < n; k++ {
				ext, _ := manyInst.Export(fmt.Sprintf("f%d", k))
				if _, err := Invoke(ctx, ext.Func, []Val{{N: 2}}); err != nil {
					return err
				}
			}
			return &Exception{hdr: header{kind: ObjExn}, Tag: inst.Tags[0], Vals: []Val{{N: args[0].N + 1}}}
		})
	inst = instantiateCode(t, code, Extern{Kind: module.ExternFunc, Func: boom})
	before := ReadJITStats().Dropped
	for round := 0; round < 3; round++ {
		for _, c := range exnCases() {
			got, gerr := refCall(t, inst, c.name, c.args...)
			want, werr := refCall(t, interp, c.name, c.args...)
			if !sameExnOutcome(got, gerr, want, werr) {
				t.Fatalf("%s%v = %v %v, want %v %v", c.name, c.args, got, gerr, want, werr)
			}
		}
	}
	if code.JITMemory().Dropped == 0 || ReadJITStats().Dropped == before {
		t.Fatal("the host's calls dropped none of the exceptions' module's code")
	}
	if held := b.Held(); held > b.Limit() {
		t.Fatalf("holds %d past the limit %d", held, b.Limit())
	}
}

// sameExnOutcome is sameJITOutcome, an exception the same as another of
// the same values.
func sameExnOutcome(rn []Val, en error, ri []Val, ei error) bool {
	var xn, xi *Exception
	if an, ai := errors.As(en, &xn), errors.As(ei, &xi); an || ai {
		if xn == nil || xi == nil || len(xn.Vals) != len(xi.Vals) {
			return false
		}
		for i := range xn.Vals {
			if xn.Vals[i].N != xi.Vals[i].N {
				return false
			}
		}
		return true
	}
	return sameJITOutcome(rn, en, ri, ei)
}

// Calls of one module's code on many goroutines, its budget room for a
// few functions, compile, drop and run its code at once, references and
// all while the collector marks; each computes what it should. Run with
// the race detector.
func TestJITDropConcurrent(t *testing.T) {
	const n = 24
	unit, tables := unitSize(t, n)
	b := NewJITBudget(tables + 5*unit)
	steppingClock(t)
	code := budgetCode(t, manyWAT(n), b)
	stop := make(chan struct{})
	var gcs sync.WaitGroup
	gcs.Add(1)
	go func() {
		defer gcs.Done()
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		inst := instantiateCode(t, code, hostH(func(ctx context.Context, k uint64) uint64 { return k * 3 }))
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < 40; i++ {
				k := uint64((g*7 + i*5) % n)
				name := fmt.Sprintf("f%d", k)
				ext, _ := inst.Export(name)
				res, err := Invoke(ctx, ext.Func, []Val{{N: uint64(i)}})
				if err != nil || res[0].N != fk(k, uint64(i)) {
					errs <- fmt.Errorf("goroutine %d: %s(%d) = %v, %v", g, name, i, res, err)
					return
				}
				ext, _ = inst.Export("ind")
				res, err = Invoke(ctx, ext.Func, []Val{{N: k}, {N: 3}})
				if err != nil || res[0].N != 1000+fk(k, 3) {
					errs <- fmt.Errorf("goroutine %d: ind(%d, 3) = %v, %v", g, k, res, err)
					return
				}
				ext, _ = inst.Export("grow")
				res, err = Invoke(ctx, ext.Func, []Val{{N: k}})
				if err != nil || res[0].N != uint32of(k*3+1) {
					errs <- fmt.Errorf("goroutine %d: grow(%d) = %v, %v", g, k, res, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	gcs.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	checkAccounting(t, code, b)
	if held := b.Held(); held > b.Limit() {
		t.Fatalf("holds %d past the limit %d", held, b.Limit())
	}
}

// Codes sharing a budget make room in each other's code: a compile of one
// drops the other's old code when its own has none to drop.
func TestJITBudgetShared(t *testing.T) {
	const n = 8
	unit, tables := unitSize(t, n)
	b := NewJITBudget(2*tables + 4*unit)
	steppingClock(t)
	host := hostH(func(context.Context, uint64) uint64 { return 0 })
	c1 := budgetCode(t, manyWAT(n), b)
	c2 := budgetCode(t, manyWAT(n), b)
	i1, i2 := instantiateCode(t, c1, host), instantiateCode(t, c2, host)
	for k := 0; k < 4; k++ {
		call(t, i1, fmt.Sprintf("f%d", k), 2)
	}
	if m := c1.JITMemory(); m.Functions != 4 {
		t.Fatalf("the first code compiled %d of 4", m.Functions)
	}
	for k := 0; k < n; k++ {
		res, err := call(t, i2, fmt.Sprintf("f%d", k), 2)
		if err != nil || res[0].N != fk(uint64(k), 2) {
			t.Fatalf("f%d(2) = %v, %v", k, res, err)
		}
	}
	if c1.JITMemory().Dropped == 0 {
		t.Fatal("the second code's compiles dropped none of the first's")
	}
	if b.Held() != c1.JITMemory().Held()+c2.JITMemory().Held() || b.Held() > b.Limit() {
		t.Fatalf("the budget holds %d, the codes %d and %d, the limit %d", b.Held(), c1.JITMemory().Held(), c2.JITMemory().Held(), b.Limit())
	}
}

// Tier-ups compiled in the background charge the budget as they are
// placed, from the workers' goroutines, while calls drop code: a function
// whose baseline code a pass dropped while its optimizing compile was
// queued gets the optimized code; one whose optimized code finds no room
// is refused for the budget and asked again later, not left uncounted; and the
// results are the same throughout.
func TestJITDropWithBackgroundTierUps(t *testing.T) {
	const n = 24
	unit, tables := unitSize(t, n)
	b := NewJITBudget(tables + 10*unit)
	slowClock(t, 100)
	code := budgetCode(t, manyWAT(n), b)
	code.setOptBackground(true)
	code.SetOptThreshold(20)
	// Calls from Go enter the functions' code, whose counts tier them up,
	// whatever WASM_JIT says.
	code.setOSRCalls(0)
	before := ReadJITStats()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for g := 0; g < 4; g++ {
		inst := instantiateCode(t, code, hostH(func(context.Context, uint64) uint64 { return 0 }))
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// Six functions, then six others, which the first six's
			// code, baseline and optimized, makes room for once old; and
			// then all of them, more than fit.
			for i := 0; i < 3000; i++ {
				k := uint64((g + i) % 6)
				switch {
				case i >= 2000:
					k = uint64((g*5 + i) % n)
				case i >= 1000:
					k += 6
				}
				ext, _ := inst.Export(fmt.Sprintf("f%d", k))
				res, err := Invoke(context.Background(), ext.Func, []Val{{N: uint64(i % 50)}})
				if err != nil || res[0].N != fk(k, uint64(i%50)) {
					errs <- fmt.Errorf("goroutine %d: f%d(%d) = %v, %v", g, k, i%50, res, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	code.waitOptBackground()
	// Then on one goroutine, the last twelve functions, for which the code
	// of the first twelve, old by then, is dropped whatever the workers
	// did meanwhile.
	inst := instantiateCode(t, code, hostH(func(context.Context, uint64) uint64 { return 0 }))
	for i := 0; i < 200; i++ {
		k := uint64(12 + i%12)
		if res, err := call(t, inst, fmt.Sprintf("f%d", k), 9); err != nil || res[0].N != fk(k, 9) {
			t.Fatalf("f%d(9) = %v, %v", k, res, err)
		}
	}
	code.waitOptBackground()
	after := ReadJITStats()
	t.Logf("optimized %d, refused for the budget %d, dropped %d and %d replaced", after.Optimized-before.Optimized,
		after.OptRefused[RefuseBudget]-before.OptRefused[RefuseBudget], after.Dropped-before.Dropped, after.DroppedReplaced-before.DroppedReplaced)
	// Which tier-ups find room depends on when the workers run.
	if after.Optimized+after.OptRefused[RefuseBudget] == before.Optimized+before.OptRefused[RefuseBudget] || after.Dropped == before.Dropped {
		t.Fatal("no tier-up compiled or refused for the budget, or no code dropped")
	}
	checkAccounting(t, code, b)
	if held := b.Held(); held > b.Limit() {
		t.Fatalf("holds %d past the limit %d", held, b.Limit())
	}
	// No function is left with a tier-up neither queued nor counted: a
	// refusal for the budget counts its calls again.
	cj := code.jit.Load()
	for k := 0; k < n; k++ {
		ext, _ := instantiateCode(t, code, hostH(func(context.Context, uint64) uint64 { return 0 })).Export(fmt.Sprintf("f%d", k))
		cd := code.get(ext.Func.Index)
		if nat := cd.native.Load(); nat != nil && !nat.opt && !cd.optDone.Load() && atomic.LoadInt64(&cj.counters[ext.Func.Index]) == math.MaxInt64 {
			t.Fatalf("f%d: baseline code, no tier-up done or queued, and its count stopped", k)
		}
	}
}

// The execs a pass reads (jitExecs) are swept of those collected once
// their list has doubled, so that making many -- as a pool that lets go of
// its execs does, under the race detector a quarter of those put back --
// costs a constant each, and the list stays within twice those alive.
func TestJITExecsSwept(t *testing.T) {
	if !JITAvailable() {
		t.Skip("no native code in this build")
	}
	for i := 0; i < 200000; i++ {
		jitTrack(new(exec))
		if i%5000 == 0 {
			runtime.GC()
		}
	}
	runtime.GC()
	jitTrack(new(exec))
	jitExecs.mu.Lock()
	n := len(jitExecs.list)
	jitExecs.mu.Unlock()
	if n > 20000 {
		t.Fatalf("%d execs listed after 200,000 made and collected", n)
	}
}
