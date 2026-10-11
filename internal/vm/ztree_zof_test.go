package vm

import "testing"

// TestTreeForOf pins that for...of and for...in loops, and destructuring
// patterns, give in the tree tier what they give in the interpreter, and run
// as trees: what they iterate -- arrays, strings, Sets, Maps, generators,
// iterators of the script's own -- and how a loop is left, which closes its
// iterator, or does not: running to its end, a break, a continue of an outer
// loop, a return, and an exception thrown in the function or in a call,
// caught in the loop, outside it or not at all; and a return method that
// throws or answers what is not an object.
func TestTreeForOf(t *testing.T) {
	defer SetTreeTier(true)
	// log is an iterable that says, in s, what was asked of it: n values,
	// and whether it was closed. ret, where set, is its return method.
	const log = `var s = ''; function it(n, ret) { var i = 0; var o = { next() { return i < n ? { value: i++, done: false } : { value: undefined, done: true } } }; o.return = ret || function () { s += 'R'; return {} }; return { [Symbol.iterator]() { return o } } } `
	const gen = `var s = ''; function* gen(n) { try { for (var i = 0; i < n; i++) yield i } finally { s += 'F' } } `
	for _, tt := range []struct {
		name, src, want string
	}{
		{name: "array", src: `function f(a) { let s = 0; for (const x of a) s += x; return s } f([1, 2, 3, 4])`, want: "10"},
		{name: "arrayGrows", src: `function f(a) { let s = 0; for (const x of a) { if (a.length < 6) a.push(x * 10); s += x } return s } f([1, 2, 3])`, want: "66"},
		{name: "holes", src: `function f(a) { let s = ''; for (const x of a) s += x + ','; return s } f([1, , 3])`, want: "1,undefined,3,"},
		{name: "string", src: `function f(t) { let s = ''; for (const c of t) s = c + s; return s } f('ab\u{1F600}c')`, want: "c\U0001F600ba"},
		{name: "set", src: `function f() { let s = 0; for (const x of new Set([1, 2, 2, 3])) s += x; return s } f()`, want: "6"},
		{name: "mapEntries", src: `function f() { let s = ''; for (const [k, v] of new Map([['a', 1], ['b', 2]])) s += k + v; return s } f()`, want: "a1b2"},
		{name: "destructuringRest", src: `function f() { const [a, ...r] = [1, 2, 3]; let [x, y] = 'hi'; return a + ':' + r.join() + ':' + x + y } f()`, want: "1:2,3:hi"},
		{name: "destructuringCloses", src: gen + `function f() { const [a, b] = gen(5); return a + b + s } f()`, want: "1F"},
		{name: "objectOwnIterator", src: log + `function f() { let t = 0; for (const x of it(4)) t += x; return t + s } f()`, want: "6"},
		{name: "generator", src: gen + `function f() { let t = 0; for (const x of gen(4)) t += x; return t + s } f()`, want: "6F"},
		{name: "break", src: gen + `function f() { let t = 0; for (const x of gen(10)) { if (x == 3) break; t += x } return t + s } f()`, want: "3F"},
		{name: "breakOwnIterator", src: log + `function f() { for (const x of it(10)) if (x == 2) break; return s } f()`, want: "R"},
		{name: "return", src: gen + `function f() { for (const x of gen(10)) if (x == 3) return x; } f() + s`, want: "3F"},
		{name: "continueOuter", src: log + `function f() { let t = ''; outer: for (const x of [1, 2]) { for (const y of it(5)) { if (y == 1) continue outer; t += y } } return t + s } f()`, want: "00RR"},
		{name: "throwUncaught", src: gen + `function f() { for (const x of gen(10)) if (x == 2) throw 'x' + x } var r; try { f() } catch (e) { r = e } r + s`, want: "x2F"},
		{name: "throwCaughtOutside", src: gen + `function f() { try { for (const x of gen(10)) if (x == 2) throw 'x' + x } catch (e) { return e + s } } f()`, want: "x2F"},
		{name: "throwCaughtInside", src: gen + `function f() { let t = ''; for (const x of gen(4)) { try { if (x == 2) throw 'x' } catch (e) { t += e } t += x } return t + s } f()`, want: "01x23F"},
		{name: "throwFromCall", src: gen + `function g(x) { if (x == 2) throw 'g' + x } function f() { for (const x of gen(10)) g(x) } var r; try { f() } catch (e) { r = e } r + s`, want: "g2F"},
		{name: "throwFromCallCaught", src: gen + `function g(x) { if (x == 2) throw 'g' + x } function f() { try { for (const x of gen(10)) g(x) } catch (e) { return e + s } } f()`, want: "g2F"},
		{name: "throwNested", src: gen + `function f() { try { for (const x of gen(3)) for (const y of gen(3)) if (y == 1) throw x } catch (e) { return e + s } } f()`, want: "0FF"},
		{name: "throwInFinallyLoop", src: gen + `function f() { let t = ''; try { for (const x of gen(3)) { try { t += x } finally { if (x == 1) throw 'e' } } } catch (e) { t += e } return t + s } f()`, want: "01eF"},
		{name: "returnThrowsOnBreak", src: log + `function f() { try { for (const x of it(5, function () { throw 'R!' })) break } catch (e) { return e } } f()`, want: "R!"},
		{name: "returnNotObject", src: log + `function f() { try { for (const x of it(5, function () { return 1 })) break } catch (e) { return e.name } } f()`, want: "TypeError"},
		{name: "returnThrowsOnThrow", src: log + `function f() { try { for (const x of it(5, function () { throw 'R!' })) throw 'body' } catch (e) { return e } } f()`, want: "body"},
		{name: "nextThrows", src: `var s = ''; var it = { [Symbol.iterator]() { return { next() { throw 'N' }, return() { s += 'R'; return {} } } } }; function f() { try { for (const x of it) ; } catch (e) { return e + s } } f()`, want: "N"},
		{name: "notIterable", src: `function f(o) { try { for (const x of o) ; } catch (e) { return e.name } } f({})`, want: "TypeError"},
		{name: "closures", src: `function f() { const a = []; for (const x of [1, 2, 3]) a.push(function () { return x }); var p = a[0], q = a[1], r = a[2]; return p() + q() + r() } f()`, want: "6"},
		{name: "forIn", src: `function f(o) { let s = ''; for (const k in o) s += k; return s } f({ a: 1, b: 2, [3]: 3 })`, want: "3ab"},
		{name: "forInDeletes", src: `function g(o) { delete o.b } function f(o) { let s = ''; for (const k in o) { g(o); s += k } return s } f({ a: 1, b: 2, c: 3 })`, want: "ac"},
		{name: "forInBreak", src: `function f(o) { for (const k in o) if (k == 'b') return k; } f({ a: 1, b: 2, c: 3 })`, want: "b"},
		{name: "loopCalls", src: `function g(x) { return x * 2 } function f(a) { let s = 0; for (const x of a) s += g(x); return s } var a = []; for (var i = 0; i < 100; i++) a.push(i); f(a)`, want: "9900"},
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
				if r.frameDepth != 0 || r.stackTop != 0 {
					t.Errorf("tree %v: left %d frames and %d stack slots", on, r.frameDepth, r.stackTop)
				}
				r.Close()
				if on {
					assertExceptionTree(t, fn, "f")
				}
			}
		})
	}
}

