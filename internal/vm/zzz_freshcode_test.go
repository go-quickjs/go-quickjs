package vm

import (
	"fmt"
	"runtime"
	"testing"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// freshCodeRuns runs n scripts made by src, each compiled anew, as a host's
// Eval does for each request, with a collection after each thousand, and
// reports how many of the functions outlived it, of one in ten kept track of.
func freshCodeRuns(t *testing.T, r *Runtime, n int, src func(i int) string) (alive, kept int) {
	t.Helper()
	var refs []weak.Pointer[bytecode.Function]
	for i := range n {
		fn := compileForTest(t, src(i))
		if i%10 == 0 {
			refs = append(refs, weak.Make(fn))
		}
		if _, err := r.Run(fn); err != nil {
			t.Fatal(err)
		}
		if i%1000 == 999 {
			runtime.GC()
		}
	}
	runtime.GC()
	for _, w := range refs {
		if w.Value() != nil {
			alive++
		}
	}
	return alive, len(refs)
}

// Code with a tagged template, compiled for each request and run in one
// realm, stayed for as long as the realm: its table of template objects held
// each function, and so the function's script, source and all, 3 KB a run for
// a line of code. What a site's function is collected with, no code can ask
// for.
func TestTemplatesLeaveWithTheirCode(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	const runs = 5000
	alive, kept := freshCodeRuns(t, r, runs, func(i int) string {
		return fmt.Sprintf("var tag = s => s.raw[0]; tag`x%d`", i)
	})
	if alive > kept/2 {
		t.Errorf("%d of %d functions alive after their runs", alive, kept)
	}
	if n := len(r.templateCache); n > runs/2 {
		t.Errorf("%d functions in the table after %d runs", n, runs)
	}
}

// A template object is the same for as long as its site's code can run: over
// runs of one function in a realm, whichever run's closure asks, and with the
// table looked through, the function's script gone and collections between.
func TestTemplateObjectsLastAsLongAsTheirCode(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	script := compileForTest(t, `
		var tag = s => s
		function site() { return tag`+"`a${0}b`"+` }
		var other = () => tag`+"`a${0}b`"+`
		globalThis.kept ??= []
		kept.push(site(), site(), other(), tag`+"`a${0}b`"+`)
		kept.length`)
	run := func(fn *bytecode.Function) {
		t.Helper()
		if _, err := RunProgram[struct{}](r, fn); err != nil {
			t.Fatal(err)
		}
	}
	run(script)
	if _, err := r.Run(script); err != nil {
		t.Fatal(err)
	}
	// A closure whose top-level code has gone: its own function is what
	// keeps its site's object.
	closure := compileForTest(t, "globalThis.late = (s => () => s`late`)(s => s); globalThis.first = late()")
	if _, err := r.Run(closure); err != nil {
		t.Fatal(err)
	}
	closure = nil
	freshCodeRuns(t, r, 3000, func(i int) string {
		return fmt.Sprintf("(s => s)`x%d`", i)
	})
	run(script)
	runtime.KeepAlive(closure)

	check := func(src string) {
		t.Helper()
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if !v.IsBool() || !v.BoolValue() {
			t.Errorf("%s: not true", src)
		}
	}
	for i := 4; i < 12; i += 4 {
		check(fmt.Sprintf("kept[0] === kept[%d] && kept[1] === kept[%d]", i, i+1))
		check(fmt.Sprintf("kept[2] === kept[%d] && kept[3] === kept[%d]", i+2, i+3))
	}
	check("kept[0] === kept[1]")
	check("kept[0] !== kept[2] && kept[0] !== kept[3] && kept[2] !== kept[3]")
	check("late() === first")
	check("Object.isFrozen(first) && Object.isFrozen(first.raw) && first[0] === 'late'")
}

// Each realm has its own template object for a site, and keeps it over the
// other's.
func TestTemplateObjectsByRealm(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	fn := compileForTest(t, "(s => s)`in a realm`")
	a, b := r.NewRealm(), r.NewRealm()
	run := func(re *Realm) *Object {
		t.Helper()
		v, err := r.RunIn(re, fn)
		if err != nil {
			t.Fatal(err)
		}
		return v.Object()
	}
	x, y := run(a), run(b)
	if x == y {
		t.Error("two realms share a template object")
	}
	if run(a) != x || run(b) != y {
		t.Error("a realm's template object changed")
	}
}

// An address is the next function's once its function is collected: the entry
// of the function that had it is not the new one's.
func TestTemplateEntryOfACollectedFunction(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	fn := compileForTest(t, "(s => s)`new`")
	other := compileForTest(t, "(s => s)`old`")
	stale := &Object{}
	dead := func() weak.Pointer[bytecode.Function] {
		f := new(bytecode.Function)
		w := weak.Make(f)
		runtime.KeepAlive(f)
		return w
	}()
	for w := dead; w.Value() != nil; {
		runtime.GC()
	}
	for _, s := range []*templateSites{{fn: dead, objects: []*Object{stale}}, {fn: weak.Make(other), objects: []*Object{stale}}} {
		r.templateCache[uintptr(unsafe.Pointer(fn))] = s
		v, err := r.Run(fn)
		if err != nil {
			t.Fatal(err)
		}
		if o := v.Object(); o == stale || !o.IsArray() {
			t.Errorf("site given another function's template object: %v", v)
		}
	}
	runtime.KeepAlive(other)
}

// Each class fresh code made stayed on Object.prototype's list of the
// prototypes whose chains go through it, as a dead entry, for as long as
// nothing changed it. The list is cut down as it fills, and a change of the
// object still reaches the chains of those that live.
func TestProtoUsersOfGoneCode(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		class Kept {}
		globalThis.kept = new Kept()
		globalThis.read = () => kept.later
		for (var i = 0; i < 3; i++) read()`)); err != nil {
		t.Fatal(err)
	}
	const runs = 5000
	freshCodeRuns(t, r, runs, func(i int) string {
		return `{ class A { m() { return this.absent } }; new A().m(); new A().m(); new A().m() }`
	})
	users := 0
	if info := infoOf[struct{}](r.proto.object.shape); info != nil {
		users = len(info.users)
	}
	if users > runs/2 {
		t.Errorf("%d users of Object.prototype after %d classes", users, runs)
	}
	v, err := r.Run(compileForTest(t, `Object.prototype.later = 7; read()`))
	if err != nil {
		t.Fatal(err)
	}
	if v.Number() != 7 {
		t.Errorf("a read past a prototype of a live class after a change: %v, want 7", v)
	}
}

// A runtime with a source map loader kept each script that named a map and
// wrote a stack, source and map, for as long as it lived. A script that lives
// has its map read once, whatever went since.
func TestSourceMapsLeaveWithTheirScripts(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	loads := map[string]int{}
	r.SetSourceMapLoader(func(script, url string) ([]byte, string, bool) {
		loads[url]++
		return nil, "", false
	})
	stack := "new Error('x').stack\n"
	keep := compileForTest(t, stack+"//# sourceMappingURL=kept.map\n")
	if _, err := r.Run(keep); err != nil {
		t.Fatal(err)
	}
	const runs = 3000
	alive, kept := freshCodeRuns(t, r, runs, func(i int) string {
		return fmt.Sprintf("%s//# sourceMappingURL=%d.map\n", stack, i)
	})
	if alive > kept/2 {
		t.Errorf("%d of %d scripts' functions alive after their runs", alive, kept)
	}
	if n := len(r.sourceMaps.maps); n > runs/2 {
		t.Errorf("%d scripts in the table after %d runs", n, runs)
	}
	if _, err := r.Run(keep); err != nil {
		t.Fatal(err)
	}
	if n := loads["kept.map"]; n != 1 {
		t.Errorf("the kept script's map read %d times, want 1", n)
	}
}
