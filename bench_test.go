package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Benchmarks for the interpreter rather than for the compiler.
//
// The ones that eval a source string measure parsing and compiling as much as
// running, which is the right measurement for a host that evaluates snippets
// and the wrong one for anything else. These compile once and then run the
// compiled function, so what they report is the interpreter's own cost.

// benchRun compiles a script that leaves a function in `f` and calls it.
func benchRun(b *testing.B, setup string) {
	b.Helper()
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(setup); err != nil {
		b.Fatal(err)
	}
	fn, err := rt.Get("f")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := fn.Call(); err != nil {
			b.Fatal(err)
		}
	}
}

// A counted loop with arithmetic, which is the interpreter's dispatch cost
// with as little else in the way as possible.
func BenchmarkLoopArithmetic(b *testing.B) {
	benchRun(b, `function f() {
		var t = 0
		for (var i = 0; i < 10000; i++) t += i * 2 - 1
		return t
	}`)
}

// Reading and writing properties of an ordinary object, which is the shape
// lookup and the property table.
func BenchmarkLoopPropertyAccess(b *testing.B) {
	benchRun(b, `var o = {a: 1, b: 2, c: 3}
	function f() {
		var t = 0
		for (var i = 0; i < 10000; i++) { o.a = i; t += o.a + o.b + o.c }
		return t
	}`)
}

// Indexing a dense array, which goes through the element storage rather than
// the property table.
func BenchmarkLoopArrayIndex(b *testing.B) {
	benchRun(b, `var a = new Array(1000)
	for (var i = 0; i < 1000; i++) a[i] = i
	function f() {
		var t = 0
		for (var i = 0; i < a.length; i++) t += a[i]
		return t
	}`)
}

// Calling a function in a loop: frame setup, argument binding and return.
func BenchmarkLoopFunctionCall(b *testing.B) {
	benchRun(b, `function add(a, b) { return a + b }
	function f() {
		var t = 0
		for (var i = 0; i < 10000; i++) t = add(t, i)
		return t
	}`)
}

// Calling a method, which resolves the callee through the prototype chain and
// binds a receiver.
func BenchmarkLoopMethodCall(b *testing.B) {
	benchRun(b, `class C { constructor() { this.n = 0 } bump(i) { this.n += i; return this.n } }
	var c = new C()
	function f() {
		var t = 0
		for (var i = 0; i < 10000; i++) t = c.bump(1)
		return t
	}`)
}

// Building objects, which is allocation and property definition.
func BenchmarkAllocObjects(b *testing.B) {
	benchRun(b, `function f() {
		var last
		for (var i = 0; i < 2000; i++) last = {a: i, b: i + 1, c: "x"}
		return last.a
	}`)
}

// Building arrays, the other allocation shape.
func BenchmarkAllocArrays(b *testing.B) {
	benchRun(b, `function f() {
		var last
		for (var i = 0; i < 2000; i++) last = [i, i + 1, i + 2]
		return last[0]
	}`)
}

// Concatenating strings, which is the rope and the conversion machinery.
func BenchmarkStringConcat(b *testing.B) {
	benchRun(b, `function f() {
		var s = ""
		for (var i = 0; i < 2000; i++) s += "ab"
		return s.length
	}`)
}

// Closures: one is created per iteration and called, which exercises upvalue
// capture.
func BenchmarkClosureCreateAndCall(b *testing.B) {
	benchRun(b, `function f() {
		var t = 0
		for (var i = 0; i < 5000; i++) { var g = function (x) { return x + i }; t = g(t) }
		return t
	}`)
}

// The array methods that take a callback, which is where a host's own code
// spends much of its time.
func BenchmarkArrayCallbacks(b *testing.B) {
	benchRun(b, `var a = new Array(1000)
	for (var i = 0; i < 1000; i++) a[i] = i
	function f() {
		return a.map(function (x) { return x * 2 })
		        .filter(function (x) { return x % 3 === 0 })
		        .reduce(function (t, x) { return t + x }, 0)
	}`)
}

// A Map, which is the hashed collection rather than the property table.
func BenchmarkMapOperations(b *testing.B) {
	benchRun(b, `function f() {
		var m = new Map()
		for (var i = 0; i < 2000; i++) m.set(i, i * 2)
		var t = 0
		for (var i = 0; i < 2000; i++) t += m.get(i)
		return t
	}`)
}

// A regular expression matched repeatedly, compiled once.
func BenchmarkRegExpExec(b *testing.B) {
	benchRun(b, `var re = /(\w+)@(\w+)\.com/
	var s = "write to someone@example.com today"
	function f() {
		var n = 0
		for (var i = 0; i < 1000; i++) n += re.exec(s)[1].length
		return n
	}`)
}

// Exceptions, thrown and caught in a loop.
func BenchmarkThrowCatch(b *testing.B) {
	benchRun(b, `function f() {
		var n = 0
		for (var i = 0; i < 2000; i++) {
			try { throw new Error("x") } catch (e) { n += e.message.length }
		}
		return n
	}`)
}

// JSON, which is the parser and the serializer rather than the interpreter.
func BenchmarkJSONRoundTrip(b *testing.B) {
	benchRun(b, `var obj = {name: "x", list: [1, 2, 3, 4, 5], nested: {a: true, b: null}}
	function f() {
		var n = 0
		for (var i = 0; i < 500; i++) n += JSON.parse(JSON.stringify(obj)).list.length
		return n
	}`)
}

// Compiling: what a host pays before anything runs.
func BenchmarkCompileScript(b *testing.B) {
	// Everything is inside a function so that the script declares nothing and
	// can be evaluated again and again in one runtime.
	const src = `(function () {
		class Point {
			constructor(x, y) { this.x = x; this.y = y }
			add(o) { return new Point(this.x + o.x, this.y + o.y) }
			get length() { return Math.sqrt(this.x * this.x + this.y * this.y) }
		}
		function sum(points) {
			return points.reduce((acc, p) => acc.add(p), new Point(0, 0))
		}
		var pts = []
		for (let i = 0; i < 10; i++) pts.push(new Point(i, i * 2))
		return sum(pts).length
	})()`
	rt := quickjs.New()
	defer rt.Close()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := rt.Eval(src); err != nil {
			b.Fatal(err)
		}
	}
}

// Creating a runtime, which a host that isolates each request pays per
// request.
func BenchmarkNewRuntime(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		rt := quickjs.New()
		rt.Close()
	}
}

// Creating a runtime with a small stack, which is what a host that makes many
// of them and runs shallow code should ask for.
func BenchmarkNewRuntimeSmallStack(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		rt := quickjs.New(quickjs.WithStackSize(8 * 1024))
		rt.Close()
	}
}
