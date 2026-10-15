package vm

import "testing"

// recv.name(args) on a local or this receiver is one node of the tree tier
// (methodCall in zzmethod.go), which spills nothing beneath it, and calls
// a leaf itself; and so are call and new (callOperands). It answers as the interpreter and Node
// do, each case in a function whose loop gives it a tree, called three
// times:
//   - a local a closure has captured and the call writes, and a parameter
//     a sloppy arguments object passed to the call aliases, beneath it,
//     which are read before the call; and i + p.id(i++), whose argument
//     writes the local beneath;
//   - a method a getter gives, looked up once a call;
//   - each leaf -- this.a, this.arr.length, this.arr[i], a store, a pure
//     body, one forwarding to another -- on an instance and on one whose
//     properties are getters, which leafCall gives up on;
//   - a primitive receiver, a built-in's, a bound function as a method;
//   - an undefined method, an undefined receiver, a method of a number;
//   - an optional call, a polymorphic site, recursion through this, an
//     arrow as a method;
//   - a plain call beneath which a captured local the call writes is, a
//     callee called after its own expression and before its arguments, a
//     construction, and methods of a property and of an element.
func TestTreeMethodCalls(t *testing.T) {
	const src = `
		const out = []
		class P {
			constructor(a) { this.a = a; this.arr = [10, 20, 30]; this.s = 'str' }
			get1() { return this.a }
			len() { return this.arr.length }
			at(i) { return this.arr[i] }
			set(v) { this.a = v }
			sum(x, y) { return this.a + x + y }
			fwd(x) { return this.sum(x, 1) }
			id(x) { return x }
			fib(n) { return n < 2 ? n : this.fib(n - 1) + this.fib(n - 2) }
			twice(x) { let t = 0; for (let i = 0; i < 2; i++) t += this.sum(x, i); return t }
		}
		function captured(o) {
			let s = 1
			const q = { f() { s += 10; return 2 } }
			const r = []
			for (let i = 0; i < 3; i++) r.push(s + q.f())
			return r.join('/') + ':' + s
		}
		function mapped(a) {
			const args = arguments
			const o = { f(x) { x[0] = 100; return 1 } }
			let t = 0
			for (let i = 0; i < 3; i++) { a = i; t += a + o.f(args) }
			return t + ':' + a
		}
		function updates(p) {
			let t = ''
			for (let i = 0; i < 3;) t += (i + p.id(i++)) + ','
			return t
		}
		function getters() {
			let n = 0
			const o = { v: 7, get f() { n++; return function (x) { return this.v * x } } }
			let t = 0
			for (let i = 0; i < 4; i++) t += o.f(n)
			return t + ':' + n
		}
		function leaves(p, g) {
			const r = []
			for (let i = 0; i < 3; i++) {
				r.push([p.get1(), p.len(), p.at(i), p.sum(i, 2), p.fwd(i), g.get1(), g.len(), g.at(i), g.sum(i, 1)].join(' '))
				p.set(p.a + 1)
			}
			return r.join(';')
		}
		function prims() {
			const str = 'abc', n = 2.5, M = Math, F = function (x) { return this.k + x }.bind({ k: 3 }), h = { F }
			let t = ''
			for (let i = 0; i < 3; i++) t += str.charAt(i) + n.toFixed(i) + M.max(i, 1) + h.F(i) + '|'
			return t
		}
		function errs(p) {
			const r = []
			let u
			for (let i = 0; i < 2; i++) {
				try { p.nope() } catch (e) { r.push(e.name + ': ' + e.message) }
				try { u.f() } catch (e) { r.push(e.name + ': ' + e.message) }
				try { p.a.f() } catch (e) { r.push(e.name + ': ' + e.message) }
			}
			return r.join(' / ')
		}
		function optional(p) {
			let t = ''
			for (let i = 0; i < 3; i++) t += p.get1?.() + ':' + p.none?.() + ','
			return t
		}
		function poly(objs) {
			let t = 0
			for (let i = 0; i < 6; i++) { const o = objs[i % 3]; t = t * 3 + o.m(i) }
			return t
		}
		function self(p) { let t = 0; for (let i = 0; i < 3; i++) t += p.fib(10) + p.twice(i); return t }
		function arrows() {
			const o = { f: () => typeof this, g(x) { return x } }
			let t = ''
			for (let i = 0; i < 2; i++) t += o.f() + o.g(i)
			return t
		}
		function plains() {
			let s = 1, log = ''
			const f = () => { s += 10; return 2 }
			const pick = k => { log += 'c' + k; return x => x * k }
			const arg = v => { log += 'a' + v; return v }
			function C(x) { this.x = x }
			const o = { in: { m(x) { return this === o.in ? x + 1 : -1 } } }, k = 'm', arr = [o.in]
			let t = 0
			for (let i = 0; i < 3;) {
				t += s + f()
				t += pick(i)(arg(i))
				t += new C(i++).x
				t += o.in.m(i) + o.in[k](i) + arr[0].m(i)
			}
			return t + ':' + s + ':' + log
		}
		const g = new P(5)
		Object.defineProperty(g, 'a', { get() { return 50 } })
		Object.defineProperty(g, 'arr', { get() { return [1, 2, 3, 4] } })
		for (let k = 0; k < 3; k++) {
			out.push(captured({}), mapped(0), updates(new P(0)), getters(), leaves(new P(1), g), prims(), errs(new P(1)),
				optional(new P(4)), poly([{ m: x => x }, { m(x) { return 2 * x } }, { m() { return 1 } }]), self(new P(2)), arrows(), plains())
		}
		out.slice(0, 12).join('\n') + '\n' + (out.slice(0, 12).join() === out.slice(12, 24).join() && out.slice(12, 24).join() === out.slice(24).join())`
	const want = "3/13/23:31\n" +
		"6:100\n" +
		"0,2,4,\n" +
		"70:4\n" +
		"1 3 10 3 2 50 4 1 51;2 3 20 5 4 50 4 2 52;3 3 30 7 6 50 4 3 53\n" +
		"a313|b2.514|c2.5025|\n" +
		"TypeError: undefined is not a function / TypeError: cannot read property \"f\" of undefined / TypeError: undefined is not a function / TypeError: undefined is not a function / TypeError: cannot read property \"f\" of undefined / TypeError: undefined is not a function\n" +
		"4:undefined,4:undefined,4:undefined,\n" +
		"241\n" +
		"186\n" +
		"object0object1\n" +
		"74:31:c0a0c1a1c2a2\n" +
		"true"
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
