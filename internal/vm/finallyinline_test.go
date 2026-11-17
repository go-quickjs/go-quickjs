package vm

import "testing"

// A finally clause is compiled inline after its try's normal completion,
// and again as the handler's, which a throw and a return routed through it
// enter with their completion record (compileTryFinally). Both answer as
// Node does: a clause after a normal completion, a throw and a return; a
// return in the clause overriding the try's; continue and break in the try
// and in the clause, of a loop and of a labelled outer one; finally
// clauses nested in a clause, and in each other; a throw from the clause;
// a generator yielding in its clause, run to its end and returned from; a
// for-of's iterator closed by a break before the clause runs; closures of
// a block's bindings in the try and in the clause; and the try's value
// returned before the clause changes what it was read from.
func TestFinallyInline(t *testing.T) {
	t.Parallel()
	src := `
		var log = []
		function a(x) { try { log.push('t' + x) } finally { log.push('f' + x) } return 'a' }
		function b(x) { try { if (x) throw new Error('e' + x); return 'r' } finally { log.push('fb') } }
		function c(x) { try { return 'try' } finally { if (x) return 'fin' } }
		function d(n) { var s = 0; for (var i = 0; i < n; i++) { try { if (i === 2) continue; if (i === 4) break; s += i } finally { s += 100 } } return s }
		function e(n) { var s = ''; outer: for (var i = 0; i < n; i++) { for (var j = 0; j < n; j++) { try { s += i + '' + j } finally { if (j === 1) continue outer; if (i === 2) break outer } } } return s }
		function f() { try { try { log.push('in') } finally { log.push('f1'); try { log.push('f2') } finally { log.push('f3') } } } finally { log.push('f4') } return 'f' }
		function g(x) { try { log.push('g') } finally { if (x) throw new TypeError('from finally') } return 'g' }
		function* h() { try { yield 1; yield 2 } finally { yield 3; log.push('hf') } }
		function k() { var r = []; for (var x of [1, 2, 3]) { try { if (x === 2) break; r.push(x) } finally { r.push('k' + x) } } return r.join('') }
		function m() { var fs = []; for (let i = 0; i < 3; i++) { try { fs.push(() => i) } finally { let j = i * 10; fs.push(() => j) } } return fs.map(f => f()).join() }
		function p(o) { try { return o.v } finally { o.v = 'changed' } }
		var out = []
		out.push(a(1), b(0), c(0), c(1), d(10), e(4), f(), g(0), k(), m(), p({ v: 'orig' }))
		try { b(1) } catch (err) { out.push(err.message) }
		try { g(1) } catch (err) { out.push(err.constructor.name + err.message) }
		out.push([...h()].join(''))
		var it = h(); it.next(); out.push(JSON.stringify(it.return(9)), JSON.stringify(it.next()), JSON.stringify(it.next()))
		out.push(log.join(','))
		var s = out.join('|')
		s`
	const want = `a|r|try|fin|504|0001101120|f|g|1k1k2|0,0,1,10,2,20|orig|e1|TypeErrorfrom finally|123|{"value":3,"done":false}|{"value":9,"done":true}|{"done":true}|t1,f1,fb,in,f1,f2,f3,f4,g,fb,g,hf,hf`
	r := New(Config{})
	v, err := r.Run(compileForTest(t, src))
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// A try statement's value is its try block's, whichever copy of the clause
// runs, unless the clause breaks out, when it is what the clause produced:
// as Node's eval gives them.
func TestFinallyInlineCompletion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ src, want string }{
		{"try { 1 } finally { 2 }", "1"},
		{"do { try { 3 } finally { break } } while (0)", "undefined"},
		{"do { try { 3 } finally { 6; break } } while (0)", "6"},
		{"l: try { 4 } finally { 5 }", "4"},
		{"var q = 0; while (q < 2) { q++; try { q * 10 } finally { q + 100 } }", "20"},
		{"try { throw 1 } catch (e) { 7 } finally { 8 }", "7"},
		{"try { } finally { 9 }", "undefined"},
	} {
		r := New(Config{})
		v, err := r.Run(compileForTest(t, tc.src))
		if err != nil {
			t.Fatal(err)
		}
		str, err := r.ToString(v)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := str.Go(); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.src, got, tc.want)
		}
	}
}
