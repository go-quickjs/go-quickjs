//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"strings"
	"testing"
)

// jitSpreadProgram exercises the operations Go makes from native code
// (jitcompile.GoOperation) in loops, which compile for Go's entry, and in
// callees without loops, which compile for native callers: object spread
// and rest, array spread, spread calls, a literal's computed keys, its
// __proto__ and the fields after them, tagged templates and a named
// function expression's own name -- of sources that run no script, and of
// those that do, a getter, a proxy's traps, an iterator, a key's toString,
// each logging, and some throwing, in the order the interpreter keeps.
const jitSpreadProgram = `
	var log = [];
	function note(x) { log.push(String(x)); return x }
	function show(v) { return typeof v === 'object' && v !== null ? JSON.stringify(v) + '<' + Object.keys(v).join() + '>' : String(v) }
	function spreadObj(n, o) {
		let s = '';
		for (let i = 0; i < n; i++) {
			const c = { a: 0, ...o, k: i };
			s += show(c) + ';';
		}
		return s;
	}
	function spreadEach(list) {
		let s = '';
		for (const o of list) s += show({ ...o, last: 1 }) + show({ x: 0, ...o }) + ';';
		return s;
	}
	function spreadTwice(n, o, p) {
		let s = '';
		for (let i = 0; i < n; i++) s += show({ ...o, i, ...p, z: i }) + ';';
		return s;
	}
	function spreadArr(n, a, b) {
		let s = '';
		for (let i = 0; i < n; i++) s += show([i, ...a, 'x', ...b]) + ';';
		return s;
	}
	function spreadCall(n, f, a) {
		let s = '';
		for (let i = 0; i < n; i++) s += f(i, ...a) + ';';
		return s;
	}
	function spreadMethod(n, o, a) {
		let s = '';
		for (let i = 0; i < n; i++) s += o.m(...a, i) + ';';
		return s;
	}
	function spreadNew(n, F, a) {
		let s = '';
		for (let i = 0; i < n; i++) s += show(new F(...a, i)) + ';';
		return s;
	}
	function rest(n, o) {
		let s = '';
		for (let i = 0; i < n; i++) {
			const { a, ['b']: b, ...r } = o;
			s += a + '/' + b + '/' + show(r) + ';';
		}
		return s;
	}
	function computed(n, k) {
		let s = '';
		for (let i = 0; i < n; i++) s += show({ [k + i % 3]: i, [i % 2]: k, x: 1, [k]: 2 }) + ';';
		return s;
	}
	function proto(n, p) {
		let s = '';
		for (let i = 0; i < n; i++) {
			const o = { __proto__: p, own: i };
			s += (Object.getPrototypeOf(o) === p) + ':' + o.own + ':' + (o.inherited) + ';';
		}
		return s;
	}
	var lastStrings;
	function tag(strings, ...vals) { const same = strings === lastStrings; lastStrings = strings; return strings.raw.join('|') + vals.join(',') + same + Object.isFrozen(strings) }
	function tagged(n) {
		let s = '';
		for (let i = 0; i < n; i++) s += tag` + "`a${i}b\\n${'c'}`" + ` + ';';
		return s;
	}
	var named = function self(n) {
		let s = 0;
		for (let i = 0; i < n; i++) s += self === named ? 1 : 100;
		return s;
	};
	function callNamed(n) { let s = 0; for (let i = 0; i < n; i++) s += named(2); return s }
	// Callees without loops, compiled for native callers.
	function keyed(k, v) { return { [k]: v, w: 1 } }
	function spreadIn(o) { return { ...o, w: 1 } }
	function restIn(o) { const { a, ...r } = o; return r }
	function arrIn(a) { return [0, ...a] }
	function protoIn(p) { return { __proto__: p, w: 1 } }
	function callees(n, k, o, a, p) {
		let s = '';
		for (let i = 0; i < n; i++) s += show(keyed(k, i)) + show(spreadIn(o)) + show(restIn(o)) + show(arrIn(a)) + (Object.getPrototypeOf(protoIn(p)) === p) + ';';
		return s;
	}
	function attempt(f) { try { return f() } catch (e) { return 'caught ' + e.constructor.name + ': ' + e.message } }
	const getter = { a: 1, get g() { note('g'); return 'G' }, h: 2 };
	const hidden = Object.defineProperty({ v: 1 }, 'hidden', { value: 2, enumerable: false });
	const sym = Symbol('s');
	const withSym = { [sym]: 1, s: 2 };
	const proxy = new Proxy({ p: 1 }, {
		ownKeys() { note('ownKeys'); return ['p', 'q'] },
		getOwnPropertyDescriptor(t, k) { note('gopd ' + k); return { value: k, enumerable: true, configurable: true } },
		get(t, k) { note('get ' + String(k)); return k + '!' },
	});
	const indexed = { b: 2, 1: 'one', 0: 'zero', a: 1 };
	const holey = [1, , 3];
	const iterated = [1, 2]; iterated[Symbol.iterator] = function* () { note('it'); yield 9; yield 8 };
	const keyObj = { toString() { note('ts'); return 'T' } };
	const out = [
		spreadObj(12, { b: 1, c: 2 }), spreadObj(6, { b: 1, a: 9 }), spreadObj(4, [7, 8]), spreadObj(4, 'hi'),
		spreadObj(3, null), spreadObj(3, undefined), spreadObj(3, 3), spreadObj(4, getter), spreadObj(4, hidden),
		spreadObj(4, withSym), spreadObj(3, proxy), spreadObj(4, indexed), spreadObj(4, holey),
		attempt(() => spreadObj(3, { get bad() { throw new Error('boom') } })),
		spreadEach([{ a: 1, b: 2 }, { a: 3, b: 4 }, { b: 5, a: 6 }, { a: 7, b: 8, c: 9 }, { a: 10, b: 11 }, { x: 12, a: 13 }]),
		spreadEach([{ 1: 'a', b: 1 }, { 1: 'c', b: 2 }, Object.freeze({ 1: 'd', b: 3 }), { 0: 'e', b: 4 }]),
		spreadTwice(6, { a: 1 }, { b: 2, a: 3 }), spreadTwice(4, getter, indexed),
		spreadArr(12, [1, 2], [3]), spreadArr(4, 'ab', new Set([4, 5])), spreadArr(4, holey, []),
		spreadArr(4, iterated, new Map([[1, 2]])), attempt(() => spreadArr(3, [1], 7)),
		spreadCall(12, (a, b, c) => a + b + (c | 0), [1, 2]), spreadCall(4, Math.max, [3, 9, 4]),
		spreadCall(4, function () { return arguments.length }, 'abc'), spreadCall(4, (...xs) => xs.join('+'), iterated),
		attempt(() => spreadCall(3, 5, [1])), attempt(() => spreadCall(3, () => { throw new RangeError('in call') }, [])),
		spreadMethod(12, { m(a, b) { return a * 10 + b + (this.k | 0) }, k: 5 }, [1]),
		spreadNew(8, function (a, b) { this.v = a + b }, [1]), spreadNew(4, Array, [3]),
		attempt(() => spreadNew(3, () => 1, [])),
		rest(12, { a: 1, b: 2, c: 3, d: 4 }), rest(4, { b: 0, c: 1, a: 2, get z() { note('z'); return 'Z' } }),
		rest(4, [5, 6]), rest(4, 'str'), rest(3, proxy), rest(4, indexed), attempt(() => rest(2, null)),
		computed(12, 'k'), computed(4, sym.description), attempt(() => computed(3, keyObj)),
		attempt(() => computed(3, { toString() { throw new TypeError('key') } })),
		proto(12, { inherited: 'I' }), proto(4, null), proto(4, 5), proto(4, Array.prototype),
		tagged(8), callNamed(10), named(5),
		callees(12, 'k', { a: 1, b: 2 }, [1, 2], { q: 1 }), callees(4, sym, getter, iterated, null),
		attempt(() => callees(3, keyObj, proxy, 'xy', 7)),
	];
	// Array.prototype's iteration replaced, and restored: each spread
	// iterates as it is then.
	const values = Array.prototype[Symbol.iterator], arrayIter = Object.getPrototypeOf([][Symbol.iterator]()), next = arrayIter.next;
	Array.prototype[Symbol.iterator] = function* () { note('values'); yield 'v' };
	out.push(spreadArr(3, [1, 2], [3]), spreadCall(3, (...xs) => xs.join('+'), [4, 5]));
	Array.prototype[Symbol.iterator] = values;
	out.push(spreadArr(3, [1, 2], [3]));
	arrayIter.next = function () { note('next'); return { done: true } };
	out.push(spreadArr(3, [1, 2], [3]), spreadCall(3, (...xs) => xs.length, [4, 5]));
	arrayIter.next = next;
	out.push(spreadArr(3, [1, 2], [3]));
	delete Array.prototype[Symbol.iterator];
	out.push(attempt(() => spreadArr(3, [1, 2], [3])));
	Array.prototype[Symbol.iterator] = values;
	out.push(spreadArr(3, [1, 2], [3]));
	out.join('\n') + '\n' + log.join();
`

