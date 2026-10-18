package quickjs_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// The opt-in is valid in ordinary builds too, where it falls back. Pin its
// independence from code-generation policy and Node compatibility options.
func TestJITOption(t *testing.T) {
	for _, node := range []bool{false, true} {
		opts := []quickjs.Option{quickjs.WithJIT(), quickjs.WithoutCodeGeneration()}
		if node {
			opts = append(opts, quickjs.WithNodeQuirks())
		}
		r := quickjs.New(opts...)
		// A few calls warm a function up, as these tests run it.
		hostaccess.VM(r).SetJITWarmUp(8, 8)
		defer r.Close()
		for _, tc := range []struct{ source, want string }{
			{`function f(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s } f(10000)`, "49995000"},
			{`let count=0; let x=f({valueOf() { count++; return 3 }}); x+":"+count`, "3:4"},
			{`typeof eval`, "undefined"},
			{`function z(n) { let a=0; for(let i=0;i<n;i++) a=-a; return a } Object.is(z(1),-0)`, "true"},
		} {
			if got := evalString(t, r, tc.source); got != tc.want {
				t.Fatalf("node=%v: %s = %s, want %s", node, tc.source, got, tc.want)
			}
		}
		if _, err := r.Eval(`Function("return 1")()`); err == nil {
			t.Fatal("JIT enabled blocked Function constructor")
		}
	}
}

// jitHotFunctions is a program with n hot functions, each run in turn, and
// its answer.
func jitHotFunctions(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "function f%d(n){let t=%d;for(let i=0;i<n;i++)t=(t*31+i+%d)|0;return t}\n", i, i, i)
	}
	b.WriteString("var fs=[")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "f%d,", i)
	}
	b.WriteString("];let t=0;for(let r=0;r<20;r++)for(const f of fs)t=(t+f(200))|0;t")
	return b.String()
}

// WithJITMemoryLimit bounds what the JIT holds, whatever the program: with
// more hot functions than fit, it holds no more than the limit, compiles
// what fits, and the answers are the interpreter's. It is apart from
// WithMemoryLimit, which it overrides the eighth of, and a script's
// ErrMemoryLimit is the same with the JIT on and off. A runtime without the
// JIT, or a build without it, takes the option and does nothing with it.
func TestJITMemoryLimitOption(t *testing.T) {
	src := jitHotFunctions(300)
	plain := quickjs.New()
	defer plain.Close()
	want := evalString(t, plain, src)
	for _, tc := range []struct {
		name  string
		opts  []quickjs.Option
		limit int64
	}{
		{"jit 256K", []quickjs.Option{quickjs.WithJIT(), quickjs.WithJITMemoryLimit(256 << 10)}, 256 << 10},
		{"jit 1M", []quickjs.Option{quickjs.WithJIT(), quickjs.WithJITMemoryLimit(1 << 20)}, 1 << 20},
		{"jit 4M over a memory limit", []quickjs.Option{quickjs.WithJIT(), quickjs.WithMemoryLimit(64 << 20), quickjs.WithJITMemoryLimit(4 << 20)}, 4 << 20},
		{"jit default", []quickjs.Option{quickjs.WithJIT(), quickjs.WithJITMemoryLimit(0)}, 8 << 20},
		{"no jit", []quickjs.Option{quickjs.WithJITMemoryLimit(256 << 10)}, 0},
	} {
		r := quickjs.New(tc.opts...)
		// A few calls warm a function up, as these tests run it.
		hostaccess.VM(r).SetJITWarmUp(8, 8)
		if got := evalString(t, r, src); got != want {
			t.Fatalf("%s: %s, want %s", tc.name, got, want)
		}
		st := hostaccess.VM(r).JITStats()
		t.Logf("%s: %+v", tc.name, st)
		r.Close()
		if st.Budget == 0 {
			// No JIT: in this build, or this runtime.
			if st.Compiled != 0 || st.Held != 0 {
				t.Fatalf("%s: the JIT ran: %+v", tc.name, st)
			}
			continue
		}
		if tc.limit == 0 {
			t.Fatalf("%s: a JIT budget without the JIT: %+v", tc.name, st)
		}
		if st.Budget != tc.limit || st.Held > st.Budget || st.Compiled == 0 {
			t.Fatalf("%s: %+v", tc.name, st)
		}
		if tc.limit == 256<<10 && st.Refused+st.Evicted == 0 {
			t.Fatalf("%s: the program fit: %+v", tc.name, st)
		}
	}
	// A script's memory limit is the same with the JIT on and off, whatever
	// the JIT's.
	for _, opts := range [][]quickjs.Option{nil, {quickjs.WithJIT(), quickjs.WithJITMemoryLimit(64 << 20)}} {
		r := quickjs.New(append(opts, quickjs.WithMemoryLimit(1<<20))...)
		// A few calls warm a function up, as these tests run it.
		hostaccess.VM(r).SetJITWarmUp(8, 8)
		_, err := r.Eval(`new ArrayBuffer(2 << 20)`)
		r.Close()
		if !errors.Is(err, quickjs.ErrMemoryLimit) {
			t.Fatalf("over a script's limit, with %d options: %v", len(opts), err)
		}
	}
}

