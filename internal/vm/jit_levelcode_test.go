//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// levelCodeSetup's functions make native callees' levels that Go finds
// (jitRegLevelAt): calls of Go from deep in a recursion of native calls --
// a host function's, a rest parameter's, the arguments read, a stack trace,
// which makes the levels a call of Go put off -- calls in tail position
// that give a frame to another function, a generator resumed natively, and
// derived constructions. drop(k) is the host's: a no-op in the
// interpreter's runtime, and in the JIT's what the test makes it.
const levelCodeSetup = `
	'use strict'
	function leaf(a, ...r) { return r.length + a + probe() }
	function va() { return arguments.length + (arguments[0] | 0) }
	function rec(n, k) {
		if (n === 0) {
			let s = probe() + leaf(1, 2, 3) + va(4, 5)
			if (k) {
				drop(k)
				s += probe() + leaf(1, 2) + va(6)
				if (k > 3) throw new RangeError('at ' + k)
			}
			return s + (k === 3 ? new Error('t').stack.split('\n').length : 0)
		}
		return rec(n - 1, k) + va(n, n) + 1
	}
	function deep(n, k) { try { return rec(n, k) } catch (e) { return e.message } }
	function tailA(n, acc) { if (n === 0) return acc + probe(); return tailB(n - 1, acc + 1) }
	function tailB(n, acc) { return tailA(n, acc + va(n)) }
	function* gen(n) { for (let i = 0; i < n; i++) yield probe() + i }
	function gens(n) { let s = 0; for (const v of gen(n)) s += v; return s }
	class B { constructor(x) { this.x = probe() + x } }
	class D extends B { constructor(x) { super(x); this.y = va(x) } }
	function builds(n) { let s = 0; for (let i = 0; i < n; i++) s += new D(i).y + new B(i).x; return s }
	function all(n, k) { return [deep(n, k), tailA(n, 0), gens(n), builds(n)].join() }
	function many(m, f, n, k) { let s; for (let i = 0; i < m; i++) s = f(n, k); return s }
`

// levelCodeChecked has r's JIT check, at each level Go finds, that the code
// found is the code the level runs: its function's, and, where the level
// made a native call that has not returned, the code its callee returns
// into. It returns the count of levels found, and of those found running
// code the cache no longer holds for their function -- let go of, or
// invalid -- which no lookup by the function's code now finds.
func levelCodeChecked(t *testing.T) (found, retired *int) {
	t.Helper()
	found, retired = new(int), new(int)
	jitTestSwitch(t, func(jc *jitTestConfig) {
		jc.levelFound = func(r *Runtime, i int, e *jitRegEntry) {
			s := r.jit
			c := s.ssaCtxs[i]
			cl := (*closure)(c.Closure)
			*found++
			switch {
			case c.Live != abi.LiveCall:
				t.Errorf("a level found in context %d, which runs no native call", i)
			case e.code == nil || e.p == nil:
				t.Errorf("context %d's level found with no code", i)
			case e.key.Value() != cl.fn:
				t.Errorf("context %d's level of %s found running another function's code", i, cl.fn.Name)
			case i+1 < len(s.ssaCtxs) && s.ssaCtxs[i+1].Live == abi.LiveCall && !e.code.Contains(s.ssaCtxs[i+1].ReturnTo):
				t.Errorf("context %d's level of %s found running code its callee does not return into", i, cl.fn.Name)
			}
			if e.dropped || e.invalid || !e.cached {
				*retired++
			}
		}
	})
	return found, retired
}

// The context of a native call names the code it runs (abi.Context's Code),
// which Go reads a level's code from: at every level Go finds -- calls of Go
// from a recursion's levels, a stack trace's put-off levels, a tail call's
// frame given to another function, a generator's resumption, derived
// constructions -- the code found is the one the level runs, with the
// collector idle and marking.
func TestJITLevelNamesItsCode(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			found, _ := levelCodeChecked(t)
			rounds := []string{}
			for range 6 {
				rounds = append(rounds, "many(40, all, 20, 0)")
			}
			rounds = append(rounds, "all(30, 3)", "many(40, all, 30, 0)")
			jitLevelAgree(t, nil, rounds...)
			t.Logf("levels found: %d", *found)
			if *found == 0 {
				t.Fatal("Go found no native callee's level")
			}
		})
	}
}