// Object spread and rest, array spread, spread calls, a literal's computed
// keys and __proto__ and the fields after them, tagged templates and a
// named function expression's own name are compiled, each an operation Go
// makes from native code (jitcompile.GoOperation): without a frame where
// that runs no script, and with the code's frame where it may -- a getter,
// a proxy's trap, an iterator, a key's toString, a callee -- in the order
// of evaluation the interpreter keeps, its exceptions caught where it
// catches them. Every answer is the interpreter's and the tree tier's,
// under each of the stress settings' exits (jitDifferential).
func TestJITSpreadOperations(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Under a memory limit, which jitDifferential sets, Go makes each with
	// the frame (jitOpFrameless); without one, without it where it can.
	st := jitDifferential(t, jitSpreadProgram)
	if st.SSAEntries == 0 {
		t.Fatal("no native code ran")
	}
	run := func(cfg Config, c jitStressConfig) (*Runtime, string) {
		r := jitRuntimeForTest(t, cfg)
		r.jitStress = c
		v, err := r.Run(compileForTest(t, jitSpreadProgram))
		if err != nil {
			t.Fatal(err)
		}
		return r, v.String().Go()
	}
	_, want := run(Config{}, jitStressConfig{})
	if !strings.Contains(want, "caught TypeError") {
		t.Fatalf("unexpected answer %q", want)
	}
	for _, c := range []jitStressConfig{{threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1}} {
		if _, got := run(Config{JIT: true}, c); got != want {
			t.Fatalf("JIT %+v: %q\ninterpreter: %q", c, got, want)
		}
	}
	// Each function ran natively, at its first call.
	r, got := run(Config{JIT: true}, jitStressConfig{})
	if got != want {
		t.Fatalf("JIT: %q\ninterpreter: %q", got, want)
	}
	for _, name := range []string{"spreadObj", "spreadEach", "spreadTwice", "spreadArr", "spreadCall", "spreadMethod", "spreadNew", "rest", "computed", "proto", "tagged", "callees"} {
		jitNativeForTest(t, r, name)
	}
	for _, name := range []string{"keyed", "spreadIn", "restIn", "arrIn", "protoIn"} {
		if e := r.jit.entryFor(jitGlobalClosureForTest(r, name).fn); e == nil || e.ssa == nil {
			t.Errorf("%s was not compiled for its native callers", name)
		}
	}
}