// import() and import.meta in functions the JIT compiles give what they
// give in the interpreter: modules loaded by a loop, with the referrer each
// is resolved against, import attributes passed on, a specifier whose
// toString throws rejecting rather than throwing, and the module's
// import.meta read in a loop.
func TestJITImport(t *testing.T) {
	run := func(opts ...quickjs.Option) (string, vm.JITStats) {
		rt := quickjs.New(opts...)
		// A few calls warm a function up, as these tests run it.
		hostaccess.VM(rt).SetJITWarmUp(8, 8)
		defer rt.Close()
		var got []string
		rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
			got = append(got, specifier+" from "+referrer)
			if strings.HasSuffix(specifier, ".json") {
				return `{"v": 7}`, "/app/" + specifier, nil
			}
			return `export const name = "` + specifier + `";`, "/app/" + specifier, nil
		})
		ns, err := rt.EvalModule("/app/main.mjs", `
			// No closures: each runs natively (what the JIT does not compile
			// yet is left outside).
			function load(n, ps) {
				for (let i = 0; i < n; i++) ps.push(import("./m" + (i % 4) + ".mjs"));
				return ps;
			}
			function json(n, ps, opts) {
				for (let i = 0; i < n; i++) ps.push(import("./d" + (i % 2) + ".json", opts));
				return ps;
			}
			function bad(n, ps, spec) {
				for (let i = 0; i < n; i++) ps.push(import(spec));
				return ps;
			}
			function meta(n) {
				let same = 0;
				for (let i = 0; i < n; i++) if (import.meta === import.meta && typeof import.meta === "object") same++;
				return same;
			}
			const spec = { toString() { throw "S" } }, opts = { with: { type: "json" } };
			const rounds = [];
			for (let k = 0; k < 20; k++) rounds.push(Promise.all([
				Promise.all(load(5, [])).then(ms => ms.map(m => m.name).join()),
				Promise.all(json(2, [], opts)).then(ms => ms.map(m => m.default.v).join()),
				Promise.allSettled(bad(2, [], spec)).then(r => r.map(x => x.status + x.reason).join()),
				meta(30)]).then(r => r.join(" ")));
			export const all = Promise.all(rounds).then(r => r[0] + " / " + r[19]);`)
		if err != nil {
			t.Fatal(err)
		}
		all, _ := ns.Get("all")
		v, err := all.Await(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return v.String() + " | " + strings.Join(got, ", "), hostaccess.VM(rt).JITStats()
	}
	want, _ := run()
	got, st := run(quickjs.WithJIT())
	if got != want {
		t.Errorf("jit: %s\ninterpreter: %s", got, want)
	}
	if st.Budget != 0 && (st.Compiled < 4 || st.SSAEntries == 0) {
		// load, json, bad and meta among those compiled.
		t.Errorf("the functions did not run natively: %+v", st)
	}
}

// ?? and ?. test a value for null and undefined natively, nothing
// converted -- not 0, "", false or NaN, and no object -- keeping it on
// either path.
func TestJITNullish(t *testing.T) {
	cases := []struct{ name, src string }{
		{"??", `function f(a) { let s = ""; for (let i = 0; i < a.length; i++) s += (a[i] ?? "d") + ","; return s }
			const a = [null, undefined, 0, "", false, NaN, "x", {}, [], 7]; let r = ""; for (let k = 0; k < 300; k++) r = f(a); r`},
		{"?.", `function f(a) { let n = 0; for (let i = 0; i < a.length; i++) { const o = a[i]; n += (o?.x ?? 1) + (o?.f?.() ?? 2) + (o?.["y"]?.z ?? 3) } return n }
			const a = [null, undefined, { x: 10 }, { f() { return 20 } }, { y: { z: 30 } }, { y: null }, 0]; let r = 0; for (let k = 0; k < 300; k++) r += f(a); r`},
		{"numbers", `function f(n) { let s = 0; for (let i = 0; i < n; i++) { const v = i % 3 ? i : null; s += v ?? -1 } return s }
			let r = 0; for (let k = 0; k < 100; k++) r += f(1000); r`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(opts ...quickjs.Option) (string, vm.JITStats) {
				r := quickjs.New(opts...)
				// A few calls warm a function up, as these tests run it.
				hostaccess.VM(r).SetJITWarmUp(8, 8)
				defer r.Close()
				v, err := r.Eval(tc.src)
				if err != nil {
					t.Fatal(err)
				}
				return v.String(), hostaccess.VM(r).JITStats()
			}
			want, _ := run()
			got, st := run(quickjs.WithJIT())
			if got != want {
				t.Fatalf("jit: %s, interpreter: %s", got, want)
			}
			if st.Budget != 0 && st.SSAEntries == 0 {
				t.Errorf("nothing ran natively: %+v", st)
			}
		})
	}
}

