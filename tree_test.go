package quickjs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// treeScripts are run with the tree tier and without it, and must give the
// same answers: each value, each error and each stack trace. Between them
// they run every instruction the tier builds, and the slow path behind each
// fast one -- a valueOf, a getter, a proxy, a hole, a BigInt, a string, an
// exception from inside an expression -- and values left on the stack across
// blocks, by ?:, && and ||.
var treeScripts = []string{
	// Arithmetic, bitwise and comparison operators over numbers.
	`function f(n) { var r = []; for (var i = -3; i < n; i++) { var x = i * 1.5, y = i - 2;
	   r.push(x + y, x - y, x * y, x / y, x % y, -x, i & 5, i | 9, i ^ 3, i << 3, i >> 1, i >>> 1,
	     i < y, i <= y, i > y, i >= y, i == y, i != y, i === y, i !== y, 1 / -0 * i, i % 0) } return r.join() } f(5)`,
	`function f() { var r = []; for (var i = 0; i < 4; i++) { var x = 2147483647 + i, y = -2147483648 - i;
	   r.push(x | 0, y | 0, x >>> 0, y >>> 0, x << 1, y >> 33, x & y, ~~x, x ^ -1, 1e21 | 0, NaN | 0) } return r.join() } f()`,
	// Strings, objects with valueOf, and the order they are converted in.
	`function f() { var log = [], s = ""; var o = { valueOf() { log.push("v" + s.length); return 2 } };
	   for (var i = 0; i < 3; i++) { s = s + i + "-"; var t = o * i + o - i / o; log.push(t, "a" < "b" + i, "10" < 9 + i, o > i) }
	   return s + " " + log.join() } f()`,
	`function f() { var r = []; var a = { valueOf() { r.push("a"); return 1 } }, b = { valueOf() { r.push("b"); return 2 } };
	   for (var i = 0; i < 2; i++) { r.push(a + b, a - b, a < b, a == 1, b != "2", a | b, a << b) } return r.join() } f()`,
	// BigInt, ++ and -- on what is not a number.
	`function f() { var r = [], b = 10n, s = "5", u, o = { valueOf() { return 7 } };
	   for (var i = 0; i < 3; i++) { b++; r.push(b, b * 3n, -b, b % 4n); s++; u--; var p = o++; r.push(s, u, p, o, typeof o) }
	   return r.join() } function g(b) { for (var i = 0; i < 1; i++) b = b + 1; return b }
	 f() + " " + (function () { try { return g(1n) } catch (e) { return e.constructor.name } })()`,
	`function f() { var r = []; var a = [1, 2, 3], i = 0, s = "1"; for (var k = 0; k < 3; k++) { r.push(a[i++], a[++i - 1], a[s++]) } return r.join() } f()`,
	// Array reads and writes: holes, a getter and a setter up the chain,
	// bounds, odd keys, typed arrays, frozen arrays.
	`function f() { Object.defineProperty(Array.prototype, 3, { get() { return "proto3" }, set(v) { log.push("set" + v) }, configurable: true });
	   var log = [], a = [0, , 2], r = []; for (var i = -1; i < 6; i++) { r.push(a[i], a[i + 0.5], a["" + i]); a[i] = i * 2 }
	   delete Array.prototype[3]; return r.join() + " " + a.join() + " " + log.join() } f()`,
	`function f() { var t = new Float64Array(4), u = new Uint8Array(3), r = []; for (var i = 0; i < 5; i++) { t[i] = i / 3; u[i] = i * 100; r.push(t[i], u[i]) } return r.join() } f()`,
	`"use strict"; function f(a, i) { for (var k = 0; k < 1; k++) a[i] = 9 } var a = Object.freeze([1, 2]), r = [];
	 for (var i = 0; i < 2; i++) { try { f(a, i) } catch (e) { r.push(e.constructor.name + ": " + e.message) } } r.join() + a`,
	`function f() { var a = Object.freeze([1, 2]); for (var i = 0; i < 2; i++) a[i] = 9; return a.join() } f()`,
	// Properties: getters, setters, proxies, the chain, methods and new.
	`function f() { var log = [], p = { get x() { log.push("get"); return 1 }, set x(v) { log.push("set" + v) } };
	   var q = new Proxy({}, { get(t, k) { log.push("pget " + String(k)); return 5 }, set(t, k, v) { log.push("pset " + String(k) + v); return true } });
	   var s = 0; for (var i = 0; i < 2; i++) { s += p.x + q.y; p.x = i; q.z = i; s += Object.create(p).x } return s + " " + log.join() } f()`,
	`function P(v) { this.v = v } P.prototype.get = function () { return this.v };
	 function f() { var s = 0, o; for (var i = 0; i < 4; i++) { o = new P(i); s += o.get() * 10 + o.v } return s } f()`,
	`function f() { var o = { n: 0, inc() { return ++this.n } }, s = 0; for (var i = 0; i < 5; i++) { s += o.inc(); o.m = o.n } return s + "," + o.m } f()`,
	// Globals: declared, lexical, in their dead zone, undeclared, and
	// assignment to them, sloppy and strict.
	`var gv = 1; let gl = 2; const gc = 3; function f() { var s = 0; for (var i = 0; i < 3; i++) { s += gv + gl + gc; gv++; gl += 2 } return s + " " + gv + " " + gl }
	 function g() { for (var i = 0; i < 1; i++) gc = 4 } f() + " " + (function () { try { g() } catch (e) { return e.constructor.name + ": " + e.message } })()`,
	`function f() { var r = []; for (var i = 0; i < 2; i++) { newGlobal = i; r.push(newGlobal) } return r.join() }
	 function g() { for (var i = 0; i < 1; i++) return undeclared + 1 }
	 f() + " " + newGlobal + " " + (function () { try { g() } catch (e) { return e.constructor.name + ": " + e.message } })()`,
	`"use strict"; function f() { for (var i = 0; i < 2; i++) strictUndeclared = i } try { f() } catch (e) { e.constructor.name + ": " + e.message }`,
	`function f() { var r = []; for (var i = 0; i < 2; i++) r.push(tdz); return r } var res; try { f() } catch (e) { res = e.constructor.name + ": " + e.message } let tdz = 1; res + " " + f()`,
	// Upvalues, closures made in a loop, recursion.
	`function outer() { var a = 1, fs = []; function f() { for (var i = 0; i < 4; i++) { a = a * 2 + i; fs.push(function () { return a + i }) } return a }
	   var r = f(); return r + " " + fs.map(g => g()).join() } outer()`,
	`function fib(n) { var a = 0, b = 1; for (var i = 0; i < n; i++) { var t = a + b; a = b; b = t } return n < 2 ? n : fib(n - 1) + a - fib(n - 1) + b - b } fib(12)`,
	// Values carried from block to block: ?:, &&, ||, nested assignments.
	`function f() { var s = 0, a = [0, 0, 0], b = [0, 0, 0], o = {}, q = {}; for (var i = 0; i < 6; i++) {
	   s += (i % 2 ? i : -i) + (i && 3 || 4) + (i > 2 ? (i > 4 ? 100 : 10) : 1);
	   var v = a[i % 3] = b[(i + 1) % 3] = i * 2; o.p = q.r = v; s += o.p + q.r + v } return s + " " + a + " " + b } f()`,
	`function f() { var r = []; for (var i = 0; i < 4; i++) { var x = i === 0 ? null : i === 1 ? undefined : i; r.push(x || "dflt", x && x * 2, !x, !!x) } return r.join() } f()`,
	// Calls in the middle of an expression that change what the rest reads.
	`function f() { var h = [1, 2, 3], s = 0; function g(i) { h[i] = h[i] * 10; return i } for (var i = 0; i < 3; i++) { s += h[i] + g(i) * h[i] + h[i] } return s + " " + h } f()`,
	`function f() { var x = 1, r = []; for (var i = 0; i < 3; i++) { r.push(x + (x = x + 1) + x, x++ + x, x + x++) } return r.join() } f()`,
	// Operands read in place, a local's or an upvalue's: each is read where
	// the operator's own operand would have been, before what follows it
	// can change it.
	`function f() { var x = 1, y = 2.5, a = [5, 6, 7, 8], r = []; for (var i = 0; i < 3; i++) {
	   r.push(x * x++, x - (x = 10), y / (y = 4), (x = 3) - x, x + 0.5, 0.5 - x, 2 * y, x * y, a[i] + a[x - i], a[i++] + a[i]) } return r.join() } f()`,
	`function f() { var a = [0, 0, 0], i = 0, r = []; for (var n = 0; n < 2; n++) { a[i] = (i = 2); r.push(a.join(), i); a[i - 1] = i++ } return r.join("|") + " " + a } f()`,
	`function outer() { var lim = 3, u = 1.5; function f() { var r = []; for (var i = 0; i < lim; i++) { if (u < i) r.push("u" + i); if (i >= u) r.push("ge"); r.push(i) } lim = 1; return r.join() }
	   return f() + " " + f() } outer()`,
	`function f() { var n = 0; for (var i = 0; i < i++ + 1 && n < 5;) n++; var r = [n, i];
	   for (var j = 0; j < NaN; j++) r.push("nan"); for (var s = "a"; s < "aaa"; s += "a") r.push(s);
	   for (var k = 0; k <= "2"; k++) r.push(k); var o = { valueOf() { r.push("v"); return 1 } };
	   for (var m = 0; m < o; m++) r.push("m"); for (var p = 3; p > 1n; p--) r.push(p); return r.join() } f()`,
	`function f(a) { var s = "abc", r = ""; for (var i = -1; i < 4; i++) { r += a[i] + "/" + s[i] + "/" + a[i + 1] + ";"; a[i] = i } return r + " " + a } f([1, , 3])`,
	// Labelled loops, break, continue, switch, returns from inside a loop.
	`function f(n) { var r = []; outer: for (var i = 0; i < n; i++) { for (var j = 0; j < n; j++) { if (j > i) continue outer; if (i + j > 5) break outer;
	   switch (j % 3) { case 0: r.push("z"); break; case 1: r.push(i); default: r.push(j) } } } return r.join() } f(5)`,
	`function f(a) { for (var i = 0; i < a.length; i++) { if (a[i] < 0) return i; while (a[i] > 10) a[i] -= 7 } return -1 } f([3, 20, 15, -1, 2]) + "," + f([1])`,
	// this, in a method and in a sloppy function.
	`function f() { var s = 0; for (var i = 0; i < 3; i++) s += this.k * i; return s } f.call({ k: 7 }) + "," + (function () { var c = 0; for (var i = 0; i < 2; i++) c += this === globalThis; return c })()`,
	// Exceptions from inside an expression, and their stacks.
	`function f(a) { var s = 0; for (var i = 0; i < a.length; i++) { s += a[i].x.y } return s }
	 try { f([{ x: { y: 1 } }, { x: null }]) } catch (e) { e.constructor.name + " " + e.message + "|" + e.stack.split("\n").slice(0, 3).join("|") }`,
	`function g(v) { if (v > 1) throw new RangeError("big " + v); return v }
	 function f() { var s = 0; for (var i = 0; i < 5; i++) s += g(i) * 2; return s }
	 try { f() } catch (e) { e.message + "|" + e.stack.split("\n").slice(0, 3).join("|") }`,
	`function f() { var o = { valueOf() { throw new TypeError("no") } }; var s = 0; for (var i = 0; i < 2; i++) s += i * o; return s }
	 try { f() } catch (e) { e.message + "|" + e.stack.split("\n").slice(0, 3).join("|") }`,
	`function f(o) { for (var i = 0; i < 2; i++) o.p.q = i } try { f({}) } catch (e) { e.message + "|" + e.stack.split("\n").slice(0, 2).join("|") }`,
}

