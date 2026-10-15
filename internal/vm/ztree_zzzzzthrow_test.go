package vm

import "testing"

// A throw the tree tier takes to a handler of its own frame with the value
// as it is (throwValueToHandler) does what the interpreter's throw does: the
// catch clause binds the value, a finally clause runs and the completion
// goes on, the iterators of for...of loops between the throw and the
// handler are closed, an inner handler catches before an outer one, an
// error object keeps the stack it was made with, and what the catch clause
// throws again reaches the caller.
func TestTreeThrowToOwnHandler(t *testing.T) {
	const src = `
		const out = []
		function plain(n) { let s = 0; for (let i = 0; i < n; i++) { try { throw i } catch (e) { s += e } } return s }
		function objects() { const o = { k: 1 }; try { throw o } catch (e) { return e === o } }
		function finallyRuns() {
			const log = []
			try { try { throw 'a' } finally { log.push('finally') } } catch (e) { log.push('caught ' + e) }
			return log.join(' ')
		}
		function closes() {
			const log = []
			const it = { [Symbol.iterator]() { return { next() { return { value: 1, done: false } }, return() { log.push('closed'); return {} } } } }
			try { for (const x of it) { for (const y of it) { throw x + y } } } catch (e) { log.push('caught ' + e) }
			return log.join(' ')
		}
		function nested() {
			const log = []
			try { try { throw 'inner' } catch (e) { log.push('1:' + e); throw 'again' } } catch (e) { log.push('2:' + e) }
			return log.join(' ')
		}
		function errorStack() { try { throw new TypeError('t') } catch (e) { return e.name + ' ' + /errorStack/.test(e.stack) } }
		function rethrows() { try { throw 'x' } catch (e) { throw e + 'y' } }
		function undef() { try { throw undefined } catch (e) { return e === undefined } }
		for (let r = 0; r < 3; r++) {
			out.push(plain(10), objects(), finallyRuns(), closes(), nested(), errorStack(), undef())
			try { rethrows() } catch (e) { out.push(e) }
		}
		out.join()`
	const want = "45,true,finally caught a,closed closed caught 2,1:inner 2:again,TypeError true,true,xy"
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want+","+want+","+want {
			t.Errorf("tree %v:\n%s\nwant three of\n%s", tree, got, want)
		}
	}
}