// Destructuring from an iterator compiles: its cursor made by a call of Go,
// each step natively over an array walked in place and by Go otherwise --
// undefined once done, the iterator's next called no more -- and its end
// closing the iterator natively where there is nothing to close, by Go
// where its return method must be called.
func TestJITDestructuring(t *testing.T) {
	cases := []struct{ name, src string }{
		{"arrays", `function f(a) { let s = 0; for (let i = 0; i < a.length; i++) { const [x, y] = a[i]; s += x + y } return s }
			const a = []; for (let i = 0; i < 500; i++) a.push([i, i * 2]); let r = 0; for (let k = 0; k < 50; k++) r += f(a); r`},
		{"holes, defaults, rest", `function f(a) { let s = ""; for (let i = 0; i < a.length; i++) { const [x, y = "d", ...rest] = a[i]; s += x + y + rest.length + "," } return s }
			const a = [[1], [1, 2, 3, 4], [, 5], [], [undefined, null]]; let r = ""; for (let k = 0; k < 500; k++) r = f(a); r`},
		{"iterables", `function f(m, set, str) { let s = 0; for (const [k, v] of m) s += k * v; const [[a, b], [c]] = m; const [p, q] = set; const [c0, c1] = str; return s + a + b + c + p + q + c0 + c1 }
			const m = new Map(), set = new Set([3, 4, 5]); for (let i = 0; i < 50; i++) m.set(i, i + 1); let r = ""; for (let k = 0; k < 300; k++) r = f(m, set, "xy"); r`},
		{"generators", `function* g(n) { for (let i = 0; i < n; i++) yield i } function f(n) { let s = 0; for (let i = 0; i < n; i++) { const [x, y, z] = g(i % 5); s += (x ?? 7) + (y ?? 11) + (z ?? 13) } return s }
			let r = 0; for (let k = 0; k < 50; k++) r += f(300); r`},
		{"assignment targets", `function f(o, a) { let s = 0; for (let i = 0; i < a.length; i++) { [o.x, o["y"]] = a[i]; s += o.x - o.y } return s }
			const a = []; for (let i = 0; i < 300; i++) a.push([i, 1]); let r = 0; for (let k = 0; k < 50; k++) r += f({}, a); r`},
		{"closing", `let closed = 0, nexts = 0; function it(n) { let i = 0; return { [Symbol.iterator]() { return this }, next() { nexts++; return { value: i++, done: i > n } }, return() { closed++; return {} } } }
			function f(n) { let s = 0; for (let i = 0; i < n; i++) { const [a = 0, b = 0] = it(i % 4); s += a + b } return s }
			let r = 0; for (let k = 0; k < 50; k++) r += f(200); r + ":" + closed + ":" + nexts`},
		{"throws", `function bad() { return { [Symbol.iterator]() { return this }, next() { throw new RangeError("n") }, return() { throw new Error("never") } } }
			function f(n) { let c = 0; for (let i = 0; i < n; i++) { try { const [a] = i % 3 ? [i] : bad(); c += a } catch (e) { c += e instanceof RangeError ? 100 : -1 } } return c }
			let r = 0; for (let k = 0; k < 50; k++) r += f(300); r`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(opts ...quickjs.Option) (string, vm.JITStats) {
				r := quickjs.New(opts...)
				// A few calls warm a function up, as these tests run it.
				hostaccess.VM(r).SetJITWarmUp(8, 8)
				defer r.Close()
				v, err := r.Eval(tc.src)
				if err != nil {
					t.Fatal(err)
				}
				return v.String(), hostaccess.VM(r).JITStats()
			}
			want, _ := run()
			got, st := run(quickjs.WithJIT())
			if got != want {
				t.Fatalf("jit: %s, interpreter: %s", got, want)
			}
			if st.Budget != 0 && st.SSAEntries == 0 {
				t.Errorf("nothing ran natively: %+v", st)
			}
		})
	}
}

// A private member is read and written natively as a property of its key,
// a symbol each evaluation of its class mints, in objects of the shapes the
// site met; the code checks the key its binding holds is the one it was
// compiled for, since two evaluations of a class share their methods' code.
// #x in o is true natively where the read would find the member. Go makes
// the rest: a class's members, accessors, a key in a local.
func TestJITPrivateMembers(t *testing.T) {
	cases := []struct{ name, src string }{
		{"fields and methods", `class C { #x = 1; #y = 0.5; #m() { return this.#x * 2 } run(n) { let s = 0; for (let i = 0; i < n; i++) { this.#x = i; this.#y += 1; s += this.#m() + (#x in this ? 1 : 0) } return s + this.#y } }
			const c = new C(); let r = 0; for (let k = 0; k < 50; k++) r += c.run(2000); r`},
		{"two evaluations of a class", `function make(v) { return class { #x = v; static get(o) { let s = 0; for (let i = 0; i < 100; i++) s += o.#x; return s } } }
			const A = make(1), B = make(2), a = new A(), b = new B(); let r = "";
			for (let k = 0; k < 200; k++) r = A.get(a) + ":" + B.get(b);
			for (let k = 0; k < 3; k++) { try { A.get(b) } catch (e) { r += ":" + e.constructor.name } try { B.get(a) } catch (e) { r += ":" + e.constructor.name } } r`},
		{"#x in o", `class K { #k; static has(o) { let n = 0; for (let i = 0; i < 100; i++) if (#k in o) n++; return n } }
			const objs = [new K(), {}, new K(), Object.create(K.prototype)]; let r = 0; for (let k = 0; k < 400; k++) r += K.has(objs[k % 4]); r`},
		{"accessors, statics and errors", `class G { #v = 3; static #count = 0; get #twice() { return this.#v * 2 } set #twice(x) { this.#v = x / 2 } #f() {}
			step() { let s = 0; for (let i = 0; i < 100; i++) { this.#twice = i * 2; s += this.#twice; G.#count++ } return s }
			static count() { return G.#count } bad() { try { this.#f = 1 } catch (e) { return e.constructor.name } } }
			const g = new G(); let r = 0; for (let k = 0; k < 50; k++) r += g.step(); r + ":" + G.count() + ":" + g.bad()`},
		{"shapes", `class Q { #p = 0; constructor(extra) { if (extra) this.e = 1 } bump(n) { for (let i = 0; i < n; i++) this.#p += i; return this.#p } }
			const qs = [new Q(false), new Q(true)]; let r = 0; for (let k = 0; k < 200; k++) r += qs[k & 1].bump(100); r`},
		{"strings", `class S { #s = ""; grow(n) { for (let i = 0; i < n; i++) this.#s += i % 10; return this.#s.length } } let r = 0; for (let k = 0; k < 50; k++) r += new S().grow(500); r`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(opts ...quickjs.Option) (string, vm.JITStats) {
				r := quickjs.New(opts...)
				// A few calls warm a function up, as these tests run it.
				hostaccess.VM(r).SetJITWarmUp(8, 8)
				defer r.Close()
				v, err := r.Eval(tc.src)
				if err != nil {
					t.Fatal(err)
				}
				return v.String(), hostaccess.VM(r).JITStats()
			}
			want, _ := run()
			got, st := run(quickjs.WithJIT())
			if got != want {
				t.Fatalf("jit: %s, interpreter: %s", got, want)
			}
			if st.Budget != 0 && st.SSAEntries == 0 {
				t.Errorf("nothing ran natively: %+v", st)
			}
		})
	}
}

