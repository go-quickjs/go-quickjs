package vm

import "testing"

// TestTreeGenerators pins that generators, async functions and async
// generators give in the tree tier what they give in the interpreter, and
// run as trees: values sent in, a yield or an await inside an expression,
// return and throw injected at a suspension -- caught, uncaught, through
// finally clauses and loops over iterators -- exceptions the body throws
// after a resumption, closures made between suspensions, parameter
// prologues, rejections, and a script's jobs run to the end. The empty
// loops some cases begin f's body with are from when only a body with a
// loop was given a tree. An async case
// leaves its promise in p, and out is what that settles to: its value, or
// E and what it threw.
func TestTreeGenerators(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
		// async says the script settles out once its jobs have run.
		async bool
	}{
		{name: "values", src: `function* f(n) { for (let i = 0; i < n; i++) yield i * 2 } [...f(4)].join()`, want: "0,2,4,6"},
		{name: "sent", src: `function* f() { for (var k = 0; k < 1; k++); let a = yield 1; let b = yield a + 1; return a + b } var g = f(); g.next(); g.next(10).value + ',' + g.next(5).value + ',' + g.next().done`, want: "11,15,true"},
		{name: "midExpression", src: `function* f() { for (var k = 0; k < 1; k++); var x = 1; return x + (yield 2) * 3 } var g = f(); g.next().value + ',' + g.next(4).value`, want: "2,13"},
		{name: "busyBody", src: `function* f(n) { let s = 0; for (let i = 0; i < n; i++) { s = (s + i * 3) | 0; if ((i & 255) == 0) yield s } return s } var t = 0, r; for (var g = f(5000), x = g.next(); !x.done; x = g.next()) t ^= x.value; t + ',' + x.value`, want: "65674752,37492500"},
		{name: "return", src: `var s = ''; function* f() { try { for (let i = 0; ; i++) yield i } finally { s += 'F' } } var g = f(); g.next(); g.next(); g.return(7).value + s + g.next().done`, want: "7Ftrue"},
		{name: "returnOverridden", src: `function* f() { for (var k = 0; k < 1; k++); try { yield 1 } finally { return 9 } } var g = f(); g.next(); g.return(5).value`, want: "9"},
		{name: "returnYieldsInFinally", src: `function* f() { for (var k = 0; k < 1; k++); try { yield 1 } finally { yield 2 } } var g = f(); g.next(); var a = g.return(5); var b = g.next(); a.value + ',' + a.done + ',' + b.value + ',' + b.done`, want: "2,false,5,true"},
		{name: "throwCaught", src: `function* f() { while (true) { try { yield 1 } catch (e) { yield 'c' + e } } } var g = f(); g.next(); g.throw('x').value + ',' + g.next().value`, want: "cx,1"},
		{name: "throwUncaught", src: `function* f() { for (var k = 0; k < 1; k++); yield 1 } var g = f(), r; g.next(); try { g.throw('x') } catch (e) { r = e } r + g.next().done`, want: "xtrue"},
		{name: "throwBeforeStart", src: `function* f() { for (var k = 0; k < 1; k++); yield 1 } var g = f(), r; try { g.throw('x') } catch (e) { r = e } r + g.next().done + f().next().value`, want: "xtrue1"},
		{name: "throwAfterResume", src: `function* f() { for (var k = 0; k < 1; k++); yield 1; null.x } var g = f(), r; g.next(); try { g.next() } catch (e) { r = e.name } r + g.next().done`, want: "TypeErrortrue"},
		{name: "forOfClosed", src: `var s = ''; function* inner() { try { yield 1; yield 2 } finally { s += 'I' } } function* f() { for (const x of inner()) yield x * 10 } var g = f(); g.next().value + ',' + g.return(0).value + s`, want: "10,0I"},
		{name: "forOfThrown", src: `var s = ''; function* inner() { try { yield 1; yield 2 } finally { s += 'I' } } function* f() { for (const x of inner()) yield x * 10 } var g = f(), r; g.next(); try { g.throw('t') } catch (e) { r = e } r + s`, want: "tI"},
		{name: "closures", src: `function* f() { const fs = []; for (let i = 0; i < 3; i++) { fs.push(() => i); yield } return fs.map(h => h()).join() } var g = f(); g.next(); g.next(); g.next(); g.next().value`, want: "0,1,2"},
		{name: "defaults", src: `function* f(a, b = a + 1) { for (var k = 0; k < 1; k++); yield a + b } f(1).next().value`, want: "3"},
		{name: "arguments", src: `function* f() { for (var k = 0; k < 1; k++); yield arguments.length + arguments[1] } f(1, 2).next().value`, want: "4"},
		{name: "method", src: `var o = { v: 5, *f() { for (var k = 0; k < 1; k++); yield this.v } }; o.f().next().value`, want: "5"},
		{name: "running", src: `var g; function* f() { for (var k = 0; k < 1; k++); try { g.next() } catch (e) { yield e.name } } g = f(); g.next().value`, want: "TypeError"},
		{name: "await", async: true, src: `async function f(n) { let s = 0; for (let i = 0; i < n; i++) s += await i; return s } var p = f(10)`, want: "45"},
		{name: "awaitMidExpression", async: true, src: `async function f() { for (var k = 0; k < 1; k++); var x = 1; return x + await 2 * 3 } var p = f()`, want: "7"},
		{name: "awaitRejectionCaught", async: true, src: `async function f() { for (var k = 0; k < 1; k++); try { await Promise.reject('r') } catch (e) { return 'c' + e } } var p = f()`, want: "cr"},
		{name: "awaitThenThrow", async: true, src: `async function f() { for (var k = 0; k < 1; k++); await 1; throw 'x' } var p = f()`, want: "E x"},
		{name: "awaitFinally", async: true, src: `var s = ''; async function f() { for (var k = 0; k < 1; k++); try { await null; s += 'a'; return 'r' } finally { s += 'f' } } var p = f().then(v => v + s)`, want: "raf"},
		{name: "awaitForOf", async: true, src: `async function f() { let s = 0; for (const x of [1, 2, 3]) s += await x; return s } var p = f()`, want: "6"},
		{name: "awaitBusyBody", async: true, src: `async function f(n) { let s = 0; for (let i = 0; i < n; i++) { s = (s + i * 3) | 0; if ((i & 255) == 0) await null } return s } var p = f(5000)`, want: "37492500"},
		{name: "awaitRejectedInForOf", async: true, src: `var s = ''; function* inner() { try { yield 1; yield 2 } finally { s += 'I' } } async function f() { for (const x of inner()) await Promise.reject('r' + x) } var p = f().catch(e => e + s)`, want: "r1I"},
		{name: "asyncThis", async: true, src: `class A { constructor() { this.v = 3 } async f() { for (var k = 0; k < 1; k++); await 0; return this.v } } var p = new A().f()`, want: "3"},
		{name: "asyncGenerator", async: true, src: `async function* f() { for (let i = 0; i < 3; i++) yield i } async function main() { let s = ''; for await (const x of f()) s += x; return s } var p = main()`, want: "012"},
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

// TestTreeResumedLoopStructured pins that a loop a resumption goes on into
// is structured from its own header, as a block of its own: a resumption
// enters the loop partway round, and the loop rebuilt as part of the
// function's first block was run from block to block after the first
// suspension, every turn of it.
func TestTreeResumedLoopStructured(t *testing.T) {
	if !treeTier.Load() {
		t.Skip("the tree tier is off")
	}
	for _, src := range []string{
		`function* f(n) { var i, s = 0; for (i = 0; i < n; i++) { s = (s + i * 3) | 0; if ((i & 255) == 0) yield s } return s }`,
		`async function f(n) { var i, s = 0; for (i = 0; i < n; i++) { s = (s + i * 3) | 0; if ((i & 255) == 0) await null } return s }`,
		`function* f(n) { var s = 0; for (var j = 0; j < n; j++) { for (var i = 0; i < n; i++) s += i; yield s } }`,
	} {
		headers := 0
		flowSeen = func(f *flow, _ *tree) {
			if f.entry == 0 {
				return
			}
			for _, nd := range f.nodes {
				if !nd.dead && nd.changed {
					headers++
					break
				}
			}
			for i, nd := range f.nodes {
				if !nd.dead && (nd.level >= 0 || has(nd.succ, i)) {
					t.Errorf("%s: block %d, in the loop at %d, is left to dispatch", src, i, nd.level)
				}
			}
		}
		tr := buildTree(flowFunc(t, src))
		flowSeen = nil
		if tr == nil || tr.gen == nil {
			t.Errorf("%s: not built", src)
		} else if headers != 1 {
			t.Errorf("%s: %d parts rebuilt from a resumption's loop header, want 1", src, headers)
		}
	}
}

// TestTreeResumedLoops pins what loops a generator or an async function is
// suspended in do once resumed, in both tiers, now that a loop a
// resumption goes on into is a structure of its own: a yield in an inner
// loop, in an outer loop around a busy inner one, in a loop's test, in a
// do-while, with labelled break and continue, in a try inside a loop, and
// an await in a loop of a loop.
func TestTreeResumedLoops(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
		async           bool
	}{
		{name: "inner", src: `function* f(n) { for (var j = 0; j < n; j++) for (var i = 0; i < j; i++) yield j * 10 + i } [...f(4)].join()`, want: "10,20,21,30,31,32"},
		{name: "outer", src: `function* f(n) { var s = 0; for (var j = 0; j < n; j++) { for (var i = 0; i < 100; i++) s += i; yield s } return -s } var g = f(3), a = []; for (var r = g.next(); !r.done; r = g.next()) a.push(r.value); a.push(r.value); a.join()`, want: "4950,9900,14850,-14850"},
		{name: "inTest", src: `function* f() { var n = 0; while ((yield n) < 3) n++; return n } var g = f(), a = [g.next().value]; for (var k = 0; k < 5; k++) { var r = g.next(k); a.push(r.value, r.done) } a.join()`, want: "0,1,false,2,false,3,false,3,true,,true"},
		{name: "doWhile", src: `function* f(n) { var i = 0; do { yield i; i += 2 } while (i < n) } [...f(7)].join()`, want: "0,2,4,6"},
		{name: "labels", src: `function* f() { outer: for (var j = 0; j < 4; j++) { for (var i = 0; i < 4; i++) { if (i == 2) continue outer; if (j == 3) break outer; yield j + '' + i } } yield 'end' } [...f()].join()`, want: "00,01,10,11,20,21,end"},
		{name: "tryInLoop", src: `function* f(n) { var s = ''; for (var i = 0; i < n; i++) { try { if (i == 1) throw 'x'; yield i } catch (e) { s += e } finally { s += 'f' } } return s } var g = f(3), a = []; for (var r = g.next(); !r.done; r = g.next()) a.push(r.value); a.push(r.value); a.join()`, want: "0,2,fxff"},
		{name: "sentValues", src: `function* f() { var s = 0; for (var i = 0; i < 3; i++) s += yield i; return s } var g = f(); g.next(); g.next(10); g.next(20); g.next(30).value`, want: "60"},
		{name: "awaitNested", async: true, src: `async function f(n) { var s = 0; for (var j = 0; j < n; j++) for (var i = 0; i < n; i++) { s += i * j; if (i == j) await null } return s } var p = f(10)`, want: "2025"},
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
				r.Close()
				if on {
					assertTreeIn(t, fn, "f")
				}
			}
		})
	}
}
