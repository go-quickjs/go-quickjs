//go:build amd64 || arm64

package vm

import (
	"sync/atomic"
	"testing"
)

const nativeGetterSource = `
function Box(v) { this.v = v }
Box.prototype.get = function () { return this.v };
Box.prototype.size = function () { return this.v.length };
Box.prototype.at = function (i) { return this.v[i] };
function getters(n) {
	var a = new Box([1, 2, 3]), b = new Box(7), s = 0;
	for (var i = 0; i < n; i++) s += b.get() + a.size() + a.at(i % 3);
	return s;
}`

func BenchmarkRegNativeGetters(b *testing.B) {
	r := newTestRuntime(b, Config{})
	b.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if _, err := r.Run(compileForTest(b, nativeGetterSource)); err != nil {
		b.Fatal(err)
	}
	fn := compileForTest(b, `getters(3000)`)
	if _, err := r.Run(fn); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if v, err := r.Run(fn); err != nil || v.Number() != 36000 {
			b.Fatalf("got %v, %v", v, err)
		}
	}
}

func TestRegNativeGettersFrameless(t *testing.T) {
	nativeCounting(t)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		before := atomic.LoadInt64(&nativeCounter.Load().calls)
		if got := nativeRunSrc(t, nativeGetterSource+`String(getters(3000))`, true); got != "36000" {
			t.Fatalf("marking %v: got %s", marking, got)
		}
		if n := atomic.LoadInt64(&nativeCounter.Load().calls) - before; n > 12 {
			t.Fatalf("marking %v: getters entered %d native frames", marking, n)
		}
		before = atomic.LoadInt64(&nativeCounter.Load().calls)
		const pointers = nativeGetterSource + `
		function pointers() {
			var a = new Box({x: 7}), b = new Box([{x: 11}]), s = 0;
			for (var i = 0; i < 1000; i++) s += a.get().x + b.at(0).x;
			return s;
		}
		String(pointers())`
		if got := nativeRunSrc(t, pointers, true); got != "18000" {
			t.Fatalf("marking %v: pointer getters got %s", marking, got)
		}
		if n := atomic.LoadInt64(&nativeCounter.Load().calls) - before; n > 10 {
			t.Fatalf("marking %v: pointer getters entered %d native frames", marking, n)
		}
	}
}

func TestRegNativeGettersFallback(t *testing.T) {
	nativeCounting(t)
	for _, tc := range []struct{ name, src, want string }{
		{"field", `
		function field(b) { return b.get() }
		var b = new Box(1), out = [], log = [];
		for (var i = 0; i < 100; i++) field(b);
		out.push(field(b));
		b.v = { x: 9 }; out.push(field(b).x);
		delete b.v; Box.prototype.v = "inherited"; out.push(field(b));
		Object.defineProperty(b, "v", { get() { log.push("get"); return "accessed" }, configurable: true });
		out.push(field(b));
		Object.defineProperty(b, "v", { get() { throw new TypeError("read") }, configurable: true });
		try { field(b) } catch (e) { out.push(e.name) }
		var p = new Proxy(new Box("plain"), { get(t, k, r) { return k === "v" ? "proxy" : Reflect.get(t, k, r) } });
		out.push(field(p), log.join());
		out.join("|")`, "1|9|inherited|accessed|TypeError|proxy|get"},
		{"length", `
		function length(b) { return b.size() }
		var b = new Box([1, 2]), out = [];
		for (var i = 0; i < 100; i++) length(b);
		out.push(length(b));
		b.v.length = 4000000000; out.push(length(b));
		b.v = "a\ud83d\ude00"; out.push(length(b));
		b.v = { get length() { return 17 } }; out.push(length(b));
		b.v = null; try { length(b) } catch (e) { out.push(e.name) }
		out.join("|")`, "2|4000000000|3|17|TypeError"},
		{"index", `
		function at(b, i) { return b.at(i) }
		var b = new Box([10, 20, 30]), out = [], log = [];
		for (var i = 0; i < 100; i++) at(b, i % 3);
		for (var k of [-0, 1.5, NaN, -1, 4294967296, undefined, "1", { toString() { log.push("key"); return "2" } }])
			out.push(String(at(b, k)));
		delete b.v[1]; Array.prototype[1] = 99; out.push(at(b, 1)); delete Array.prototype[1];
		Object.defineProperty(b.v, "1", { get() { log.push("index"); return 77 }, configurable: true });
		out.push(at(b, 1));
		b.v = new Uint8Array([4, 5]); out.push(at(b, 1));
		b.v = "abc"; out.push(at(b, 1));
		out.push(log.join(",")); out.join("|")`, "10|undefined|undefined|undefined|undefined|undefined|20|30|99|77|5|b|key,index"},
		{"this", `
		function get() { return this.v }
		function strict() { "use strict"; return this.v }
		var b = { v: 42, get, strict }, out = [];
		for (var i = 0; i < 100; i++) { b.get(); b.strict() }
		out.push(get.call({v: 7}), strict.call({v: 8}));
		var v = 23; out.push(get());
		try { strict() } catch (e) { out.push(e.name) }
		Number.prototype.v = 19; out.push(get.call(3), strict.call(3));
		out.join("|")`, "7|8|23|TypeError|19|19"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := nativeGetterSource + tc.src
			if got := nativeRunSrc(t, src, false); got != tc.want {
				t.Fatalf("Go table: got %q, want %q", got, tc.want)
			}
			var exits [2]int64
			for i, marking := range []bool{false, true} {
				nativeMarking(t, marking)
				before := atomic.LoadInt64(&nativeCounter.Load().enters)
				if got := nativeRunSrc(t, src, true); got != tc.want {
					t.Fatalf("marking %v: got %q, want %q", marking, got, tc.want)
				}
				exits[i] = atomic.LoadInt64(&nativeCounter.Load().enters) - before
			}
			if exits[0] != exits[1] {
				t.Fatalf("%d exits, %d while marking", exits[0], exits[1])
			}
		})
	}
}

// A this getter's index is the parameter its get_index reads, wherever it
// is in the list: the descriptor holds the parameter's index, its register
// less the two of the callee and this, and a call without it reads
// undefined, which the frame answers.
func TestRegNativeGetterIndexParameter(t *testing.T) {
	nativeCounting(t)
	const src = `
function Box(v) { this.v = v }
Box.prototype.third = function (a, b, i) { return this.v[i] };
function run(n) {
	var box = new Box([5, 6, 7]), s = 0;
	for (var i = 0; i < n; i++) s += box.third(9, 9, i % 3);
	return s + "," + box.third(0, 2) + "," + box.third(2);
}
run(1000)`
	const want = "5999,undefined,undefined"
	if got := nativeRunSrc(t, src, false); got != want {
		t.Fatalf("Go table: got %q, want %q", got, want)
	}
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		before := atomic.LoadInt64(&nativeCounter.Load().calls)
		if got := nativeRunSrc(t, src, true); got != want {
			t.Fatalf("marking %v: got %q, want %q", marking, got, want)
		}
		if n := atomic.LoadInt64(&nativeCounter.Load().calls) - before; n > 12 {
			t.Fatalf("marking %v: the getter entered %d native frames", marking, n)
		}
	}
}
