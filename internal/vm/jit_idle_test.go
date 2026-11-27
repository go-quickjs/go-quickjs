//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"
	"weak"
)

// jitParkSource is a program whose f calls g natively, f's code holding
// g's function object as its call's target, and g a 1 MB array (probe)
// through its closure.
const jitParkSource = `
	var probe;
	function make(k) {
		var bytes = new Uint8Array(1 << 20);
		for (var i = 0; i < 1024; i++) bytes[i] = (i * k) & 255;
		probe = bytes;
		return function g(i) { return bytes[i & 1023] + i };
	}
	function f(g, n) { var s = 0; for (var i = 0; i < n; i++) s = (s + g(i)) | 0; return s }
	var g = make(7);`

// jitParkWant is f(make(7), 100), Node's answer.
const jitParkWant = 16816

// jitParkRuntime runs jitParkSource in a runtime whose clocks the test
// sets, f hot and compiled to call g natively, in the background where bg
// says, and returns it with its clock's setter.
func jitParkRuntime(t *testing.T, bg bool) (*Runtime, func(uint64)) {
	t.Helper()
	setEpoch := jitFlushForTest(t)
	jitNoInlineForTest(t, "g")
	r := jitRuntimeForTest(t, Config{JIT: true})
	if bg {
		r.SetJITBackground(true)
	}
	if _, err := r.Run(compileForTest(t, jitParkSource)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		v, err := r.Run(compileForTest(t, `f(g, 100)`))
		if err != nil || v.Number() != jitParkWant {
			t.Fatalf("round %d: %v, %v", i, v, err)
		}
		r.jitDrain()
	}
	fl := jitGlobalClosure(r, "f")
	if e := r.jit.regCache[r.jitRegKeyOf(fl)]; e == nil || e.code == nil || len(e.life.nativeCalls) == 0 {
		t.Fatalf("f's code does not call g natively: %+v", e)
	}
	return r, setEpoch
}

// jitParkNow has r park its code at the host's next call's return: a
// flush age of the program's running after the last parking.
func jitParkNow(t *testing.T, r *Runtime, setEpoch func(uint64), at uint64) {
	t.Helper()
	setEpoch(at)
	parked := r.jit.parkedCount
	if _, err := r.Run(compileForTest(t, `0`)); err != nil {
		t.Fatal(err)
	}
	if r.jit.parkedCount == parked {
		t.Fatal("nothing parked as the runtime returned to its host")
	}
}