// treeRun evaluates a script and gives its value, or its error.
func treeRun(t *testing.T, src string) string {
	t.Helper()
	rt := quickjs.New()
	defer rt.Close()
	v, err := rt.Eval(src)
	if err != nil {
		return "error: " + err.Error()
	}
	return v.String()
}

// TestTreeTierMatchesInterpreter runs each of treeScripts with every
// function the tree tier can build built, and compares it with the
// interpreter's run.
func TestTreeTierMatchesInterpreter(t *testing.T) {
	defer vm.SetTreeTier(true, false)
	for _, src := range treeScripts {
		vm.SetTreeTier(false, false)
		want := treeRun(t, src)
		vm.SetTreeTier(true, true)
		before := vm.TreesBuilt()
		got := treeRun(t, src)
		if got != want {
			t.Errorf("%s\n tree: %s\ninterp: %s", src, got, want)
		}
		if vm.TreesBuilt() == before {
			t.Errorf("%s\nno function was built as a tree", src)
		}
	}
}

// TestTreeTierInterrupted stops a loop running as a tree, as one running in
// the interpreter is stopped.
func TestTreeTierInterrupted(t *testing.T) {
	defer vm.SetTreeTier(true, false)
	vm.SetTreeTier(true, true)
	rt := quickjs.New()
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	before := vm.TreesBuilt()
	_, err := rt.EvalContext(ctx, `function spin() { var n = 0; for (;;) { n = n + 1 } } spin()`)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "interrupt") {
		t.Fatalf("got %v, want the deadline", err)
	}
	if vm.TreesBuilt() == before {
		t.Fatal("spin was not built as a tree")
	}
}

// TestTreeTierSharedAcrossRuntimes runs one compiled program in runtimes on
// several goroutines at once. They share its functions, and so the trees
// built for them, which the first to run each one builds.
func TestTreeTierSharedAcrossRuntimes(t *testing.T) {
	defer vm.SetTreeTier(true, false)
	vm.SetTreeTier(true, true)
	p, err := quickjs.Compile("shared.js", `function f(n) { var s = 0, a = []; for (var i = 0; i < n; i++) { a.push(i * 2); s += a[i] % 7 } return s } f(2000)`)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	results := make(chan string, workers)
	for w := 0; w < workers; w++ {
		go func() {
			rt := quickjs.New()
			defer rt.Close()
			v, err := rt.RunProgram(p)
			if err != nil {
				results <- err.Error()
				return
			}
			results <- v.String()
		}()
	}
	for w := 0; w < workers; w++ {
		if got := <-results; got != "5998" {
			t.Errorf("got %s, want 5998", got)
		}
	}
}
