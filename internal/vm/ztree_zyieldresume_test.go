package vm

import (
	"strconv"
	"strings"
	"testing"
)

// TestGeneratorResumption pins what a resumption keeps, in both tiers, now
// that the common one -- next() on a generator its tree suspended, from a
// for...of loop or a call -- is made in one place (resumeTree): a stack
// trace taken in the body, with next's frame under it; resumptions from
// different depths of the stack; an operand stack live across the yield;
// a body's own exception handlers and the closures it made; `with`; the
// generator running when it is resumed again; return and throw after a
// resumption made there; and an async function's awaits and its
// rejections.
func TestGeneratorResumption(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
		async           bool
		// tree says f runs as a tree.
		tree bool
	}{
		{name: "stackTrace", tree: true, src: `function* f() { for (let i = 0; i < 2; i++) yield new Error('e').stack.split('\n').slice(1, 4).map(l => l.trim().split(' ')[1]).join() }
function outer() { const r = []; for (const s of f()) r.push(s); return r.join(';') } outer()`, want: "f,Object.next,outer;f,Object.next,outer"},
		{name: "depths", tree: true, src: `function* f() { for (let i = 0; ; i++) yield i + ':' + new Error().stack.split('\n').length }
var g = f(); function d(n) { return n ? d(n - 1) : g.next().value } [d(0), d(5), d(1), g.next().value].join()`, want: "0:5,1:10,2:6,3:4"},
		{name: "liveStack", tree: true, src: `function* f() { var s = 0; for (let i = 0; i < 3; i++) s += 10 * (yield i) + (yield -i); return s }
var g = f(), out = [], r = g.next(); for (let k = 1; !r.done; k++) { out.push(r.value); r = g.next(k) } out.push(r.value); out.join()`, want: "0,0,1,-1,2,-2,102"},
		{name: "forOfInBody", tree: true, src: `function* f() { for (const x of [1, 2, 3]) for (const y of 'ab') yield x + y }
var s = []; for (const v of f()) s.push(v); s.join()`, want: "1a,1b,2a,2b,3a,3b"},
		{name: "handlers", tree: true, src: `function* f() { var s = ''; for (let i = 0; i < 3; i++) { try { yield i; if (i == 1) throw 'x' } catch (e) { s += 'c' + e } finally { s += 'f' } } return s }
var g = f(), v = []; for (let r = g.next(); ; r = g.next()) { v.push(r.value); if (r.done) break } v.join()`, want: "0,1,2,fcxff"},
		{name: "closures", tree: true, src: `function* f() { const fs = []; for (let i = 0; i < 3; i++) { yield fs.push(() => i * 10) } return fs.map(h => h()).join() }
var g = f(); for (const x of g); g = f(); g.next(); g.next(); g.next(); g.next().value`, want: "0,10,20"},
		{name: "with", src: `function* f(o) { for (var i = 0; i < 2; i++) with (o) { yield x + i; x += 10 } }
var s = []; for (const v of f({ x: 1 })) s.push(v); s.join()`, want: "1,12"},
		{name: "running", tree: true, src: `var g; function* f() { for (let i = 0; i < 2; i++) { yield i; try { g.next() } catch (e) { yield e.message } } }
g = f(); [...g].join()`, want: "0,the generator is already running,1,the generator is already running"},
		{name: "returnAfterResume", tree: true, src: `var s = ''; function* f() { try { for (let i = 0; ; i++) yield i } finally { s += 'F' } }
var g = f(); g.next(); g.next(); g.next(); [g.return(9).value, g.next().done, s].join()`, want: "9,true,F"},
		{name: "throwAfterResume", tree: true, src: `function* f() { for (let i = 0; ; i++) try { yield i } catch (e) { yield 'c' + e } }
var g = f(); g.next(); g.next(); [g.throw('x').value, g.next().value, g.next().value].join()`, want: "cx,2,3"},
		{name: "breakClosesGenerator", tree: true, src: `var s = ''; function* f() { try { for (let i = 0; ; i++) yield i } finally { s += 'F' } }
for (const v of f()) { s += v; if (v == 2) break } s`, want: "012F"},
		{name: "loopThrowsOut", tree: true, src: `function* f() { for (let i = 0; i < 5; i++) { if (i == 2) null.x; yield i } }
var s = ''; try { for (const v of f()) s += v } catch (e) { s += e.name } s`, want: "01TypeError"},
		{name: "noLoop", tree: true, src: `function* f(a, b) { yield a; var c = yield b; return a + b + c }
var g = f(1, 2); [g.next().value, g.next().value, g.next(3).value, g.next().done].join()`, want: "1,2,6,true"},
		{name: "asyncNoLoop", async: true, tree: true, src: `async function f(x) { var y = await x; return y + await Promise.resolve(1) } var p = f(2)`, want: "3"},
		{name: "await", async: true, tree: true, src: `async function f(n) { let s = 0; for (let i = 0; i < n; i++) s += await new Promise(r => r(i)); return s } var p = f(20)`, want: "190"},
		{name: "awaitRejected", async: true, tree: true, src: `async function f() { let s = ''; for (let i = 0; i < 3; i++) { try { s += await (i == 1 ? Promise.reject('r') : i) } catch (e) { s += e } } return s } var p = f()`, want: "0r2"},
		{name: "asyncGenerator", async: true, tree: true, src: `async function* f() { for (let i = 0; i < 3; i++) yield await i } async function main() { let s = ''; for await (const x of f()) s += x; return s } var p = main()`, want: "012"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				src := tt.src
				if tt.async {
					src = "var out = 'pending'; " + src + "; Promise.resolve(p).then(v => out = String(v), e => out = 'E ' + e)"
				}
				fn := compileForTest(t, src)
				r := New(Config{})
				v, err := r.Run(fn)
				if err == nil && tt.async {
					if err = r.runMicrotasks(); err == nil {
						v, err = r.Run(compileForTest(t, "out"))
					}
				}
				if err != nil {
					r.Close()
					t.Fatalf("tree %v: %v", on, err)
				}
				got, err := r.ToString(v)
				if err != nil || got.Go() != tt.want {
					t.Errorf("tree %v: got %v, %v; want %s", on, got, err, tt.want)
				}
				if r.frameDepth != 0 {
					t.Errorf("tree %v: left %d frames", on, r.frameDepth)
				}
				// The suspension a tree hands out is the runtime's one,
				// which keeps no value once it is read.
				if !r.treeSig.value.IsUndefined() && r.treeSig.value != (Value{}) {
					t.Errorf("tree %v: the suspension kept %v", on, r.treeSig.value)
				}
				r.Close()
				if on && tt.tree {
					assertTreeIn(t, fn, "f")
				}
			}
		})
	}
}

