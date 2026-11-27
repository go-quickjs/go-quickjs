//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
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

// jitParkPlanProgram is code whose compiles meet much of what an entry
// holds: closures called natively and inlined, literals' pools, shapes
// and prototypes, accessors, cells of constants, typed arrays, calls of
// built-ins and constructions.
const jitParkPlanProgram = `
	function Point(x, y) { this.x = x; this.y = y }
	Point.prototype.norm = function () { return this.x * this.x + this.y * this.y };
	Object.defineProperty(Point.prototype, 'sum', { get() { return this.x + this.y } });
	var bytes = new Uint8Array(64);
	function make(k) { return function (i) { return bytes[i & 63] + k + i } }
	var g = make(3), h = make(5);
	function lit(i) { return { a: i, b: [i, i + 1], c: 'k' + (i & 3), f: () => i } }
	function arr(n) { var a = []; for (var i = 0; i < n; i++) a.push(new Point(i, -i)); return a }
	function run(n) {
		var s = 0, a = arr(8);
		for (var i = 0; i < n; i++) {
			var o = lit(i), p = a[i & 7];
			s = (s + g(i) + h(i) + o.b[1] + p.norm() + p.sum + Math.max(i, 3) + o.c.length) | 0;
			bytes[i & 63] = s & 255;
		}
		return s;
	}`

// A parking finds, by its plans (jitParkPlan), exactly the pointers
// reflect's walk of what it parks finds: the slots it holds weakly and the
// pools it empties, its pools' sources' slots among them, entry by entry,
// over the code the program above compiles and, where
// QUICKJS_PARK_PLAN_DIRS names directories of Octane's or the V8 suite's
// files, every benchmark's of each run once.
func TestJITParkPlanMatchesWalk(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	suites := []jitParkPlanSuite{{"program", []string{jitParkPlanProgram}, `run(500)`, 3}}
	if dirs := os.Getenv("QUICKJS_PARK_PLAN_DIRS"); dirs != "" {
		for _, dir := range filepath.SplitList(dirs) {
			names, err := filepath.Glob(filepath.Join(dir, "*.js"))
			if err != nil {
				t.Fatal(err)
			} else if len(names) == 0 {
				t.Fatalf("%s: no .js files", dir)
			}
			s := jitParkPlanSuite{name: dir, run: `(() => {
				for (const s of BenchmarkSuite.suites) for (const b of s.benchmarks) {
					// A benchmark that needs what the host gives, as eval, is left out.
					try { b.Setup(); b.run(); b.TearDown() } catch (e) {}
				}
				return 0 })()`}
			// base.js first; run.js, the suite's own driver, not at all.
			slices.Sort(names)
			slices.SortStableFunc(names, func(a, b string) int {
				return cmpBool(filepath.Base(b) == "base.js", filepath.Base(a) == "base.js")
			})
			for _, n := range names {
				if filepath.Base(n) == "run.js" {
					continue
				}
				b, err := os.ReadFile(n)
				if err != nil {
					t.Fatal(err)
				}
				s.src = append(s.src, string(b))
			}
			suites = append(suites, s)
		}
	}
	for _, su := range suites {
		// Each suite's runtime a subtest's, let go of at its end.
		t.Run(filepath.Base(su.name), func(t *testing.T) { jitParkPlanCheck(t, su) })
	}
}

// jitParkPlanSuite is a program TestJITParkPlanMatchesWalk parks the code
// of: its files, and what runs it.
type jitParkPlanSuite struct {
	name string
	src  []string
	run  string
	// runs is how many times run runs before the parking, and once
	// after; none, once before and not after.
	runs int
}

// jitParkPlanCheck runs su's files, then its run, and parks
// the code compiled entry by entry, holding each parking to reflect's walk.
func jitParkPlanCheck(t *testing.T, su jitParkPlanSuite) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	// Nothing parks but what the test parks.
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.parkNow = func() time.Duration { return 0 } })
	for i, src := range su.src {
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			if i == 0 {
				t.Fatalf("%s: %v", su.name, err)
			}
			// A file that needs what the host gives, as eval.
			t.Logf("%s: file %d: %v", su.name, i, err)
		}
	}
	// A suite's code compiled once at its first call, the threshold one; a
	// suite run again keeps what each run made, as much as the memory has.
	for i := 0; i < max(1, su.runs); i++ {
		if _, err := r.Run(compileForTest(t, su.run)); err != nil {
			t.Fatalf("%s: %v", su.name, err)
		}
	}
	s := r.jit
	var keys []jitKey
	for key, e := range s.regCache {
		if e.code != nil && !key.gone() && !s.regInUse(e) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		t.Fatalf("%s: nothing compiled", su.name)
	}
	weaks := map[unsafe.Pointer]weak.Pointer[byte]{}
	slots := 0
	for _, key := range keys {
		e := s.regCache[key]
		want := &jitParkWalkForTest{s: s, poolSlots: map[*abi.ObjectPool][]*unsafe.Pointer{}}
		if e.keep != nil {
			want.walk(reflect.ValueOf(e.keep).Elem())
		}
		want.walk(reflect.ValueOf(&e.targets).Elem())
		if l := e.life; l != nil && l.jitLearned != nil {
			want.walk(reflect.ValueOf(l.jitLearned).Elem())
		}
		s.regParkEntry(key, e, s.flushNow(), weaks)
		p := e.parked
		var got []*unsafe.Pointer
		for _, sl := range p.slots {
			got = append(got, sl.at)
		}
		if !slices.Equal(got, want.slots) {
			missed := ""
			for i, at := range want.slots {
				if !slices.Contains(got, at) {
					missed = want.paths[i]
					break
				}
			}
			t.Fatalf("%s: %s's code: the plans hold %d slots, reflect's walk %d, %s among them", su.name, key.Value().Name, len(got), len(want.slots), missed)
		}
		if !slices.Equal(p.pools, want.pools) {
			t.Fatalf("%s: %s's code: the plans empty %d pools, reflect's walk %d", su.name, key.Value().Name, len(p.pools), len(want.pools))
		}
		for pool, ws := range want.poolSlots {
			var gs []*unsafe.Pointer
			for _, sl := range s.parkedPools[pool].slots {
				gs = append(gs, sl.at)
			}
			if !slices.Equal(gs, ws) {
				t.Fatalf("%s: %s's code: a pool's source: the plans hold %d slots, reflect's walk %d", su.name, key.Value().Name, len(gs), len(ws))
			}
		}
		slots += len(got)
	}
	t.Logf("%s: %d entries parked, %d slots, %d objects", su.name, len(keys), slots, len(weaks))
	// What was parked is put back as it was, and runs on.
	if su.runs == 0 {
		return
	}
	if _, err := r.Run(compileForTest(t, su.run)); err != nil {
		t.Fatalf("%s: after the parking: %v", su.name, err)
	}
}

