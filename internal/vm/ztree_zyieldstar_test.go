package vm

import "testing"

// TestTreeDelegation pins that yield* and for await give in the tree tier
// what they give in the interpreter, and run as trees: yield* over
// generators, arrays, strings and iterators of the script's own, with and
// without return and throw methods; next, return and throw forwarded to the
// delegate, a delegate's result handed out as it is, results that are not
// objects, methods that throw, nested delegations and delegations in
// finally clauses; yield* in async generators, over async and sync
// iterators; and for await over async generators and sync iterables of
// promises and values, left by break, return and throw, its iterator
// failing. Only a body with a loop is given a tree, and the jump back after
// a yield* is not one, so a case without one starts f's body with an empty
// loop. An async case leaves its promise in p, and out is what that settles
// to: its value, or E and what it threw. The answers are Node's.
func TestTreeDelegation(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
		// async says the script settles out once its jobs have run.
		async bool
	}{
		{name: "generator", src: `function* inner(n) { for (let i = 0; i < n; i++) yield i; return 'r' } function* f() { for (var k = 0; k < 1; k++); var r = yield* inner(3); yield r } [...f()].join()`, want: "0,1,2,r"},
		{name: "array", src: `function* f() { for (var k = 0; k < 1; k++); yield* [1, 2, 3] } [...f()].join()`, want: "1,2,3"},
		{name: "string", src: `function* f() { for (var k = 0; k < 1; k++); yield* 'a\u{1F600}c' } [...f()].length + [...f()].join('|')`, want: "3a|😀|c"},
		{name: "ownIterator", src: `var log = []; var it = { i: 0, [Symbol.iterator]() { return this }, next(v) { log.push(String(v)); return { value: this.i, done: this.i++ >= 2 } } }; function* f() { for (var k = 0; k < 1; k++); var r = yield* it; log.push('r' + r) } var g = f(); g.next('a'); g.next('b'); g.next('c'); log.join()`, want: "undefined,b,c,r2"},
		{name: "sent", src: `function* inner() { var a = yield 1; var b = yield a + 1; return a + b } function* f() { for (var k = 0; k < 1; k++); return yield* inner() } var g = f(); g.next(9).value + ',' + g.next(10).value + ',' + JSON.stringify(g.next(5))`, want: "1,11,{\"value\":15,\"done\":true}"},
		{name: "returnForwarded", src: `var s = ''; function* inner() { try { yield 1; yield 2 } finally { s += 'I' } } function* f() { for (var k = 0; k < 1; k++); try { yield* inner() } finally { s += 'F' } } var g = f(); g.next(); JSON.stringify(g.return(5)) + s + g.next().done`, want: "{\"value\":5,\"done\":true}IFtrue"},
		{name: "returnNoMethod", src: `var it = { [Symbol.iterator]() { return this }, next() { return { value: 1, done: false } } }; function* f() { for (var k = 0; k < 1; k++); yield* it; yield 'after' } var g = f(); g.next(); JSON.stringify(g.return(7)) + JSON.stringify(g.next())`, want: "{\"value\":7,\"done\":true}{\"done\":true}"},
		{name: "returnNotDone", src: `var it = { [Symbol.iterator]() { return this }, next() { return { value: 1, done: false } }, return(v) { return { value: 'r' + v, done: false } } }; function* f() { for (var k = 0; k < 1; k++); yield* it } var g = f(); g.next(); JSON.stringify(g.return(7)) + JSON.stringify(g.next())`, want: "{\"value\":\"r7\",\"done\":false}{\"value\":1,\"done\":false}"},
		{name: "returnThrows", src: `var it = { [Symbol.iterator]() { return this }, next() { return { value: 1, done: false } }, return() { throw 'rt' } }; function* f() { for (var k = 0; k < 1; k++); try { yield* it } catch (e) { yield 'c' + e } } var g = f(); g.next(); JSON.stringify(g.return(7)) + JSON.stringify(g.next())`, want: "{\"value\":\"crt\",\"done\":false}{\"done\":true}"},
		{name: "throwForwarded", src: `function* inner() { while (true) { try { yield 1 } catch (e) { yield 'c' + e } } } function* f() { for (var k = 0; k < 1; k++); yield* inner() } var g = f(); g.next(); g.throw('x').value + ',' + g.next().value`, want: "cx,1"},
		{name: "throwNoMethod", src: `var log = []; var it = { [Symbol.iterator]() { return this }, next() { return { value: 1, done: false } }, return() { log.push('ret'); return {} } }; function* f() { for (var k = 0; k < 1; k++); try { yield* it } catch (e) { log.push(e.constructor.name) } } var g = f(); g.next(); log.push(JSON.stringify(g.throw('x'))); log.join()`, want: "ret,TypeError,{\"done\":true}"},
		{name: "throwNoMethodUncaught", src: `var log = []; var it = { [Symbol.iterator]() { return this }, next() { return { value: 1, done: false } } }; function* f() { for (var k = 0; k < 1; k++); yield* it } var g = f(); g.next(); try { g.throw('x') } catch (e) { log.push(e.constructor.name) } log.push(g.next().done); log.join()`, want: "TypeError,true"},
		{name: "throwUncaughtByInner", src: `var s = ''; function* inner() { try { yield 1 } finally { s += 'I' } } function* f() { for (var k = 0; k < 1; k++); try { yield* inner() } catch (e) { s += 'c' + e; yield 'y' } } var g = f(); g.next(); g.throw('x').value + s`, want: "yIcx"},
		{name: "raw", src: `var res = { value: 1, done: false }, n = 0; var it = { [Symbol.iterator]() { return this }, next() { return n++ < 2 ? res : { value: 'end', done: true } } }; function* f() { for (var k = 0; k < 1; k++); yield* it } var g = f(); String(g.next() === res)`, want: "true"},
		{name: "valueGetter", src: `var log = []; var it = { [Symbol.iterator]() { return this }, n: 0, next() { var d = this.n++ > 0; return { get value() { log.push('v' + d); return 7 }, get done() { log.push('d'); return d } } } }; function* f() { for (var k = 0; k < 1; k++); var r = yield* it; log.push('r' + r) } var g = f(); g.next(); g.next(); log.join()`, want: "d,d,vtrue,r7"},
		{name: "notObject", src: `var it = { [Symbol.iterator]() { return this }, next() { return 1 } }; function* f() { for (var k = 0; k < 1; k++); try { yield* it } catch (e) { yield e.constructor.name } } f().next().value`, want: "TypeError"},
		{name: "nextThrows", src: `var it = { [Symbol.iterator]() { return this }, next() { throw 'nt' } }; function* f() { for (var k = 0; k < 1; k++); try { yield* it } catch (e) { yield 'c' + e } } f().next().value`, want: "cnt"},
		{name: "notIterable", src: `function* f() { for (var k = 0; k < 1; k++); try { yield* 5 } catch (e) { yield e.constructor.name } } f().next().value`, want: "TypeError"},
		{name: "nested", src: `var s = ''; function* a() { try { var x = yield 1; s += x; return 'A' } finally { s += 'a' } } function* b() { for (var k = 0; k < 1; k++); var r = yield* a(); s += r; return yield* a() } function* f() { for (var k = 0; k < 1; k++); var r = yield* b(); s += 'f' + r } var g = f(); g.next(); g.next('x'); g.next('y'); g.next('z'); s`, want: "xaAyafA"},
		{name: "nestedReturn", src: `var s = ''; function* a() { try { yield 1 } finally { s += 'a' } } function* b() { for (var k = 0; k < 1; k++); try { yield* a() } finally { s += 'b' } } function* f() { for (var k = 0; k < 1; k++); try { yield* b() } finally { s += 'f' } } var g = f(); g.next(); JSON.stringify(g.return(4)) + s`, want: "{\"value\":4,\"done\":true}abf"},
		{name: "finallyYields", src: `var s = ''; function* inner() { try { yield 1 } finally { yield 'i' } } function* f() { for (var k = 0; k < 1; k++); try { yield* inner() } finally { s += 'F' } } var g = f(); g.next(); var a = g.return(5); var b = g.next(); a.value + ',' + a.done + ',' + b.value + ',' + b.done + s`, want: "i,false,undefined,trueF"},
		{name: "inLoop", src: `function* inner(i) { yield i; yield i * 10 } function* f() { for (let i = 1; i < 4; i++) yield* inner(i) } [...f()].join()`, want: "1,10,2,20,3,30"},
		{name: "inForOf", src: `var s = ''; function* inner(i) { try { yield i } finally { s += 'i' } } function* f() { for (const x of [1, 2, 3]) yield* inner(x) } var g = f(); g.next(); g.next(); g.return(0); s`, want: "ii"},
		{name: "busy", src: `function* inner() { yield 1; yield 2 } function* f(n) { var t = 0; for (let i = 0; i < n; i++) { t += i; if (i % 100 == 0) yield* inner() } return t } var x = 0, g = f(1000), r; while (!(r = g.next()).done) x += r.value; x + ',' + r.value`, want: "30,499500"},
		{name: "asyncGenerator", async: true, src: `async function* inner() { yield 1; yield 2; return 3 } async function* f() { for (var k = 0; k < 1; k++); const r = yield* inner(); yield r } async function main() { let s = ''; for await (const x of f()) s += x; return s } var p = main()`, want: "123"},
		{name: "asyncSyncIterable", async: true, src: `async function* f() { for (var k = 0; k < 1; k++); yield* [Promise.resolve(1), 2] } async function main() { let s = ''; for await (const x of f()) s += x; return s } var p = main()`, want: "12"},
		{name: "asyncSyncPromiseValue", async: true, src: `function* inner() { yield Promise.resolve('p'); return Promise.resolve('q') } async function* f() { for (var k = 0; k < 1; k++); var r = yield* inner(); yield r } async function main() { let s = []; for await (const x of f()) s.push(typeof x == 'string' ? x : 'obj'); return s.join() } var p = main()`, want: "p,q"},
		{name: "asyncSent", async: true, src: `async function* inner() { var a = yield 1; var b = yield a + 1; return a + b } async function* f() { for (var k = 0; k < 1; k++); return yield* inner() } async function main() { var g = f(); var r = [await g.next('x'), await g.next(10), await g.next(5)]; return JSON.stringify(r) } var p = main()`, want: "[{\"value\":1,\"done\":false},{\"value\":11,\"done\":false},{\"value\":15,\"done\":true}]"},
		{name: "asyncReturn", async: true, src: `var s = ''; async function* inner() { try { yield 1; yield 2 } finally { s += 'I' } } async function* f() { for (var k = 0; k < 1; k++); try { yield* inner() } finally { s += 'F' } } async function main() { var g = f(); await g.next(); var r = await g.return(5); return JSON.stringify(r) + s } var p = main()`, want: "{\"value\":5,\"done\":true}IF"},
		{name: "asyncReturnNoMethod", async: true, src: `var it = { [Symbol.asyncIterator]() { return this }, next() { return { value: 1, done: false } } }; async function* f() { for (var k = 0; k < 1; k++); yield* it } async function main() { var g = f(); await g.next(); return JSON.stringify(await g.return(Promise.resolve(7))) + JSON.stringify(await g.next()) } var p = main()`, want: "{\"value\":7,\"done\":true}{\"done\":true}"},
		{name: "asyncThrow", async: true, src: `async function* inner() { while (true) { try { yield 1 } catch (e) { yield 'c' + e } } } async function* f() { for (var k = 0; k < 1; k++); yield* inner() } async function main() { var g = f(); await g.next(); return (await g.throw('x')).value + ',' + (await g.next()).value } var p = main()`, want: "cx,1"},
		{name: "asyncThrowNoMethod", async: true, src: `var log = []; var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) }, return() { log.push('ret'); return {} } }; async function* f() { for (var k = 0; k < 1; k++); try { yield* it } catch (e) { log.push(e.constructor.name) } } async function main() { var g = f(); await g.next(); log.push(JSON.stringify(await g.throw('x'))); return log.join() } var p = main()`, want: "ret,TypeError,{\"done\":true}"},
		{name: "asyncNextRejects", async: true, src: `var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.reject('nr') } }; async function* f() { for (var k = 0; k < 1; k++); try { yield* it } catch (e) { yield 'c' + e } } async function main() { return (await f().next()).value } var p = main()`, want: "cnr"},
		{name: "asyncNotObject", async: true, src: `var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve(3) } }; async function* f() { for (var k = 0; k < 1; k++); try { yield* it } catch (e) { yield e.constructor.name } } async function main() { return (await f().next()).value } var p = main()`, want: "TypeError"},
		{name: "asyncNested", async: true, src: `var s = ''; async function* a() { try { var x = yield 1; s += x; return 'A' } finally { s += 'a' } } async function* b() { for (var k = 0; k < 1; k++); var r = yield* a(); s += r; return yield* a() } async function* f() { for (var k = 0; k < 1; k++); var r = yield* b(); s += 'f' + r } async function main() { var g = f(); await g.next(); await g.next('x'); await g.next('y'); await g.next('z'); return s } var p = main()`, want: "xaAyafA"},
		{name: "forAwaitAsyncGenerator", async: true, src: `async function* g() { for (let i = 0; i < 3; i++) yield i } async function f() { let s = ''; for await (const x of g()) s += x; return s } var p = f()`, want: "012"},
		{name: "forAwaitSync", async: true, src: `async function f() { let s = []; for await (const x of [1, Promise.resolve(2), 3]) s.push(x); return s.join() } var p = f()`, want: "1,2,3"},
		{name: "forAwaitBreak", async: true, src: `var log = []; function mk() { var i = 0; return { [Symbol.asyncIterator]() { return { next() { return Promise.resolve({ value: i++, done: false }) }, return() { log.push('ret'); return {} } } } } } async function f() { for await (const x of mk()) { log.push(x); if (x == 2) break } log.push('after'); return log.join() } var p = f()`, want: "0,1,2,ret,after"},
		{name: "forAwaitReturn", async: true, src: `var log = []; function mk() { var i = 0; return { [Symbol.asyncIterator]() { return { next() { return Promise.resolve({ value: i++, done: false }) }, return() { log.push('ret'); return {} } } } } } async function f() { for await (const x of mk()) { if (x == 1) return 'r' + x } } var p = f().then(v => v + log.join())`, want: "r1ret"},
		{name: "forAwaitThrow", async: true, src: `var log = []; function mk() { var i = 0; return { [Symbol.asyncIterator]() { return { next() { return Promise.resolve({ value: i++, done: false }) }, return() { log.push('ret'); return {} } } } } } async function f() { try { for await (const x of mk()) { if (x == 1) throw 't' + x } } catch (e) { log.push(e) } return log.join() } var p = f()`, want: "ret,t1"},
		{name: "forAwaitThrowUncaught", async: true, src: `var log = []; function mk() { var i = 0; return { [Symbol.asyncIterator]() { return { next() { return Promise.resolve({ value: i++, done: false }) }, return() { log.push('ret'); return {} } } } } } async function f() { for await (const x of mk()) { if (x == 1) throw 't' + x } } var p = f().catch(e => e + log.join())`, want: "t1ret"},
		{name: "forAwaitGeneratorBreak", async: true, src: `var s = '', done, p = new Promise(r => done = r); async function* g() { try { for (let i = 0; ; i++) yield i } finally { s += 'F'; done(s) } } async function f() { for await (const x of g()) { s += x; if (x == 2) break } } f()`, want: "012F"},
		{name: "forAwaitSyncGeneratorThrow", async: true, src: `var s = ''; function* g() { try { yield 1; yield 2 } finally { s += 'F' } } async function f() { try { for await (const x of g()) throw x } catch (e) { s += 'c' + e } return s } var p = f()`, want: "Fc1"},
		{name: "forAwaitNextRejects", async: true, src: `var it = { [Symbol.asyncIterator]() { return { next() { return Promise.reject('nr') } } } }; async function f() { try { for await (const x of it); } catch (e) { return 'c' + e } } var p = f()`, want: "cnr"},
		{name: "forAwaitNextThrows", async: true, src: `var it = { [Symbol.asyncIterator]() { return { next() { throw 'nt' } } } }; async function f() { try { for await (const x of it); } catch (e) { return 'c' + e } } var p = f()`, want: "cnt"},
		{name: "forAwaitNotObject", async: true, src: `var it = { [Symbol.asyncIterator]() { return { next() { return 4 } } } }; async function f() { try { for await (const x of it); } catch (e) { return e.constructor.name } } var p = f()`, want: "TypeError"},
		{name: "forAwaitRejectedValue", async: true, src: `var s = ''; function* g() { try { yield Promise.reject('rv'); yield 2 } finally { s += 'F' } } async function f() { try { for await (const x of g()); } catch (e) { return 'c' + e + s } } var p = f()`, want: "crvF"},
		{name: "forAwaitNotIterable", async: true, src: `async function f() { try { for await (const x of 5); } catch (e) { return e.constructor.name } } var p = f()`, want: "TypeError"},
		{name: "forAwaitDestructure", async: true, src: `async function f() { let s = 0; for await (const [a, b] of [[1, 2], Promise.resolve([3, 4])]) s += a * b; return s } var p = f()`, want: "14"},
		{name: "forAwaitInAsyncGenerator", async: true, src: `async function* f(it) { for await (const x of it) yield x * 2 } async function main() { let s = ''; for await (const x of f([1, Promise.resolve(2)])) s += x; return s } var p = main()`, want: "24"},
		{name: "forAwaitNested", async: true, src: `async function* g(n) { for (let i = 0; i < n; i++) yield i } async function f() { let s = 0; for (let j = 0; j < 3; j++) for await (const x of g(4)) s += x * j; return s } var p = f()`, want: "18"},
		{name: "forAwaitAwaitInBody", async: true, src: `async function f() { let s = ''; for await (const x of ['a', 'b']) s += await (x + '!'); return s } var p = f()`, want: "a!b!"},
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
				r.Close()
				if on {
					assertTreeIn(t, fn, "f")
				}
			}
		})
	}
}