// Code parked as the runtime returns to its host holds what it was
// compiled against weakly: f's code, its call's target g, an object it
// compares its callee with, let go of by the program while f's code is
// parked, is collected, and with it the array g read. The next call of f
// finds g dead, lets f's code go, and gives the interpreter's answer, f
// compiled again once hot again; f's code never compares a callee with
// g's address once g is gone. In the foreground and the background, the
// collector idle and marking.
func TestJITParkedCodeLetsGoOfDeadObjects(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, bg := range []bool{false, true} {
		for _, marking := range jitMarkingModes() {
			t.Run(fmt.Sprintf("background=%v/marking=%v", bg, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				r, setEpoch := jitParkRuntime(t, bg)
				probe := weak.Make(r.global.getOwn(r.atoms.intern("probe")).value().Object())
				if _, err := r.Run(compileForTest(t, `probe = null; g = null;`)); err != nil {
					t.Fatal(err)
				}
				jitParkNow(t, r, setEpoch, 1000+jitFlushAge)
				fl := jitGlobalClosure(r, "f")
				key := r.jitRegKeyOf(fl)
				e := r.jit.regParked[key]
				if e == nil {
					t.Fatal("f's code not parked")
				}
				for i := 0; i < 3; i++ {
					runtime.GC()
				}
				if probe.Value() != nil {
					t.Fatal("the array g read is alive, kept by f's parked code")
				}
				for i := 0; i < 300; i++ {
					v, err := r.Run(compileForTest(t, `f(g || (g = make(7)), 100)`))
					if err != nil || v.Number() != jitParkWant {
						t.Fatalf("after the park, round %d: %v, %v", i, v, err)
					}
					r.jitDrain()
				}
				if r.jit.regParked[key] != nil || e.code != nil || e.parked != nil {
					t.Fatalf("f's parked code, whose target died, was not let go of: %+v", e)
				}
				if ne := r.jit.regCache[key]; ne == nil || ne == e || ne.code == nil {
					t.Fatalf("f was not compiled again once hot again: %+v", ne)
				}
				if err := regLinksHeldForTest(r.jit); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// Parked code whose objects all live is put back as it was at its next
// use, with no compile: f's code, g alive, runs natively again at the
// next call of f, the same entry, its link set again, and the answers are
// the interpreter's. Code parked and not used by the next parking goes.
func TestJITParkedCodeRestored(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, bg := range []bool{false, true} {
		for _, marking := range jitMarkingModes() {
			t.Run(fmt.Sprintf("background=%v/marking=%v", bg, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				r, setEpoch := jitParkRuntime(t, bg)
				fl := jitGlobalClosure(r, "f")
				key := r.jitRegKeyOf(fl)
				e := r.jit.regCache[key]
				jitParkNow(t, r, setEpoch, 1000+jitFlushAge)
				if r.jit.regParked[key] != e || e.parked == nil {
					t.Fatal("f's code not parked")
				}
				if l := r.jit.regLinks[key]; l != nil && l.entry != 0 {
					t.Fatal("a parked function's link calls its code")
				}
				for i := 0; i < 3; i++ {
					runtime.GC()
				}
				compiled := r.JITStats().Compiled
				unparked := r.jit.unparked
				for i := 0; i < 50; i++ {
					v, err := r.Run(compileForTest(t, `f(g, 100)`))
					if err != nil || v.Number() != jitParkWant {
						t.Fatalf("after the park, round %d: %v, %v", i, v, err)
					}
				}
				if r.jit.regCache[key] != e || e.parked != nil || e.code == nil || !e.cached {
					t.Fatalf("f's code was not put back: %+v", e)
				}
				if r.jit.unparked == unparked {
					t.Fatal("nothing unparked")
				}
				if c := r.JITStats().Compiled; c != compiled {
					t.Fatalf("%d compiles after the park", c-compiled)
				}
				if err := regLinksHeldForTest(r.jit); err != nil {
					t.Fatal(err)
				}
				// Parked again a flush age later, and unused until the next
				// parking: let go of then.
				jitParkNow(t, r, setEpoch, 1000+2*jitFlushAge)
				setEpoch(1000 + 3*jitFlushAge)
				if _, err := r.Run(compileForTest(t, `0`)); err != nil {
					t.Fatal(err)
				}
				if r.jit.regParked[key] != nil || e.code != nil {
					t.Fatalf("f's code parked and unused for a flush age was kept: %+v", e)
				}
				if v, err := r.Run(compileForTest(t, `f(g, 100)`)); err != nil || v.Number() != jitParkWant {
					t.Fatalf("after its code went: %v, %v", v, err)
				}
			})
		}
	}
}

// A parked entry that holds the package's static objects -- the caches'
// sentinels noShape, dictShape and setterNext, and absentHolder, which a
// cache entry for a property found nowhere holds, all in the binary's
// data rather than the heap -- keeps them as they are, strongly: a weak
// pointer cannot be made to one (Go 1.24's weak.Make threw "getWeakHandle
// on invalid pointer" parking a polymorphic site's entry whose ownShape
// was noShape), and none dies. The code is put back at its next use as
// with any other, its statics in place.
func TestJITParkedCodeKeepsStatics(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, setEpoch := jitParkRuntime(t, false)
	fl := jitGlobalClosure(r, "f")
	key := r.jitRegKeyOf(fl)
	e := r.jit.regCache[key]
	statics := []*shape{noShape, dictShape, setterNext, absentHolder.shape}
	// What a site's entries for them hold: their shapes, the holder, and a
	// cell holding the holder as a value.
	kb := e.keep
	shapes, holders, cells := len(kb.shapes), len(kb.holders), len(kb.cells)
	kb.shapes = append(kb.shapes, statics...)
	kb.holders = append(kb.holders, absentHolder)
	held := Obj(absentHolder)
	kb.cells = append(kb.cells, &held)
	jitParkNow(t, r, setEpoch, 1000+jitFlushAge)
	if r.jit.regParked[key] != e || e.parked == nil || len(e.parked.slots) == 0 {
		t.Fatal("f's code not parked")
	}
	check := func(when string) {
		t.Helper()
		if !slices.Equal(kb.shapes[shapes:], statics) {
			t.Fatalf("%s, the statics' shapes are %v, not %v", when, kb.shapes[shapes:], statics)
		}
		if kb.holders[holders] != absentHolder || kb.cells[cells].ref != unsafe.Pointer(absentHolder) {
			t.Fatalf("%s, absentHolder is not held", when)
		}
	}
	check("parked")
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	for i := 0; i < 50; i++ {
		v, err := r.Run(compileForTest(t, `f(g, 100)`))
		if err != nil || v.Number() != jitParkWant {
			t.Fatalf("after the park, round %d: %v, %v", i, v, err)
		}
	}
	if r.jit.regCache[key] != e || e.parked != nil || e.code == nil || !e.cached {
		t.Fatalf("f's code was not put back: %+v", e)
	}
	check("put back")
}

// jitParkStatic names every object of a type a parked entry holds weakly
// (jitParkWeak) that the package makes statically: the address of a
// composite literal of *Object, *closure, *shape or *bytecode.Function
// type in a package-level variable's initializer, which the compiler
// allocates in the binary's data, or a package-level variable of such a
// type itself. A weak pointer cannot be made to one, and parking code
// whose caches hold one threw on Go 1.24, which the Go of the local runs
// did not.
func TestJITParkStaticsListed(t *testing.T) {
	listed := map[string][]unsafe.Pointer{
		"noShape":      {unsafe.Pointer(noShape)},
		"dictShape":    {unsafe.Pointer(dictShape)},
		"setterNext":   {unsafe.Pointer(setterNext)},
		"absentHolder": {unsafe.Pointer(absentHolder), unsafe.Pointer(absentHolder.shape)},
	}
	weakType := func(pkg string, x ast.Expr) bool {
		switch x := x.(type) {
		case *ast.Ident:
			if pkg == "bytecode" {
				return x.Name == "Function"
			}
			return x.Name == "Object" || x.Name == "closure" || x.Name == "shape"
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			return ok && pkg == "vm" && id.Name == "bytecode" && x.Sel.Name == "Function"
		}
		return false
	}
	found := map[string]int{}
	fset := token.NewFileSet()
	for pkg, dir := range map[string]string{"vm": ".", "bytecode": "../bytecode"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v, %d files", dir, err, len(files))
		}
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range f.Decls {
				g, ok := d.(*ast.GenDecl)
				if !ok || g.Tok != token.VAR {
					continue
				}
				for _, sp := range g.Specs {
					vs := sp.(*ast.ValueSpec)
					static := 0
					if vs.Type != nil {
						ty := vs.Type
						if a, ok := ty.(*ast.ArrayType); ok && a.Len != nil {
							ty = a.Elt
						}
						if weakType(pkg, ty) {
							static++
						}
					}
					for _, v := range vs.Values {
						ast.Inspect(v, func(n ast.Node) bool {
							switch n := n.(type) {
							case *ast.FuncLit:
								// What a function makes, at the package's
								// initialization or later, is the heap's.
								return false
							case *ast.UnaryExpr:
								if c, ok := n.X.(*ast.CompositeLit); ok && n.Op == token.AND && weakType(pkg, c.Type) {
									static++
								}
							}
							return true
						})
					}
					if static != 0 {
						for _, id := range vs.Names {
							found[pkg+"."+id.Name] += static
						}
					}
				}
			}
		}
	}
	for name, n := range found {
		short := strings.TrimPrefix(name, "vm.")
		if len(listed[short]) != n {
			t.Errorf("%s makes %d static objects a parked entry would hold weakly; the test lists %d: "+
				"have jitParkStatic hold them and list them here", name, n, len(listed[short]))
		}
	}
	for name, ps := range listed {
		if found["vm."+name] == 0 {
			t.Errorf("%s is listed, but makes no static object", name)
		}
		for i, p := range ps {
			if !jitParkStatic(p) {
				t.Errorf("%s's static object %d is not jitParkStatic's", name, i)
			}
		}
	}
}

// jitParkClockForTest has parking's clock of the runtimes the test makes
// (parkClock) go by step at each read, the cost of what a parking or an
// unparking does between two reads, and by what the returned function
// adds, a turn's running; the flush's clocks stay held, so that no flush
// age goes by.
func jitParkClockForTest(t *testing.T, step time.Duration) (run func(time.Duration), now func() time.Duration) {
	t.Helper()
	var clock time.Duration
	jitTestSwitch(t, func(jc *jitTestConfig) {
		jc.parkNow = func() time.Duration { clock += step; return clock }
	})
	return func(d time.Duration) { clock += d }, func() time.Duration { return clock }
}

// The code a program's last turn compiled is parked as the turn returns,
// once the turn has run long enough to pay for the parking, however few of
// the flush's ticks have gone by since the last parking -- none here -- and
// what only that code held is collected: f's code, compiled in the turn to
// call g natively, held g and through it a 16 MB array, which the turn let
// go of. Parked once a flush age of the program's running, after Octane in
// one runtime the code Typescript compiled after the last parking had kept
// some 190 MB of its heap.
func TestJITLastTurnParked(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	run, _ := jitParkClockForTest(t, time.Microsecond)
	jitNoInlineForTest(t, "g")
	r := jitRuntimeForTest(t, Config{JIT: true})
	var probe weak.Pointer[Object]
	keep := r.NewFunction("keep", 1, func(r *Runtime, this Value, args []Value) (Value, error) {
		probe = weak.Make(args[0].Object())
		return Undefined, nil
	})
	if err := r.SetProp(Obj(r.Global()), r.Intern("keep"), keep); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(compileForTest(t, `
		function make(k) {
			var bytes = new Uint8Array(16 << 20);
			for (var i = 0; i < 1024; i++) bytes[i] = (i * k) & 255;
			keep(bytes);
			return function g(i) { return bytes[i & 1023] + i };
		}
		function f(g, n) { var s = 0; for (var i = 0; i < n; i++) s = (s + g(i)) | 0; return s }`)); err != nil {
		t.Fatal(err)
	}
	run(time.Second)
	if _, err := r.Run(compileForTest(t, `0`)); err != nil {
		t.Fatal(err)
	}
	parkings := r.jitState().parkings
	// The last turn: f made hot and compiled to call g natively, then g
	// let go of.
	run(time.Second)
	v, err := r.Run(compileForTest(t, `
		var g = make(7), s = 0;
		for (var k = 0; k < 300; k++) s = f(g, 100);
		g = null;
		s`))
	if err != nil || v.Number() != jitParkWant {
		t.Fatalf("%v, %v", v, err)
	}
	if r.jit.parkings == parkings {
		t.Fatal("not parked as the last turn returned")
	}
	e := r.jit.regParked[r.jitRegKeyOf(jitGlobalClosure(r, "f"))]
	if e == nil || e.parked == nil || !e.callsNative {
		t.Fatalf("f's code, calling g natively, not parked: %+v", e)
	}
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	if probe.Value() != nil {
		t.Fatal("the array g read is alive, kept by the code the last turn compiled")
	}
}

// Parking costs at most a jitParkPays'th of the program's running: a host
// whose calls return thousands of times parks only as often as the
// running since the last parking pays for what it and the unparkings since
// cost -- each read of parking's clock here a microsecond of it, each turn
// a microsecond besides -- and a turn too short to measure parks nothing.
func TestJITParkingsBounded(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const step = time.Microsecond
	run, now := jitParkClockForTest(t, step)
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function f(n) { var s = 0; for (var i = 0; i < n; i++) s += i; return s }`)); err != nil {
		t.Fatal(err)
	}
	prog := compileForTest(t, `f(10)`)
	const turns = 5000
	s := r.jitState()
	from, parkings, unparked := now(), s.parkings, s.unparked
	for i := 0; i < turns; i++ {
		run(step)
		if v, err := r.Run(prog); err != nil || v.Number() != 45 {
			t.Fatalf("turn %d: %v, %v", i, v, err)
		}
	}
	n, ran := r.jit.parkings-parkings, now()-from
	t.Logf("%d turns: %d parkings, %d unparkings, %v of parking's clock", turns, n, r.jit.unparked-unparked, ran)
	// Each parking costs at least a step, the two reads around it.
	if n == 0 || n > uint64(ran/(jitParkPays*step))+1 {
		t.Fatalf("%d parkings in %v of running, each costing a step or more", n, ran)
	}
	if r.jit.unparked == unparked {
		t.Fatal("f's code, parked, was never put back")
	}
	// A turn that runs nothing by the clock parks nothing.
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.parkNow = func() time.Duration { return 0 } })
	parkings = r.jit.parkings
	for i := 0; i < 100; i++ {
		if _, err := r.Run(prog); err != nil {
			t.Fatal(err)
		}
	}
	if r.jit.parkings != parkings {
		t.Fatalf("%d parkings of turns that ran nothing", r.jit.parkings-parkings)
	}
}

// A parking measures what it cost by a clock that resolves it (parkSpan):
// a parking of f's code, some microseconds of walking it, costs more than
// none, so that the next is due only once the program has run some
// hundreds of times as long (parkDue). Measured by the program's clock,
// which on Windows moves at the system timer's tick, it cost none in most
// parkings, and the runtime parked at every return a tick after the last:
// each of Octane's benchmarks run once parked its code at its end. The
// runtime's clocks run here, as a host's do.
func TestJITParkCostMeasured(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	if jitFastClocks {
		t.Skip("parking's clock is the fast clocks' ticks")
	}
	jitNoInlineForTest(t, "g")
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitRunClocksForTest(t, r)
	if _, err := r.Run(compileForTest(t, jitParkSource)); err != nil {
		t.Fatal(err)
	}
	// The rounds that compile f park nothing, their turns running nothing
	// by parking's clock: with it running, a parking was due at whichever
	// return the program's time had paid for one, the last round's among
	// them on a loaded machine, which left f parked before the parking
	// this test measures.
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.parkNow = func() time.Duration { return 0 } })
	for i := 0; i < 300; i++ {
		v, err := r.Run(compileForTest(t, `f(g, 100)`))
		if err != nil || v.Number() != jitParkWant {
			t.Fatalf("round %d: %v, %v", i, v, err)
		}
	}
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.parkNow = nil })
	s := r.jit
	if e := s.regCache[r.jitRegKeyOf(jitGlobalClosure(r, "f"))]; e == nil || e.code == nil {
		t.Fatalf("f not compiled: %+v", e)
	}
	parked := s.parkedCount
	s.jitRegPark()
	if s.parkedCount == parked {
		t.Fatal("nothing parked")
	}
	if s.parkCost <= 0 {
		t.Fatalf("a parking of %d entries measured as costing %v", s.parkedCount-parked, s.parkCost)
	}
	if v, err := r.Run(compileForTest(t, `f(g, 100)`)); err != nil || v.Number() != jitParkWant {
		t.Fatalf("after the parking: %v, %v", v, err)
	}
}

// The process's idle workspaces go at the flush's look, once a flush age
// of the program's running, as the code the flush lets go of does
// (jit.DropIdleWorkspaces): not at a parking, nor at each of the host's
// returns, of which a program's turn has many, so that the next turn's
// compiles take the workspace the last gave back rather than make one
// anew -- dropped at every second return, each of Octane's benchmarks run
// once made its own, a megabyte and its pages faulted in again. A runtime
// whose host's calls go on returning with no compile has a flush look at
// its workspaces once a flush age has gone by, and the next look lets go
// of those not used between (internal/jit's TestWorkspaceDroppedIdle).
func TestJITWorkspacesDroppedAtTheFlush(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function f(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + (i * 7) % 13) | 0; return s }`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		// Node: f(1000)
		if v, err := r.Run(compileForTest(t, `f(1000)`)); err != nil || v.Number() != 6000 {
			t.Fatalf("f(1000): %v, %v", v, err)
		}
	}
	if _, code := jitCachedForTest(r, "f"); !code {
		t.Fatal("f not compiled")
	}
	s := r.jit
	looks := s.workLooks
	idle := compileForTest(t, `0`)
	// A parking and the host's returns within a flush age: no look.
	parked := s.parkedCount
	s.jitRegPark()
	if s.parkedCount == parked {
		t.Fatal("nothing parked")
	}
	for i := 0; i < 20; i++ {
		if _, err := r.Run(idle); err != nil {
			t.Fatal(err)
		}
	}
	if s.workLooks != looks {
		t.Fatalf("%d looks at the workspaces within a flush age", s.workLooks-looks)
	}
	// The host's calls of an idle runtime, two flush ages apart: a look at
	// each.
	for _, e := range []uint64{1000 + jitFlushAge, 1000 + 2*jitFlushAge} {
		setEpoch(e)
		if _, err := r.Run(idle); err != nil {
			t.Fatal(err)
		}
	}
	if s.workLooks != looks+2 {
		t.Fatalf("%d looks at the workspaces over two flush ages, want 2", s.workLooks-looks)
	}
}