// TestGeneratorCreation pins what making a generator or an async function
// binds, in both tiers, now that a prologue of end_params alone is not run
// and the function's prototype is read as its own property: the prototype
// replaced by an object or by null, parameters missing, repeated or
// reassigned, no parameters, the property's attributes, and an async
// function's arguments. The answers are Node's.
func TestGeneratorCreation(t *testing.T) {
	defer SetTreeTier(true)
	src := `var out = [];
function* g(a, b) { yield a + b }
g.prototype = null;
out.push(Object.getPrototypeOf(g(1, 2)) === Object.getPrototypeOf(function* () {}).prototype, g(1, 2).next().value);
g.prototype = { hi() { return 'hi' } };
out.push(g().hi(), typeof g(1).next);
function* d(a, a) { yield a }
out.push(d(1, 2).next().value);
function* n() { for (let i = 0; i < 2; i++) yield i }
out.push([...n()].join(''));
function* p(a, b) { yield a; a = 5; yield a + b }
var it = p(1); out.push(it.next().value, String(it.next().value), JSON.stringify(Object.getOwnPropertyDescriptor(p, 'prototype')));
try { Object.defineProperty(p, 'prototype', { get() { return {} } }) } catch (e) { out.push(e.name) }
async function af(a, b) { return a + (await b) }
af(2, 3).then(v => out.push('async ' + v))`
	const want = `true,3,hi,undefined,2,01,1,NaN,{"value":{},"writable":true,"enumerable":false,"configurable":false},TypeError,async 5`
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		r := New(Config{})
		_, err := r.Run(compileForTest(t, src))
		if err == nil {
			err = r.runMicrotasks()
		}
		var v Value
		if err == nil {
			v, err = r.Run(compileForTest(t, "out.join()"))
		}
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", on, err)
		}
		if got, err := r.ToString(v); err != nil || got.Go() != want {
			t.Errorf("tree %v: got %v, %v\nwant %s", on, got, err, want)
		}
		r.Close()
	}
}