// A call in tail position is an ordinary call where the stack is shallow
// and gives its frame up deeper, as the interpreter's does (tailCallDepth):
// native code makes a call of the function itself a jump to its start, and,
// in a native callee's frame, one of another function a jump to that
// function's code in this frame's place. Every answer, a stack trace's
// too, is the interpreter's, and deep recursion in tail position runs in
// constant space.
func TestJITTailCalls(t *testing.T) {
	cases := []struct{ name, src string }{
		{"self", `function loop(n, acc) { if (n === 0) return acc; return loop(n - 1, acc + n) }
			let r = 0; for (let k = 0; k < 20; k++) r += loop(100000, k); r`},
		{"fewer and more arguments", `function few(n, x, y) { if (n === 0) return typeof x + typeof y; return few(n - 1, n) }
			function more(n) { if (n === 0) return "done"; return more(n - 1, n, n) }
			let r = ""; for (let k = 0; k < 20; k++) r = few(1000 + k) + more(1000 + k); r`},
		{"a method reading this", `const o = { n: 0, step(k) { if (k === 0) return this.n; this.n++; return this.step(k - 1) } };
			let r = 0; for (let k = 0; k < 20; k++) { o.n = 0; r += o.step(50000) } r`},
		{"mutual", `function even(n) { return n === 0 ? true : odd(n - 1) } function odd(n) { return n === 0 ? false : even(n - 1) }
			let r = 0; for (let k = 0; k < 20; k++) r += even(50000 + k) ? 1 : 0; r`},
		{"stack traces", `function depth(n, f) { if (n === 0) return f(); return depth(n - 1, f) }
			function other(n, f) { if (n === 0) return f(); return depth(n - 1, f) }
			let r = ""; for (let k = 0; k < 30; k++) r = [depth(10, () => new Error("x").stack), depth(100, () => new Error("y").stack), other(100, () => new Error("z").stack)].join("|"); r`},
		{"a construction", `function C(n) { if (n === 0) { this.v = 1; return } return C.call(this, n - 1) }
			let r = 0; for (let k = 0; k < 30; k++) r += new C(40 + k).v; r`},
		{"an arrow and closures made anew", `function mk(k) { let c = 0; const f = (n) => { if (n === 0) return c + k; c++; return f(n - 1) }; return f }
			function each(n, g) { if (n === 0) return g(0); return each(n - 1, (x) => g(x + 1)) }
			let r = 0; for (let k = 0; k < 20; k++) r += mk(k)(30000) + each(50, (x) => x); r`},
		{"captured locals", `function cap(n, acc) { const get = () => n; if (n === 0) return acc; return cap(n - 1, acc + get()) }
			let r = 0; for (let k = 0; k < 20; k++) r += cap(3000, k); r`},
		{"a built-in", `function top(n, a) { if (n === 0) return Math.max(a, 7); return top(n - 1, a + 1) }
			let r = 0; for (let k = 0; k < 20; k++) r += top(1000, k); r`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(opts ...quickjs.Option) (string, vm.JITStats) {
				r := quickjs.New(opts...)
				// A few calls warm a function up, as these tests run it.
				hostaccess.VM(r).SetJITWarmUp(8, 8)
				defer r.Close()
				v, err := r.Eval(`"use strict"; ` + tc.src)
				if err != nil {
					t.Fatal(err)
				}
				return v.String(), hostaccess.VM(r).JITStats()
			}
			want, _ := run()
			got, st := run(quickjs.WithJIT())
			if got != want {
				t.Fatalf("jit: %s, interpreter: %s", got, want)
			}
			if st.Budget != 0 && st.SSAEntries == 0 {
				t.Errorf("nothing ran natively: %+v", st)
			}
		})
	}
	// A loop of calls in tail position polls, as a loop's back-edge does:
	// the host can stop it.
	r := quickjs.New(quickjs.WithJIT())
	// A few calls warm a function up, as these tests run it.
	hostaccess.VM(r).SetJITWarmUp(8, 8)
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := r.EvalContext(ctx, `"use strict"; function inf(n) { return inf(n + 1) } inf(0)`); err == nil {
		t.Fatal("an endless loop of calls in tail position ended")
	}
}