// What runs no script is made without a frame, and nothing leaves native
// code: a loop of object spread and rest of plain objects, of array spread
// and pushes of dense arrays, of computed keys that are strings, of
// __proto__ and the fields after them, makes no call of Go with a frame
// (goCalls) and takes no exit. A source with a getter, or one iterated, is
// copied with the frame, in the same call, and leaves neither, in a loop
// whose native work pays for them.
func TestJITSpreadFrameless(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function plain(n, o, a, k, p) {
			let s = 0;
			for (let i = 0; i < n; i++) {
				const x = { ...o, k: i }, y = [i, ...a, 0], { b, ...r } = o, z = { [k]: i, w: 1 }, q = { __proto__: p, f: i };
				s += x.k + y.length + r.c + z[k] + q.f;
			}
			return s;
		}
		function framed(n, o, a) {
			let s = 0;
			for (let i = 0; i < n; i++) {
				const x = { ...o, k: i }, y = [...a];
				s += x.g + y.length;
				// Enough native work for the calls to pay (jitRunGoProfit).
				for (let j = 0; j < 100; j++) s = (s + j) % 1000003;
			}
			return s;
		}
	`)); err != nil {
		t.Fatal(err)
	}
	run := func(src, want string) *jitEntry {
		t.Helper()
		for range 3 {
			if v, err := r.Run(compileForTest(t, "''+"+src)); err != nil || v.String().Go() != want {
				t.Fatalf("%s = %v, %v; want %s", src, v, err, want)
			}
		}
		return jitNativeForTest(t, r, src[:strings.IndexByte(src, '(')])
	}
	e := run(`plain(100, {b: 1, c: 2}, [1, 2], 'key', {})`, "15450")
	hosts, guards, goCalls := e.ssaStats.hosts, e.ssaStats.guards, e.ssaStats.goCalls
	run(`plain(1000, {b: 1, c: 2}, [1, 2], 'key', {})`, "1504500")
	if e.ssaStats.hosts != hosts || e.ssaStats.guards != guards || e.ssaStats.goCalls != goCalls {
		t.Errorf("plain: %d exits, %d guards, %d calls of Go with a frame", e.ssaStats.hosts-hosts, e.ssaStats.guards-guards, e.ssaStats.goCalls-goCalls)
	}
	e = run(`framed(100, {get g() { return 2 }}, new Set([1, 2]))`, "495400")
	hosts, guards, goCalls = e.ssaStats.hosts, e.ssaStats.guards, e.ssaStats.goCalls
	run(`framed(1000, {get g() { return 2 }}, new Set([1, 2]))`, "953988")
	if e.ssaStats.hosts != hosts || e.ssaStats.guards != guards || e.ssaStats.goCalls-goCalls != 6000 {
		t.Errorf("framed: %d exits, %d guards, %d calls of Go with a frame; want 6000", e.ssaStats.hosts-hosts, e.ssaStats.guards-guards, e.ssaStats.goCalls-goCalls)
	}
}

// A loop of object spreads Go makes with the frame -- a getter's source --
// and of little else, entered at its header by on-stack replacement, is
// judged not to pay as its calls are made (jitRunGoProfit): the code
// leaves after the call that finds so, with that call's result in its
// place (jitCallResult), and the tree tier runs the rest. Each answer is
// the interpreter's.
func TestJITSpreadFramedAtLoops(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, want := jitAnswersForTest(t, `
		var src = { a: 1, get g() { return this.a + 1 } };
		function spreads(n) { let s = 0; for (let i = 0; i < n; i++) { const o = { ...src, k: i }; s += o.g + (o.k & 1) } return s }
		function arrays(n) { let s = 0; const set = new Set([1, 2, 3]); for (let i = 0; i < n; i++) s += [i, ...set].length + (i & 1); return s }`)
	want(`spreads(200000)`, "500000")
	want(`arrays(200000)`, "900000")
	for _, name := range []string{"spreads", "arrays"} {
		if e := jitNativeForTest(t, r, name); !e.loopSlow {
			t.Errorf("%s's calls of Go judged to pay", name)
		}
	}
}

// Native code makes the common spreads itself, without a call of Go: an
// object spread of a plain object by the layouts Go made it by before
// (copyProps), the fields after it along their shapes' first transitions
// (defineNext), an array spread of a dense array that iterates as
// Array.prototype does, into room its literal's arrays learned to be made
// with (spreadElems, jitFillLiteral's room), and an element appended
// (appendElem). A loop of a thousand of each calls Go only to fill its
// literals' pools and at its polls. Each answer is the interpreter's.
func TestJITSpreadNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function objects(n, o) { let s = 0, x; for (let i = 0; i < n; i++) { x = { ...o, k: i, l: 1 }; s += x.k + x.b } return s + JSON.stringify(x) }
		function arrays(n, a) { let s = 0, x; for (let i = 0; i < n; i++) { x = [i, ...a, 7]; s += x.length + x[1] + (2 in x ? 1 : 0) } return s + JSON.stringify(x) }
		function args(n, a) { let s = 0; for (let i = 0; i < n; i++) s += Math.max(...a, i); return s }`
	want := map[string]string{
		`objects(1000, {a: 1, b: 2, c: 3})`: `501500{"a":1,"b":2,"c":3,"k":999,"l":1}`,
		`arrays(1000, [5, 6, 7, 8])`:        `12000[999,5,6,7,8,7]`,
	}
	r := jitAgreesForTest(t, setup, `objects(1000, {a: 1, b: 2, c: 3})`, `arrays(1000, [5, 6, 7, 8])`, `args(100, [1, 2])`,
		`objects(10, {a: 1, b: 2, c: 3}) + objects(10, {b: 2, a: 1})`, `arrays(10, [1, , 3]) + arrays(10, new Set([1, 2]))`)
	for src, w := range want {
		for range 2 {
			r.Run(compileForTest(t, src))
		}
		v, calls := jitCountGoCalls(t, r, "''+"+src)
		if v.String().Go() != w {
			t.Fatalf("%s = %s, want %s", src, v.String().Go(), w)
		}
		if calls > 200 {
			t.Errorf("%s: %d calls of Go", src, calls)
		}
	}
	// An array whose prototype is not Array.prototype iterates as its own
	// prototype says; a field after a spread is defined with the default
	// attributes, a property of its key defined with others on such an
	// object made after it.
	jitAgreesForTest(t, setup+`
		var ownIterProto = Object.create(Array.prototype);
		ownIterProto[Symbol.iterator] = function* () { yield 100 };
		function protoArrays(n, a, p) {
			let s = 0;
			for (let i = 0; i < n; i++) { if (i === n - 3) Object.setPrototypeOf(a, p); const x = [...a]; s += x[0] + x.length }
			return s;
		}
		function leafSpread(n, m) { for (let i = 0; i < n; i++) m.set(...[i, i * 2]); return m.size * 1000 + m.get(n - 1) }
		function swapIter(f) { Array.prototype[Symbol.iterator] = f }
		function replaced(n, a, gen, values) {
			let s = 0;
			for (let i = 0; i < n; i++) { if (i === n - 3) swapIter(gen); const x = [...a]; s += x[0] + x.length }
			swapIter(values);
			return s;
		}
		function flagged(n, o) {
			let s = '';
			for (let i = 0; i < n; i++) {
				if (i === n - 3) Object.defineProperty({ ...o }, 'k', { value: 0, enumerable: false, writable: true, configurable: true });
				s += Object.keys({ ...o, k: i }).join() + ';';
			}
			return s;
		}`, `protoArrays(100, [1, 2, 3], ownIterProto)`, `flagged(100, {a: 1, b: 2})`,
		`leafSpread(100, new Map())`, `leafSpread(100, new Map())`, `leafSpread(100, new Map())`, `leafSpread(100, new Map())`, `replaced(100, [1, 2, 3], Array.prototype[Symbol.iterator], Array.prototype[Symbol.iterator])`,
		`replaced(100, [1, 2, 3], ownIterProto[Symbol.iterator], Array.prototype[Symbol.iterator])`)
	// While the collector marks, native code makes each as it does
	// otherwise, its pointers stored with Go's write barrier (mir's
	// byMarking): as many calls of Go (internal/jit/README.md, the write
	// barrier's rule).
	t.Run("marking", func(t *testing.T) {
		flag := jitMarkingForTest(t)
		r := jitAgreesForTest(t, setup, `objects(1000, {a: 1, b: 2, c: 3})`, `arrays(1000, [5, 6, 7, 8])`, `arrays(10, [1, , 3])`)
		for src, w := range want {
			for range 2 {
				r.Run(compileForTest(t, src))
			}
			v, calls := jitCountGoCalls(t, r, "''+"+src)
			if v.String().Go() != w {
				t.Fatalf("marking: %s = %s, want %s", src, v.String().Go(), w)
			}
			if calls > 200 {
				t.Errorf("marking: %s: %d calls of Go", src, calls)
			}
		}
		*flag = 0
	})
	// With the collector itself marking as the loops run, every object
	// they made reached from what holds it.
	t.Run("collecting", func(t *testing.T) {
		r := jitAgreesForTest(t, setup, `objects(1000, {a: 1, b: 2, c: 3})`, `arrays(1000, [5, 6, 7, 8])`)
		stop := jitCollectingForTest(t)
		defer stop()
		for range 20 {
			for src, w := range want {
				v, err := r.Run(compileForTest(t, "''+"+src))
				if err != nil {
					t.Fatal(err)
				}
				if v.String().Go() != w {
					t.Fatalf("collecting: %s = %s, want %s", src, v.String().Go(), w)
				}
			}
		}
	})
}

// jitCollectingForTest has the collector collect again and again, on a
// goroutine of its own, until the function it returns is called: native
// code running meanwhile meets it marking, its stores of pointers through
// Go's write barrier, which GODEBUG=gccheckmark=1 checks.
func jitCollectingForTest(t *testing.T) func() {
	t.Helper()
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			default:
				runtime.GC()
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}
