package vm

import (
	"fmt"
	"testing"
)

// globalSitesOf is how many of the global sites of the code of the
// function the global name holds the interpreter has filled, and of those
// how many are current: read where they remember while the global epoch
// is what it is (rhGetGlobal, rhSetGlobal). A leaf's reads, which no
// frame runs, fill none: the functions it is asked of make a call.
func globalSitesOf(t *testing.T, r *Runtime, name string) (filled, current int) {
	t.Helper()
	p := r.global.getOwn(r.atoms.intern(name))
	if !p.ok() || !p.value().IsObject() || p.value().Object().fn() == nil || p.value().Object().fn().closure == nil {
		t.Fatalf("%s is not a function", name)
	}
	for _, ep := range p.value().Object().fn().closure.regEpochs {
		if ep != 0 {
			filled++
			if ep == r.shapes.globalEpoch+1 {
				current++
			}
		}
	}
	return filled, current
}

// A script-level lexical binding declared changes, of what the
// interpreter's global sites remember, the global object's binding of its
// name alone, which it shadows: a site of another name goes on reading
// and writing the slot it remembers; one of the name reads the lexical
// binding from then on, as does one of a name only the global object's
// prototype has, or that the global object comes to have after the
// declaration.
func TestGlobalLexicalDeclarationKeepsGlobalSites(t *testing.T) {
	const setup = `
		var gv = 3
		globalThis.sh = 10
		globalThis.sw = 0
		Object.prototype.pq = 5
		Object.defineProperty(globalThis, 'acc', { get() { return 7 }, configurable: true })
		function id(x) { return x }
		function rg() { return id(gv) }
		function rs() { return id(sh) }
		function rp() { return id(pq) }
		function ra() { return id(acc) }
		function wg(v) { gv = id(v) }
		function ws(v) { sw = id(v) }
		function rq() { return id(q) }
		for (let k = 0; k < 5; k++) { rg(); rs(); rp(); ra(); wg(3); ws(0) }
	`
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			r := newTestRuntime(t, nativeConfig(native))
			t.Cleanup(func() { r.Close() })
			run := func(src string) string {
				t.Helper()
				v, err := r.Run(compileForTest(t, src))
				if err != nil {
					t.Fatalf("%s: %v", src, err)
				}
				if !v.IsString() {
					return ""
				}
				return v.String().Go()
			}
			run(setup)
			for _, name := range []string{"rg", "rs", "wg", "ws"} {
				if filled, current := globalSitesOf(t, r, name); filled != 2 || current != 2 {
					t.Fatalf("%s: %d sites filled, %d current, want 2 and 2", name, filled, current)
				}
			}
			epoch := r.shapes.globalEpoch

			// Names the global object has none of, its prototype's
			// among them: no site's slot moves.
			run(`let later = 1; const two = 2; class Later {}; let pq = 6`)
			if r.shapes.globalEpoch != epoch {
				t.Errorf("global epoch %d after declarations of names the global object has none of, want %d", r.shapes.globalEpoch, epoch)
			}
			for _, name := range []string{"rg", "rs", "wg", "ws"} {
				if filled, current := globalSitesOf(t, r, name); filled != 2 || current != 2 {
					t.Errorf("%s: %d sites filled, %d current after the declarations, want 2 and 2", name, filled, current)
				}
			}
			if got := run(`wg(4); '' + [rg(), rg(), rp(), rp(), ra(), gv]`); got != "4,4,6,6,7,4" {
				t.Errorf("after the declarations: %s, want 4,4,6,6,7,4", got)
			}

			// The global object's own: its sites read and write the
			// lexical bindings from then on, its own bindings left as
			// they were.
			run(`let sh = 20; let acc = 8; let sw = 1`)
			if r.shapes.globalEpoch == epoch {
				t.Errorf("global epoch unchanged by a declaration that shadows the global object's binding")
			}
			if got := run(`ws(9); ws(9); '' + [rs(), rs(), ra(), ra(), sw, globalThis.sh, globalThis.sw]`); got != "20,20,8,8,9,10,0" {
				t.Errorf("after the shadowing declarations: %s, want 20,20,8,8,9,10,0", got)
			}
			if filled, current := globalSitesOf(t, r, "rs"); filled != 2 || current != 2 {
				t.Errorf("rs: %d sites filled, %d current, want its site on the lexical binding", filled, current)
			}

			// A name declared before the global object has it, and a
			// shadowed one the global object changes: the lexical
			// bindings', whatever the global object's are.
			if got := run(`let q = 1; globalThis.q = 2; globalThis.sh = 30; '' + [rq(), rq(), rs(), rs(), q]`); got != "1,1,20,20,1" {
				t.Errorf("after the global object's bindings changed: %s, want 1,1,20,20,1", got)
			}
		})
	}
}