// TestTreeDelegateToGenerator pins what a yield* to a generator gives now
// that the tree steps the generator itself (see delegateNext), in both
// tiers, the interpreter calling the delegate's next: result objects of
// their own, in order and of Object.prototype; values sent on and returned
// through a chain of delegations; a result object a delegate hands over as
// it is, still the same object at the top of a chain; a next of the
// delegate's own, or %GeneratorPrototype%.next replaced, called as it is;
// the frames a stack trace in the delegate sees; a delegate already
// running; and return and throw after steps made directly.
func TestTreeDelegateToGenerator(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct{ name, src, want string }{
		{name: "results", src: `function* inner() { yield 1; return 2 } function* f() { var r = yield* inner(); yield r }
var g = f(), a = g.next(), b = g.next(), c = g.next(); [Object.keys(a).join(), Object.getPrototypeOf(a) === Object.prototype, a.value, a.done, a === b, b.value, b.done, c.value, c.done].join()`, want: "value,done,true,1,false,false,2,false,,true"},
		{name: "chain", src: `function* a(n) { var s = 0; for (var i = 0; i < n; i++) s += yield i; return s } function* b(n) { var r = yield* a(n); yield 'b' + r; return r * 2 } function* f(n) { var r = yield* b(n); return 'f' + r }
var g = f(3), out = [], r = g.next(); for (var k = 10; !r.done; k += 10) { out.push(r.value); r = g.next(k) } out.push(r.value); out.push([...f(2)].join()); out.join()`, want: "0,1,2,b60,f120,0,1,bNaN"},
		{name: "forOf", src: `function* inner(n) { for (var i = 0; i < n; i++) yield i } function* mid(n) { yield* inner(n); yield* inner(n) } function* f(n) { yield* mid(n) } var s = 0; for (var v of f(100)) s += v; s`, want: "9900"},
		{name: "rawThroughChain", src: `var res = { value: 1, done: false }, n = 0; var it = { [Symbol.iterator]() { return this }, next() { return n++ < 2 ? res : { value: 'end', done: true } } }; function* mid() { yield* it } function* f() { yield* mid() } var x = f().next(); String(x === res) + [...(function* () { yield* f() })()].length`, want: "true1"},
		{name: "ownNext", src: `function* inner() { yield 1 } var d = inner(); d.next = function (v) { return { value: 'own' + v, done: false } }; function* f() { yield* d } var g = f(); g.next(); g.next('x').value`, want: "ownx"},
		{name: "replacedNext", src: `function* inner() { yield 1; yield 2 } var GP = Object.getPrototypeOf(inner.prototype), orig = GP.next, log = []; GP.next = function (v) { log.push(v); return orig.call(this, v) }; function* f() { yield* inner() } var s = []; var g = f(); s.push(orig.call(g).value); s.push(orig.call(g, 'a').value); GP.next = orig; s.join() + ';' + log.join()`, want: "1,2;,a"},
		{name: "stackTrace", src: `function* inner() { yield new Error().stack.split('\n').slice(1, 5).map(l => l.trim().split(' ')[1]).join() } function* f() { yield* inner() } var r; for (var v of f()) r = v; r`, want: "inner,Object.next,f,Object.next"},
		{name: "running", src: `var g; function* f() { try { yield* g } catch (e) { yield e.message } } g = f(); g.next().value`, want: "the generator is already running"},
		{name: "returnAfterSteps", src: `var s = ''; function* inner() { try { for (var i = 0; ; i++) yield i } finally { s += 'I' } } function* f() { try { yield* inner() } finally { s += 'F' } } var g = f(); g.next(); g.next(); JSON.stringify(g.return(5)) + s`, want: `{"value":5,"done":true}IF`},
		{name: "throwAfterSteps", src: `function* inner() { for (var i = 0; ; i++) try { yield i } catch (e) { yield 'c' + e } } function* f() { yield* inner() } var g = f(); g.next(); g.next(); [g.throw('x').value, g.next().value].join()`, want: "cx,2"},
		{name: "doneDelegate", src: `function* inner() { return 'r' } function* f() { var x = yield* inner(); var y = yield* inner(); yield x + y } var g = f(); var a = g.next(); [a.value, a.done, g.next().done].join()`, want: "rr,false,true"},
		{name: "spreadArgs", src: `function* inner(a, b) { yield a; yield b } function* f() { yield* inner(1, 2); yield* inner(3) } Math.max(...f()) + ',' + [...f()].join()`, want: "NaN,1,2,3,"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				fn := compileForTest(t, tt.src)
				r := New(Config{})
				v, err := r.Run(fn)
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
				r.Close()
			}
		})
	}
}