// A level runs the code it was called in, and Go finds that code, however
// the cache has treated its function's since: let go of to make room,
// invalidated by a deoptimization, flushed, while the level ran -- each in
// the middle of a recursion of native calls, the levels then calling Go
// again and Go finishing them where they throw -- and in a realm of its own,
// let go of after. With the collector at work all along, under checkptr
// too, a level names no code that is gone.
func TestJITLevelCodeLetGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	paceCollector(t, 1)
	setEpoch := jitFlushForTest(t)
	epoch := uint64(1000)
	found, retired := levelCodeChecked(t)
	drops := 0
	drop := func(r *Runtime, k int) {
		drops++
		for _, name := range []string{"rec", "va", "leaf"} {
			cl := jitGlobalClosure(r, name)
			e := r.jit.regCache[r.jitRegKeyOf(cl)]
			if e == nil || e.code == nil {
				continue
			}
			switch k {
			case 1, 4:
				jitPortDrop(r, cl)
			case 2, 5:
				r.jitRegInvalidate(cl, e, "test", jitTierSite{cl: cl})
			case 3, 6:
				epoch += 4 * jitFlushAge
				setEpoch(epoch)
				r.jit.flush()
			}
		}
	}
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, "many(40, deep, 24, 0)")
	}
	for k := 1; k <= 6; k++ {
		rounds = append(rounds, fmt.Sprintf("deep(24, %d)", k))
		for range 4 {
			rounds = append(rounds, "many(40, deep, 24, 0)")
		}
	}
	r := jitLevelAgree(t, drop, rounds...)
	t.Logf("levels found: %d, %d of them running code let go of", *found, *retired)
	if drops < 6 {
		t.Fatalf("the code was let go of %d times under its levels", drops)
	}
	if *retired == 0 {
		t.Fatalf("of %d levels found, none ran code the cache had let go of", *found)
	}
	if len(r.jit.goPinned) != 0 || len(r.jit.regRetired) != 0 {
		t.Fatalf("levels left %d pinned, %d retired", len(r.jit.goPinned), len(r.jit.regRetired))
	}

	// The same in a realm of its own, its code let go of under its levels,
	// then the realm let go of and the cache flushed: the first realm's
	// levels find their own code.
	re := r.NewRealm()
	r.InRealm(re, func() { levelCodeHost(r, r, drop) })
	fn := compileForTest(t, levelCodeSetup)
	if _, err := r.RunIn(re, fn); err != nil {
		t.Fatal(err)
	}
	var drive Value
	r.InRealm(re, func() { drive = r.global.getOwn(r.atoms.intern("deep")).value() })
	for i := range 60 {
		v, err := r.Call(drive, Undefined, []Value{Float(24), Float(float64([]int{0, 1, 2, 0, 4}[i%5]))})
		if err != nil {
			t.Fatal(err)
		}
		if i%5 == 4 && i > 50 && v.String().Go() != "at 4" {
			t.Fatalf("the realm's deep(24, 4): %v", v)
		}
	}
	t.Logf("in a realm of its own: levels found %d, %d of them running code let go of", *found, *retired)
	re, drive = nil, Undefined
	for range 3 {
		epoch += 4 * jitFlushAge
		setEpoch(epoch)
		r.jit.flush()
	}
	for _, src := range []string{"many(40, deep, 24, 0)", "deep(24, 1)", "many(40, deep, 24, 0)"} {
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.jit.goPinned) != 0 || len(r.jit.regRetired) != 0 {
		t.Fatalf("levels left %d pinned, %d retired", len(r.jit.goPinned), len(r.jit.regRetired))
	}
}

