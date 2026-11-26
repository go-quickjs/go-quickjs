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