// An arrow's this is the one it captured, however it is called: as a
// method, through call, in tail position, by native code or inlined into
// it. A native call or an inlined body would have read the call's receiver
// there (Are We Fast Yet's Richards, whose task handlers are arrows a task
// calls as its method, failed with the JIT).
func TestJITArrowThis(t *testing.T) {
	cases := []struct{ name, src string }{
		{"as a method, in tail position", `class Owner { constructor() { this.tag = "owner"; this.task = new Task(() => this.tag) } }
			class Task { constructor(f) { this.f = f; this.tag = "task" } run() { return this.f() } }
			function loop(t, n) { let s = ""; for (let i = 0; i < n; i++) s = t.run(); return s }
			const o = new Owner(); let r = ""; for (let k = 0; k < 200; k++) r = loop(o.task, 100); r`},
		{"as a method", `class Owner { constructor() { this.tag = "owner"; this.task = new Task(() => this.tag) } }
			class Task { constructor(f) { this.f = f; this.tag = "task" } run() { const v = this.f(); return v } }
			function loop(t, n) { let s = ""; for (let i = 0; i < n; i++) s = t.run(); return s }
			const o = new Owner(); let r = ""; for (let k = 0; k < 200; k++) r = loop(o.task, 100); r`},
		{"through call", `const o = { v: 3, mk() { return (x) => this.v + x } }
			const f = o.mk(); let s = 0; for (let i = 0; i < 20000; i++) s += f.call({ v: 100 }, i % 7); s`},
		{"from a loop, closures made anew", `function mk(v) { return { v, f: () => this === undefined ? v : -1 } }
			let s = 0; for (let i = 0; i < 20000; i++) { const o = mk(i % 5); s += o.f() } s`},
		{"calling itself in tail position", `const o = { n: 7, mk() { const g = (k) => k === 0 ? this.n : g(k - 1); return g } }
			const g = o.mk(); let s = 0; for (let i = 0; i < 2000; i++) s += g.call({ n: 100 }, 50); s`},
		// Compiled, its calls in tail position made as calls, and, deep
		// in the stack, by Go, which gives its frame up: a hundred
		// thousand deep, as the interpreter's strict code's.
		{"calling itself in tail position, deeply", `const o = { n: 7, mk() { "use strict"; const g = (k) => k === 0 ? this.n : g(k - 1); return g } }
			const g = o.mk(); let s = 0; for (let i = 0; i < 20; i++) s += g.call({ n: 100 }, 100000); s`},
		{"calling methods in tail position, as Richards' tasks", `class Sched { constructor() { this.n = 0 } bump(x) { this.n += x; return this } hold() { return this.n }
				task() { return (w) => w === 0 ? this.hold() : this.bump(w) } }
			const sc = new Sched(), t = sc.task(); let s = 0; for (let i = 0; i < 20000; i++) { const r = t(i & 3); s += typeof r === "number" ? r : r.n } s`},
	}
	for _, tc := range cases {
		for _, strict := range []string{"", `"use strict"; `} {
			t.Run(tc.name+map[string]string{"": "", `"use strict"; `: ", strict"}[strict], func(t *testing.T) {
				run := func(opts ...quickjs.Option) string {
					r := quickjs.New(opts...)
					// A few calls warm a function up, as these tests run it.
					hostaccess.VM(r).SetJITWarmUp(8, 8)
					defer r.Close()
					v, err := r.Eval(strict + tc.src)
					if err != nil {
						t.Fatal(err)
					}
					return v.String()
				}
				if want, got := run(), run(quickjs.WithJIT()); got != want {
					t.Fatalf("jit: %s, interpreter: %s", got, want)
				}
			})
		}
	}
}