// jitLevelAgree runs levelCodeSetup, then each round, in the interpreter and
// with the JIT, each runtime given probe and drop -- drop calling hook, if
// set, with the JIT's runtime and its argument -- and fails where the two differ; it returns the
// JIT's runtime.
func jitLevelAgree(t *testing.T, hook func(*Runtime, int), rounds ...string) *Runtime {
	t.Helper()
	want := newTestRuntime(t, Config{StackSize: collectingStack})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true, StackSize: collectingStack})
	for _, rt := range []*Runtime{want, r} {
		levelCodeHost(rt, r, hook)
		if _, err := rt.Run(compileForTest(t, levelCodeSetup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, werr := want.Run(compileForTest(t, "String("+src+")"))
		gv, gerr := r.Run(compileForTest(t, "String("+src+")"))
		if werr != nil || gerr != nil {
			t.Fatalf("round %d (%s): error %v, interpreter's %v", i, src, gerr, werr)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d (%s): got %q, interpreter %q", i, src, got, want)
		}
		if strings.Contains(wv.String().Go(), "maximum call stack size exceeded") {
			t.Fatalf("round %d (%s): out of a stack of %d values", i, src, collectingStack)
		}
	}
	return r
}

// levelCodeHost gives rt's global, in its realm now, levelCodeSetup's probe
// and drop: drop calls hook, if set, where rt is the JIT's runtime jr.
func levelCodeHost(rt, jr *Runtime, hook func(*Runtime, int)) {
	rt.global.setOwnRaw(rt.atoms.intern("probe"), rt.NewFunction("probe", 0, func(*Runtime, Value, []Value) (Value, error) {
		return Int(1), nil
	}), propDefault)
	rt.global.setOwnRaw(rt.atoms.intern("drop"), rt.NewFunction("drop", 1, func(rt *Runtime, _ Value, args []Value) (Value, error) {
		if hook != nil && rt == jr && len(args) > 0 {
			hook(rt, int(args[0].Number()))
		}
		return Undefined, nil
	}), propDefault)
}

// The stand-in frame a call of Go on a native callee's level is made on
// (standIn) is clear of whatever an earlier use left in it, every field but
// those jitRegFrameRun sets: a field the frame gains that standIn does not
// look at fails here, not as a stale value in a later call of Go.
func TestJITStandInFrameClear(t *testing.T) {
	set := map[string]bool{"cl": true, "base": true, "this": true, "newTarget": true, "callee": true}
	var s jitState
	ft := reflect.TypeOf(frame{})
	for i := range ft.NumField() {
		sf := ft.Field(i)
		if set[sf.Name] {
			continue
		}
		s.calleeFrame = frame{}
		fv := reflect.NewAt(sf.Type, unsafe.Add(unsafe.Pointer(&s.calleeFrame), sf.Offset)).Elem()
		switch sf.Type.Kind() {
		case reflect.Slice:
			fv.Set(reflect.MakeSlice(sf.Type, 0, 1))
		case reflect.Pointer:
			fv.Set(reflect.New(sf.Type.Elem()))
		case reflect.String:
			fv.SetString("x")
		case reflect.Bool:
			fv.SetBool(true)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			fv.SetInt(1)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			fv.SetUint(1)
		default:
			t.Fatalf("frame's %s, a %v, is set by no case here", sf.Name, sf.Type)
		}
		f := s.standIn()
		if !reflect.NewAt(sf.Type, unsafe.Add(unsafe.Pointer(f), sf.Offset)).Elem().IsZero() {
			t.Errorf("the stand-in frame keeps an earlier use's %s", sf.Name)
		}
	}
}

// The package's tests' runtimes panic where Go reads a context's code id
// that names no entry of its function's (jitRegCodeNamed).
func init() {
	testDefaults = append(testDefaults, func(c *testConfig) { c.jit.selfStrict = true })
}

// selvesChecked fails where r's JIT's ids of code (jitRegSelf) are not
// exactly those of the entries that have code or are compiled: every entry
// the cache holds with code, its code for a loop, the retired, the parked
// and the compiles in the background, each by its own id; no other id
// taken, none taken twice, none given back twice, and no context's id
// read that named no entry of its function's.
func selvesChecked(t *testing.T, r *Runtime, when string) {
	t.Helper()
	s := r.jit
	live := map[*jitRegEntry]bool{}
	add := func(e *jitRegEntry) {
		if e != nil && e.code != nil {
			live[e] = true
		}
		if e != nil && e.osr != nil && e.osr.code != nil {
			live[e.osr] = true
		}
	}
	for _, e := range s.regCache {
		add(e)
	}
	for _, e := range s.regParked {
		add(e)
	}
	for _, e := range s.regRetired {
		add(e)
	}
	for _, j := range s.regJobs {
		live[j.e] = true
	}
	taken := 0
	for i, e := range s.regSelves {
		if e == nil {
			continue
		}
		taken++
		switch {
		case i == 0:
			t.Errorf("%s: id 0 taken", when)
		case int(e.self) != i:
			t.Errorf("%s: id %d names an entry whose id is %d", when, i, e.self)
		case !live[e]:
			t.Errorf("%s: id %d held by an entry with no code and no compile (%s)", when, i, e.key.Value().Name)
		}
	}
	for e := range live {
		if e.self == 0 || int(e.self) >= len(s.regSelves) || s.regSelves[e.self] != e {
			t.Errorf("%s: an entry of %s with code or compiled has no id of its own (%d)", when, e.key.Value().Name, e.self)
		}
	}
	idle := map[uint32]bool{}
	for _, id := range s.regSelfIdle {
		if idle[id] || id == 0 || int(id) >= len(s.regSelves) || s.regSelves[id] != nil {
			t.Errorf("%s: id %d given back twice, or given back while taken", when, id)
		}
		idle[id] = true
	}
	if n := len(s.regSelves); n != 0 && taken+len(idle) != n-1 {
		t.Errorf("%s: %d ids taken and %d given back of a table of %d", when, taken, len(idle), n)
	}
	if s.regSelfStale != 0 {
		t.Errorf("%s: %d contexts' ids read that named no entry of their function's", when, s.regSelfStale)
	}
}

// The ids of code (jitRegSelf) are given back wherever code goes or a
// compile makes none, on the spot and in the background: after compiles,
// code let go of under its levels, invalidated, flushed, a realm's code and
// the realm let go of, the table holds exactly the entries with code or
// compiled, and ids given back are taken again, the table no longer than
// the most entries that had code at once.
func TestJITSelfIdsGivenBack(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%v", background), func(t *testing.T) {
			paceCollector(t, 1)
			setEpoch := jitFlushForTest(t)
			epoch := uint64(1000)
			flush := func(r *Runtime) {
				epoch += 4 * jitFlushAge
				setEpoch(epoch)
				r.jit.flush()
			}
			r := jitRuntimeForTest(t, Config{JIT: true, StackSize: collectingStack})
			if background {
				r.SetJITBackground(true)
			}
			levelCodeHost(r, r, func(r *Runtime, k int) {
				for _, name := range []string{"rec", "va", "leaf"} {
					cl := jitGlobalClosure(r, name)
					e := r.jit.regCache[r.jitRegKeyOf(cl)]
					if e == nil || e.code == nil {
						continue
					}
					switch k % 3 {
					case 1:
						jitPortDrop(r, cl)
					case 2:
						r.jitRegInvalidate(cl, e, "test", jitTierSite{cl: cl})
					default:
						flush(r)
					}
				}
			})
			if _, err := r.Run(compileForTest(t, levelCodeSetup)); err != nil {
				t.Fatal(err)
			}
			run := func(src string) {
				t.Helper()
				if _, err := r.Run(compileForTest(t, "String("+src+")")); err != nil {
					t.Fatalf("%s: %v", src, err)
				}
				r.jitDrain()
			}
			most := 0
			for round := range 8 {
				for range 4 {
					run("many(30, all, 20, 0)")
				}
				for k := 1; k <= 3; k++ {
					run(fmt.Sprintf("deep(24, %d)", k))
					run("many(20, deep, 24, 0)")
				}
				selvesChecked(t, r, fmt.Sprintf("round %d", round))
				most = max(most, len(r.jit.regSelves))
				if round%2 == 1 {
					flush(r)
					selvesChecked(t, r, fmt.Sprintf("round %d flushed", round))
				}
			}
			// A realm's code, then the realm let go of and its code flushed.
			re := r.NewRealm()
			r.InRealm(re, func() { levelCodeHost(r, r, nil) })
			if _, err := r.RunIn(re, compileForTest(t, levelCodeSetup)); err != nil {
				t.Fatal(err)
			}
			var drive Value
			r.InRealm(re, func() { drive = r.global.getOwn(r.atoms.intern("all")).value() })
			for range 40 {
				if _, err := r.Call(drive, Undefined, []Value{Float(20), Float(0)}); err != nil {
					t.Fatal(err)
				}
			}
			r.jitDrain()
			selvesChecked(t, r, "a realm's code")
			re, drive = nil, Undefined
			for range 4 {
				runtime.GC()
				flush(r)
			}
			selvesChecked(t, r, "the realm let go of")
			for range 3 {
				flush(r)
			}
			taken := 0
			for _, e := range r.jit.regSelves {
				if e != nil {
					taken++
				}
			}
			t.Logf("ids: table %d (most %d before the realm), %d taken, %d given back", len(r.jit.regSelves), most, taken, len(r.jit.regSelfIdle))
			if len(r.jit.regSelves) == 0 {
				t.Fatal("no code compiled")
			}
			// Taken again: as the workload's compiles go and come, the table
			// grows by no more than the most entries the realm added.
			run("many(30, all, 20, 0)")
			selvesChecked(t, r, "after")
		})
	}
}
