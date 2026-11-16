package vm

import "testing"

// A private name is a symbol, which internSymbol gives its atom from the
// symbol itself once the runtime's table has interned it. Each evaluation
// of a class makes private names of its own, spelled alike: an object of
// one class's evaluation is not a holder of the other's member, a symbol
// used as an ordinary key keeps its own property, and another runtime's
// table interns the same symbol to an atom of its own.
func TestPrivateNamesKeepTheirAtoms(t *testing.T) {
	const src = `
		function make() {
			return class { #n = 1; get() { return this.#n } set(v) { this.#n = v; return this.#n } static has(o) { return #n in o } }
		}
		const A = make(), B = make()
		const a = new A(), b = new B()
		const out = [a.get(), b.set(5), a.get(), A.has(a), A.has(b), B.has(a)]
		try { A.prototype.get.call(b) } catch (e) { out.push(e.name) }
		const s = Symbol('n'), o = { [s]: 's' }
		out.push(o[s], Object.getOwnPropertySymbols(o).length, a.get())
		out.join()`
	const want = "1,5,1,true,false,false,TypeError,s,1,1"
	r := New(Config{})
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		r.Close()
		t.Fatal(err)
	}
	if got := v.String().Go(); got != want {
		t.Errorf("%s, want %s", got, want)
	}
	// A symbol another table interns gets an atom of that table's,
	// not the one the symbol remembers.
	sym := NewSymbol("shared", true)
	mine := r.atoms.internSymbol(sym)
	other := newAtomTable()
	theirs := other.internSymbol(sym)
	if r.atoms.internSymbol(sym) != mine || other.internSymbol(sym) != theirs || other.entries[len(other.entries)-1].sym != sym {
		t.Errorf("a symbol interned in two tables: %d, %d", mine, theirs)
	}
	r.Close()
}