// TestTreeForOfStaleCursor pins that an exception closes only the iterators
// of the function's own loops. The stack above the live frames keeps what
// was there: a suspended generator's cursor, among others, which the
// generator still steps. A tree does not know how deep its stack was where
// it threw, and looks in all of its slots, so they are cleared when it is
// entered: here every slot above the caller is such a cursor. f throws
// before its loop goes back, where the interrupt check may sweep the slots
// above its stack itself.
func TestTreeForOfStaleCursor(t *testing.T) {
	defer SetTreeTier(true)
	const src = `var s = ''; function* gen(n) { try { for (var i = 0; i < n; i++) yield i } finally { s += 'F' } }
function f() { for (const y of [0]) throw 'f'; return [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12] }
var g = gen(5); g.next(); plant(g, f) + s + g.next().value + s`
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		fn := compileForTest(t, src)
		r := New(Config{})
		plant := r.newNativeFunc("plant", 2, func(rt *Runtime, _ Value, args []Value) (Value, error) {
			cur, err := rt.startForOf(args[0])
			if err != nil {
				return Undefined, err
			}
			for i := rt.stackTop; i < rt.stackTop+64; i++ {
				rt.stack[i] = cur
			}
			if _, err := rt.call(args[1], Undefined, nil); err != nil {
				if th, ok := err.(*Thrown); ok {
					return th.Value, nil
				}
				return Undefined, err
			}
			return Undefined, nil
		})
		r.globalThis.Object().setOwnRaw(r.atoms.intern("plant"), Obj(plant), propDefault)
		v, err := r.Run(fn)
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", on, err)
		}
		got, err := r.ToString(v)
		if err != nil || got.Go() != "f1" {
			t.Errorf("tree %v: got %v, %v; want f1", on, got, err)
		}
		r.Close()
		if on {
			assertExceptionTree(t, fn, "f")
		}
	}
}