// Top-level code -- a script's, a module's, an indirect eval's -- runs
// natively from its loops, as V8 enters it by on-stack replacement: its
// globals, script-level lexical bindings and a module's bindings read where
// they are, an import in its exporter's binding. Declarations between loops
// go on natively; a direct eval's code, which reads its caller's scopes,
// stays in the interpreter.
func TestJITTopLevel(t *testing.T) {
	cases := []struct {
		name    string
		scripts []string
		modules map[string]string
	}{
		{name: "globals", scripts: []string{`var g = 0; let l = 1; const c = 2;
			for (let i = 0; i < 3000; i++) { g += i * c; l = (l + g) % 1000003 }
			var out = g + ":" + l`}},
		{name: "declarations between loops", scripts: []string{`let a = []; for (let i = 0; i < 3000; i++) a.push(i * 2);
			let t = 0; for (const v of a) t += v;
			class P { constructor(x) { this.x = x } }
			const ps = []; for (let i = 0; i < 3000; i++) ps.push(new P(i));
			let u = 0; for (const p of ps) u += p.x;
			var out = t + ":" + u`}},
		{name: "this and closures", scripts: []string{`this.k = 0; function f(x) { return x + 1 }
			let t = 0; for (let i = 0; i < 3000; i++) { const h = () => i; this.k += h(); t = f(t) }
			var out = this.k + ":" + t`}},
		{name: "temporal dead zone", scripts: []string{`let caught = 0;
			for (let i = 0; i < 3000; i++) { try { if (later > 0) caught-- } catch (e) { caught += e instanceof ReferenceError ? 1 : 0 } }
			let later = 1; var out = caught`}},
		{name: "throw and catch", scripts: []string{`let n = 0;
			for (let i = 0; i < 3000; i++) { try { if (i % 7 == 0) throw new Error("x" + i); n++ } catch (e) { n += e.message.length } }
			var out = n`}},
		{name: "a later script's loop", scripts: []string{`let shared = 5; var base = 3;`,
			`let s = 0; for (let i = 0; i < 3000; i++) s += shared * base + i; var out = s`}},
		{name: "indirect and direct eval", scripts: []string{`var out = (0, eval)("var q = 0; for (let i = 0; i < 3000; i++) q += i; q");
			function f() { var p = 1; eval("for (var i = 0; i < 3000; i++) p += i"); return p }
			out += ":" + f()`}},
		{name: "module bindings", modules: map[string]string{"main.mjs": `let m = 0; export let e = 1; var v = 2; const k = 3;
			for (let i = 0; i < 3000; i++) { m += i * k; e = m % 7; v++ }
			globalThis.out = m + ":" + e + ":" + v`}},
		{name: "imports", modules: map[string]string{
			"a.mjs": `export let counter = 0; export const K = 7; export function bump() { counter++ } export default 11;`,
			"b.mjs": `export { counter as c, K } from "./a.mjs"; import d from "./a.mjs"; export { d };`,
			"main.mjs": `import { counter, K, bump } from "./a.mjs"; import { c, K as K2, d } from "./b.mjs"; import def, * as ns from "./a.mjs";
				let s = 0; for (let i = 0; i < 3000; i++) { s += K + K2 + d + def + ns.K + (i % 3); if (i % 100 == 0) bump(); s += counter + c }
				globalThis.out = s + ":" + counter`}},
		{name: "globals from a module", scripts: []string{`var fromScript = 4; let lexical = 6;`},
			modules: map[string]string{"main.mjs": `let s = 0; for (let i = 0; i < 3000; i++) s += Math.max(i % 5, 2) + fromScript + lexical + (typeof undefined === "undefined" ? 1 : 0);
				globalThis.out = s`}},
		{name: "a cycle's binding before it is initialized", modules: map[string]string{
			"a.mjs": `import "./b.mjs"; export let late = 1;`,
			"b.mjs": `import { late } from "./a.mjs"; let caught = 0;
				for (let i = 0; i < 3000; i++) { try { caught -= late } catch (e) { caught += e instanceof ReferenceError ? 1 : 0 } }
				globalThis.out = caught;`,
			"main.mjs": `import "./a.mjs";`}},
		{name: "top-level await", modules: map[string]string{"main.mjs": `let s = 0;
			for (let i = 0; i < 3000; i++) { s += i; if (i % 1000 == 0) s += await i }
			globalThis.out = s`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(opts ...quickjs.Option) (string, vm.JITStats) {
				r := quickjs.New(opts...)
				// A few calls warm a function up, as these tests run it.
				hostaccess.VM(r).SetJITWarmUp(8, 8)
				defer r.Close()
				for _, src := range tc.scripts {
					if _, err := r.Eval(src); err != nil {
						t.Fatal(err)
					}
				}
				if tc.modules != nil {
					r.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
						name := strings.TrimPrefix(specifier, "./")
						return tc.modules[name], name, nil
					})
					ns, err := r.EvalModule("main.mjs", tc.modules["main.mjs"])
					if err != nil {
						t.Fatal(err)
					}
					if _, err := ns.Await(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				return evalString(t, r, "String(out)"), hostaccess.VM(r).JITStats()
			}
			want, _ := run()
			got, st := run(quickjs.WithJIT())
			if got != want {
				t.Fatalf("jit: %s, interpreter: %s", got, want)
			}
			if st.Budget != 0 && st.SSAEntries == 0 {
				t.Errorf("nothing ran natively: %+v", st)
			}
		})
	}
}

// A for...of step the JIT makes by a call of Go runs the iterator's next,
// which may assign a binding the loop's function captured: the loop reads
// it again after each step, as the interpreter does, rather than the value
// it had before.
func TestJITUpvalueAfterIteratorStep(t *testing.T) {
	src := `(function () {
		let x = 0;
		function make(n) {
			let i = 0;
			return { [Symbol.iterator]() { return this }, next() { x++; i++; return { done: i > n, value: i } } };
		}
		function g(it) { let s = 0; for (const v of it) s = s * 3 + x + v; return s }
		const out = [];
		for (let k = 0; k < 200; k++) { x = k; out.push(g(make(5))) }
		return out[0] + "," + out[1] + "," + out[199] + "," + out.reduce((a, b) => (a + b) % 1000003, 0);
	})()`
	run := func(opts ...quickjs.Option) (string, vm.JITStats) {
		r := quickjs.New(opts...)
		// A few calls warm a function up, as these tests run it.
		hostaccess.VM(r).SetJITWarmUp(8, 8)
		defer r.Close()
		return evalString(t, r, src), hostaccess.VM(r).JITStats()
	}
	want, _ := run()
	if want != "358,479,24437,479494" {
		t.Fatalf("interpreter: %s", want)
	}
	got, st := run(quickjs.WithJIT())
	if got != want {
		t.Fatalf("jit: %s, interpreter: %s", got, want)
	}
	if st.Budget != 0 && st.SSAEntries == 0 {
		t.Fatalf("g did not run natively: %+v", st)
	}
}