// TestTreeDelegateToGeneratorRealm pins the realm of the result object a
// delegating generator hands to another realm's next: the object the
// delegate's next makes is of the delegate's next's realm, and one made for
// a value yield* stepped a generator to (see delegateNext) is of the same.
func TestTreeDelegateToGeneratorRealm(t *testing.T) {
	defer SetTreeTier(true)
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		r := New(Config{})
		other := r.NewRealm()
		next, err := r.RunIn(other, compileForTest(t, `Object.getPrototypeOf(function* () {}).prototype.next`))
		if err != nil {
			t.Fatal(err)
		}
		r.globalThis.Object().setOwnRaw(r.atoms.intern("otherNext"), next, propDefault)
		v, err := r.Run(compileForTest(t, `function* inner() { yield 1; yield 2 } function* f() { yield* inner() }
var g = f(), a = otherNext.call(g), b = otherNext.call(g), c = otherNext.call(g);
[Object.getPrototypeOf(a) === Object.prototype, a.value, Object.getPrototypeOf(b) === Object.prototype, b.value, Object.getPrototypeOf(c) === Object.prototype, c.done].join()`))
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", on, err)
		}
		got, err := r.ToString(v)
		if want := "true,1,true,2,false,true"; err != nil || got.Go() != want {
			t.Errorf("tree %v: got %v, %v; want %s", on, got, err, want)
		}
		r.Close()
	}
}
