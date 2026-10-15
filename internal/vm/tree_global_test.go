package vm

import "testing"

// A global read the tree tier caches, the global object's layout remembered
// (noteGlobalShape), answers as the interpreter does through every change
// the property may go through after: a new value, an accessor, a read-only
// value, a delete, the property again, a script's lexical binding that
// shadows it.
func TestTreeGlobalReadFollowsItsProperty(t *testing.T) {
	const first = `
		var out = []
		globalThis.h = 5
		function readH() { try { return h } catch (e) { return e.constructor.name } }
		function warm() { for (let i = 0; i < 50; i++) readH() }
		warm(); out.push(readH())
		h = 6; warm(); out.push(readH())
		Object.defineProperty(globalThis, 'h', { get() { return 'getter' }, configurable: true })
		warm(); out.push(readH())
		Object.defineProperty(globalThis, 'h', { value: 7, writable: false, configurable: true })
		warm(); out.push(readH())
		delete globalThis.h
		warm(); out.push(readH())
		globalThis.h = 9
		warm(); out.push(readH())
		for (let i = 0; i < 200; i++) globalThis['other' + i] = i
		warm(); out.push(readH())
		out.join()`
	const second = `let h = 'lexical'; warm(); out.push(readH()); out.join()`
	const want1 = "5,6,getter,7,ReferenceError,9,9"
	const want2 = want1 + ",lexical"
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		got1, err := r.Run(compileForTest(t, first))
		if err != nil {
			t.Fatal(err)
		}
		got2, err := r.Run(compileForTest(t, second))
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatal(err)
		}
		if s := got1.String().Go(); s != want1 {
			t.Errorf("tree %v: %s, want %s", tree, s, want1)
		}
		if s := got2.String().Go(); s != want2 {
			t.Errorf("tree %v, after a lexical binding: %s, want %s", tree, s, want2)
		}
	}
}