// Functions whose locals inner closures capture, and that make closures,
// give what the interpreter gives with the JIT on: a closure reads and
// assigns a captured local where the frame has it, so native code writes
// one there as it assigns it, and reads it again after anything that may
// have run a closure -- a call, an iterator's step. Each case's function
// loops, so that the JIT compiles it.
func TestJITCapturedLocals(t *testing.T) {
	cases := []struct{ name, src string }{
		{"per-iteration bindings", `function f(n) { const fs = []; for (let i = 0; i < n; i++) { fs.push(() => i * 2) } let s = 0; for (const g of fs) s += g(); return s }
			let r = 0; for (let k = 0; k < 60; k++) r += f(k); r`},
		{"callback assigns", `function f(a) { let sum = 0, count = 0; for (let i = 0; i < a.length; i++) { a[i].forEach(x => { sum += x; count++ }) } return sum + "/" + count }
			const a = []; for (let i = 0; i < 40; i++) a.push([i, i + 1, i * 2]); let r = ""; for (let k = 0; k < 30; k++) r = f(a); r`},
		{"closure reads the latest", `function f(n) { let x = 0; const get = () => x; let s = 0; for (let i = 0; i < n; i++) { x = i * 3; s += get() } x = -1; return s + get() }
			let r = 0; for (let k = 0; k < 80; k++) r += f(k); r`},
		{"closure assigns, loop reads", `function f(n) { let x = 1; const bump = () => { x = x * 2 % 1000003 }; let s = 0; for (let i = 0; i < n; i++) { bump(); s = (s + x) % 1000003 } return s }
			let r = 0; for (let k = 0; k < 80; k++) r += f(k); r`},
		{"temporal dead zone", `function f(n) { let s = 0; for (let i = 0; i < n; i++) { const peek = () => { try { return y } catch (e) { return e.name } }; s += peek() === "ReferenceError" ? 1 : 0; let y = i; s += peek() } return s }
			let r = 0; for (let k = 0; k < 50; k++) r += f(k); r`},
		{"arrow this", `const o = { base: 5, run(n) { let t = 0; for (let i = 0; i < n; i++) { const add = v => this.base + v + t; t = add(i) % 9973 } return t } };
			let r = 0; for (let k = 0; k < 60; k++) r += o.run(k); r`},
		{"try and catch", `function f(n) { let last = "", c = 0; for (let i = 0; i < n; i++) { try { if (i % 3 == 0) throw new Error("e" + i); c++ } catch (e) { const m = () => e.message; last = m() } } return last + ":" + c }
			let r = ""; for (let k = 0; k < 60; k++) r = f(k + 1); r`},
		{"two levels", `function f(n) { let a = 0; let s = 0; for (let i = 0; i < n; i++) { let b = i; const g = () => { const h = () => a + b; a++; return h() }; s += g() } return s + a }
			let r = 0; for (let k = 0; k < 60; k++) r += f(k); r`},
		{"natively called", `function leaf(n) { let c = 0; const inc = () => c++; for (let i = 0; i < n; i++) inc(); return c }
			function top(n) { let s = 0; for (let i = 0; i < n; i++) s += leaf(i % 7); return s }
			let r = 0; for (let k = 0; k < 60; k++) r += top(k * 3); r`},
		{"generator", `function* gen(n) { let x = 0; const next = () => ++x; for (let i = 0; i < n; i++) yield next() * 10 + i }
			let r = 0; for (let k = 0; k < 60; k++) for (const v of gen(k)) r = (r + v) % 1000003; r`},
		{"async", `async function af(n) { let total = 0; const add = v => { total += v }; for (let i = 0; i < n; i++) { add(await i) } return total }
			let r = 0; (async () => { for (let k = 0; k < 40; k++) r += await af(k) })(); r`},
		{"escapes and is called later", `function make(n) { let acc = []; for (let i = 0; i < n; i++) acc.push(i); let total = 0; for (const v of acc) total += v; return () => total + acc.length }
			let r = 0; for (let k = 0; k < 60; k++) r += make(k)(); r`},
		{"callbacks of one function", `function each(a, fn) { let s = 0; for (let i = 0; i < a.length; i++) s += fn(a[i]); return s }
			function run(k) { let calls = 0; const r = each([1, 2, 3, 4, 5, 6, 7, 8], x => { calls++; return x * k }); return r + calls }
			let r = 0; for (let k = 0; k < 200; k++) r += run(k); r`},
		{"a callback that assigns its maker's local", `function each(a, fn) { for (let i = 0; i < a.length; i++) fn(a[i]) }
			function run(k) { let acc = k; each([3, 1, 4, 1, 5], x => { acc = (acc * 31 + x) % 1000003 }); return acc }
			let r = 0; for (let k = 0; k < 200; k++) r = (r + run(k)) % 1000003; r`},
		{"different functions at one site", `function each(a, fn) { let s = 0; for (let i = 0; i < a.length; i++) s += fn(a[i]); return s }
			function run(k) { const f = k % 3 == 0 ? (x => x + k) : k % 3 == 1 ? (x => x * k) : (x => x - k); return each([1, 2, 3, 4], f) }
			let r = 0; for (let k = 0; k < 200; k++) r += run(k); r`},
		// A captured local read straight from its slot by an operation
		// that finds an object's pointer word where the value came from,
		// a merge of its cell's loads: what the merge is kept live there,
		// though no frame state names the local (mir's shadow uses).
		{"pointer word from a captured local's merge", `var r = 0;
			for (var rep = 0; rep < 300; rep++) {
			function Fmt(locales, opts) { this.l = locales; this.o = opts; }
			var dates = [1, 2, 3];
			var defaults = {year: "numeric"};
			var f = function (l, o) { return String(this) + l + (o ? o.hour12 : "") };
			[undefined, ["de"], ["en"], ["ja"]].forEach(function (locales) {
			    [{hour12: false}, {hour12: true, x: 1}].forEach(function (options) {
			        var constructorOptions = options;
			        if (options === undefined) {
			        } else if (options.day === undefined) {
			            constructorOptions = Object.create(defaults);
			            for (var prop in options) {
			                if (options.hasOwnProperty(prop)) {
			                    constructorOptions[prop] = options[prop];
			                }
			            }
			        }
			        var ref = new Fmt(locales, constructorOptions);
			        var formatted = dates.map(function (a) { return f.call(a, locales, options); });
			        if (formatted.length !== 3 || ref.o === undefined) throw new Error("bad"); r++;
			    });
			});
			}`},
		// A native callee makes closures on its own frame: what they
		// captured is closed as it returns natively, or given to the frame
		// Go makes of it as a throw leaves it.
		{"closures a native callee makes and returns", `function mk(n) { let c = 0; for (let j = 0; j < n % 3 + 1; j++) c += j; const inc = () => ++c; return inc }
			for (let w = 0; w < 50; w++) mk(w);
			function top(n) { let s = 0; for (let i = 0; i < n; i++) { const f = mk(i), g = mk(i + 7); f(); s += f() * 1000 + g() } return s }
			let r = 0; for (let k = 0; k < 60; k++) r += top(k); r`},
		{"closures thrown from a native callee", `function mk(n) { let c = 0; for (let j = 0; j < n % 3 + 1; j++) c += j; const g = () => c; if (n % 5 == 4) throw g; c++; return g }
			for (let w = 0; w < 50; w++) try { mk(w) } catch (e) {}
			function burn(n) { let a = n, b = n * 2, c = n * 3; for (let j = 0; j < 2; j++) a += b + c; return a }
			function top(n) { let s = 0; for (let i = 0; i < n; i++) { try { s += mk(i)() } catch (g) { s += burn(i) + g() * 100 } } return s }
			let r = 0; for (let k = 0; k < 60; k++) r += top(k); r`},
		{"per-iteration bindings in a native callee", `function fill(n) { const fs = []; for (let i = 0; i < n; i++) fs.push(() => i); return fs }
			for (let w = 0; w < 50; w++) fill(w % 9);
			function top(n) { let s = 0; for (let k = 0; k < n; k++) { const a = fill(k % 9), b = fill(k % 7); for (const f of a) s += f() * 10; for (const f of b) s += f() } return s }
			let r = 0; for (let k = 0; k < 40; k++) r += top(k); r`},
		// A local a closure assigns is read again after a call, whoever
		// assigns it: a closure the closure makes too.
		{"assigned two closures down", `function f(n) { let x = 0; const outer = () => { const inner = () => { x += 2 }; inner() }; let s = 0; for (let i = 0; i < n; i++) { outer(); s += x } return s }
			let r = 0; for (let k = 0; k < 80; k++) r += f(k); r`},
		{"sort comparator", `function f(a) { let calls = 0; const b = a.slice(); for (let i = 0; i < 3; i++) b.sort((x, y) => { calls++; return x - y }); return b[0] + b[b.length - 1] + calls * 0 }
			const a = []; for (let i = 0; i < 50; i++) a.push((i * 37) % 101); let r = 0; for (let k = 0; k < 40; k++) r += f(a); r`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(opts ...quickjs.Option) (string, vm.JITStats) {
				r := quickjs.New(opts...)
				// A few calls warm a function up, as these tests run it.
				hostaccess.VM(r).SetJITWarmUp(8, 8)
				defer r.Close()
				if _, err := r.Eval(tc.src); err != nil {
					t.Fatal(err)
				}
				// The async case settles its promises first.
				return evalString(t, r, "String(r)"), hostaccess.VM(r).JITStats()
			}
			want, _ := run()
			got, st := run(quickjs.WithJIT())
			if got != want {
				t.Fatalf("jit: %s, interpreter: %s", got, want)
			}
			if st.Budget != 0 && st.SSAEntries == 0 {
				t.Errorf("nothing ran natively: %+v", st)
			}
		})
	}
}