// TestAwaitOrder pins the order awaits resume in, in both tiers, now that
// an await of what is not an object makes no promise and an await's
// reaction is a job of its own kind, and the job queue is reused from its
// start: against then callbacks, a promise, undefined and null awaited, a
// thenable whose then is read and one that is called, a rejection caught,
// an async generator's yields and awaits under for await, and 150 async
// functions awaiting in turn, which keep the queue from emptying. The
// answers are Node's.
func TestAwaitOrder(t *testing.T) {
	defer SetTreeTier(true)
	src := `var log = [];
async function a() { log.push('a1'); await 1; log.push('a2'); await null; log.push('a3'); return 'A' }
async function b() { log.push('b1'); await Promise.resolve(); log.push('b2'); await undefined; log.push('b3') }
async function c() { log.push('c1'); await { get then() { log.push('get'); return undefined } }; log.push('c2'); await { then(r) { log.push('then'); r(5) } }; log.push('c3') }
async function d() { try { await Promise.reject('r'); } catch (e) { log.push('d' + e) } for (let i = 0; i < 3; i++) { log.push('d' + (await i)) } }
async function* g() { log.push('g1'); yield 1; log.push('g2'); yield await 2; log.push('g3') }
(async () => { for await (const x of g()) log.push('gx' + x) })();
a().then(v => log.push(v)); b(); c(); d();
Promise.resolve().then(() => log.push('t1')).then(() => log.push('t2')).then(() => log.push('t3')).then(() => log.push('t4')).then(() => log.push('t5'));
var many = [];
for (let k = 0; k < 150; k++) (async () => { for (let i = 0; i < 4; i++) { await i; many.push(k * 10 + i) } })();
Promise.resolve().then(() => many.push('x'));`
	wantLog := "g1,a1,b1,c1,get,a2,b2,c2,dr,t1,gx1,g2,a3,b3,then,d0,t2,A,c3,d1,t3,d2,t4,gx2,g3,t5"
	var many []string
	for i := 0; i < 4; i++ {
		for k := 0; k < 150; k++ {
			many = append(many, strconv.Itoa(k*10+i))
		}
		if i == 0 {
			many = append(many, "x")
		}
	}
	want := wantLog + " " + strings.Join(many, ",")
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		r := New(Config{})
		_, err := r.Run(compileForTest(t, src))
		if err == nil {
			err = r.runMicrotasks()
		}
		var v Value
		if err == nil {
			v, err = r.Run(compileForTest(t, "log.join() + ' ' + many.join()"))
		}
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", on, err)
		}
		if got, err := r.ToString(v); err != nil || got.Go() != want {
			t.Errorf("tree %v: got %v, %v\nwant %s", on, got, err, want)
		}
		if r.HasPendingJobs() || len(r.microtasks) != 0 {
			t.Errorf("tree %v: %d jobs left from %d", on, len(r.microtasks), r.jobHead)
		}
		r.Close()
	}
}

// TestForOfGenerator pins that a for-of loop over a generator calls what
// the protocol finds, in both tiers, now that one whose iterator method and
// next are the intrinsics, inherited through ordinary objects, is not looked
// up: a prototype that is a proxy, next a getter on the function's
// prototype or a method of its own, an iterator method on the generator
// itself, and the intrinsics replaced and put back. The answers are Node's.
func TestForOfGenerator(t *testing.T) {
	defer SetTreeTier(true)
	src := `var log = [];
function* g() { for (let i = 0; i < 2; i++) yield i }
function run(it) { var s = []; for (const x of it) s.push(x); return s.join('') }
log.push(run(g()));
g.prototype = new Proxy(Object.create(Object.getPrototypeOf(g.prototype)), { get(t, k, r) { log.push(typeof k == 'symbol' ? 'sym' : k); return Reflect.get(t, k, r) } });
log.push(run(g()));
function* h() { yield 1; yield 2 }
Object.defineProperty(h.prototype, 'next', { get() { log.push('getnext'); return function () { log.push('mynext'); return { done: true } } } });
log.push(run(h()));
function* k() { yield 1 }
k.prototype.next = function () { log.push('own'); return { done: true, value: 9 } };
log.push(run(k()));
function* m() { yield 3 }
var it = m(); it[Symbol.iterator] = function () { log.push('iter'); return [7, 8][Symbol.iterator]() };
log.push(run(it));
var GP = Object.getPrototypeOf(g).prototype, saved = GP[Symbol.iterator];
GP[Symbol.iterator] = function () { log.push('gpiter'); return this };
function* n() { yield 4 }
log.push(run(n()));
GP[Symbol.iterator] = saved;
var savedNext = GP.next;
GP.next = function () { log.push('gpnext'); return savedNext.call(this) };
log.push(run(n()));
GP.next = savedNext;
log.push(run(n()));
log.join()`
	const want = "01,sym,next,01,getnext,mynext,,own,,iter,78,gpiter,4,gpnext,gpnext,4,4"
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", on, err)
		}
		if got, err := r.ToString(v); err != nil || got.Go() != want {
			t.Errorf("tree %v: got %v, %v\nwant %s", on, got, err, want)
		}
		r.Close()
	}
}