// cmpBool orders false before true.
func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// jitParkWalkForTest is the walk parking made by reflect before its plans,
// which reads, and leaves, what a parking would hold: the slots, in the
// order it meets them, the pools it would empty, and what the pools'
// sources hold that no parking emptied yet.
type jitParkWalkForTest struct {
	s         *jitState
	slots     []*unsafe.Pointer
	pools     []*abi.ObjectPool
	poolSlots map[*abi.ObjectPool][]*unsafe.Pointer
	seen      map[unsafe.Pointer]bool
	held      map[*unsafe.Pointer]bool
	src       *[]*unsafe.Pointer
	// path is where the walk is, paths where it found each of slots.
	path  []string
	paths []string
}

func (w *jitParkWalkForTest) hold(at *unsafe.Pointer) {
	// A slot reached again, by another of the structures that share it,
	// a parking found cleared.
	if *at == nil || jitParkStatic(*at) || w.held[at] {
		return
	}
	if w.held == nil {
		w.held = map[*unsafe.Pointer]bool{}
	}
	w.held[at] = true
	if w.src != nil {
		*w.src = append(*w.src, at)
		return
	}
	w.slots = append(w.slots, at)
	w.paths = append(w.paths, strings.Join(w.path, "."))
}

func (w *jitParkWalkForTest) once(x unsafe.Pointer) bool {
	if w.seen[x] {
		return false
	}
	if w.seen == nil {
		w.seen = map[unsafe.Pointer]bool{}
	}
	w.seen[x] = true
	return true
}

func (w *jitParkWalkForTest) walk(v reflect.Value) {
	t := v.Type()
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		at := (*unsafe.Pointer)(unsafe.Pointer(v.UnsafeAddr()))
		switch {
		case jitParkWeak[t]:
			w.hold(at)
		case t == jitParkPool:
			pool := (*abi.ObjectPool)(*at)
			if !w.once(*at) {
				return
			}
			w.pools = append(w.pools, pool)
			if w.s.parkedPools[pool] != nil {
				return
			}
			if src := (*jitPoolSource)(pool.Source); src != nil {
				var ss []*unsafe.Pointer
				sw := &jitParkWalkForTest{src: &ss}
				sw.walk(reflect.ValueOf(src).Elem())
				w.poolSlots[pool] = ss
			}
		case jitParkInto[t]:
			if w.once(*at) {
				w.walk(v.Elem())
			}
		}
	case reflect.Struct:
		if t == jitParkValue {
			x := (*Value)(unsafe.Pointer(v.UnsafeAddr()))
			if b := math.Float64bits(x.num); b == objectBits || b == accessorBits || b == templateBits {
				w.hold(&x.ref)
			}
			return
		}
		if t.PkgPath() != jitParkValue.PkgPath() {
			return
		}
		for i := range v.NumField() {
			w.path = append(w.path, t.Name()+"."+t.Field(i).Name)
			w.walk(v.Field(i))
			w.path = w.path[:len(w.path)-1]
		}
	case reflect.Slice:
		if t == jitParkCells {
			for i := range v.Len() {
				if c := v.Index(i); !c.IsNil() {
					w.walk(c.Elem())
				}
			}
			return
		}
		if !jitParkElem(t.Elem()) {
			return
		}
		for i := range v.Len() {
			w.walk(v.Index(i))
		}
	case reflect.Array:
		if !jitParkElem(t.Elem()) {
			return
		}
		for i := range v.Len() {
			w.walk(v.Index(i))
		}
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

// jitParkElem reports whether elements of type t may hold what reflect's
// walk weakens: a pointer of those types, a pool, a structure an entry
// owns, or a struct or array of this package's.
func jitParkElem(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer:
		return jitParkWeak[t] || jitParkInto[t] || t == jitParkPool
	case reflect.Struct:
		return t.PkgPath() == jitParkValue.PkgPath()
	case reflect.Array, reflect.Slice:
		return jitParkElem(t.Elem())
	}
	return false
}
