package wasmjs_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/wasmjs"
)

// newRuntime makes a runtime with the WebAssembly namespace.
func newRuntime(t *testing.T, opts ...quickjs.Option) *quickjs.Runtime {
	t.Helper()
	rt := quickjs.New(opts...)
	t.Cleanup(func() { rt.Close() })
	if err := wasmjs.Install(rt); err != nil {
		t.Fatal(err)
	}
	return rt
}

// eval evaluates src, runs the jobs it queued and returns the global out,
// which src sets, as a string.
func eval(t *testing.T, rt *quickjs.Runtime, src string) string {
	t.Helper()
	if _, err := rt.Eval(src); err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval(`String(globalThis.out)`)
	if err != nil {
		t.Fatal(err)
	}
	return v.String()
}

// moduleBytes is a module of the sections given, each an id and its
// contents.
func moduleBytes(sections ...[]byte) []byte {
	b := []byte{0, 'a', 's', 'm', 1, 0, 0, 0}
	for _, s := range sections {
		b = append(b, s[0])
		b = binary.AppendUvarint(b, uint64(len(s)-1))
		b = append(b, s[1:]...)
	}
	return b
}

// jsBytes writes b as a Uint8Array literal.
func jsBytes(b []byte) string {
	var s strings.Builder
	s.WriteString("new Uint8Array([")
	for i, c := range b {
		if i > 0 {
			s.WriteByte(',')
		}
		fmt.Fprint(&s, c)
	}
	s.WriteString("])")
	return s.String()
}

// TestNodeQuirksLimits pins the limits modules are validated against: the
// interface's in standards mode, V8's under WithNodeQuirks. A memory64 may
// declare up to 2^37-1 pages by the interface, 262,144 by V8; a module may
// have 100 memories by the interface, 100,000 by V8.
func TestNodeQuirksLimits(t *testing.T) {
	// A memory64 of at most 2^20 pages.
	mem64 := moduleBytes([]byte{5, 1, 0x05, 1, 0x80, 0x80, 0x40})
	// 101 memories of no pages.
	mems := []byte{5, 101}
	for range 101 {
		mems = append(mems, 0, 0)
	}
	many := moduleBytes(mems)
	src := fmt.Sprintf(`
		var out = [];
		for (const b of [%s, %s]) {
			try { new WebAssembly.Module(b); out.push("compiled") } catch (e) { out.push(e.constructor.name + ": " + e.message) }
		}
		out = out.join("\n");`, jsBytes(mem64), jsBytes(many))
	for _, c := range []struct {
		opts []quickjs.Option
		want string
	}{
		{nil, "compiled\nCompileError: WebAssembly.Module(): memory count of 101 exceeds internal limit of 100 @+11"},
		{[]quickjs.Option{quickjs.WithNodeQuirks()}, "CompileError: WebAssembly.Module(): maximum memory size (1048576 pages) is larger than implementation limit (262144 pages) @+13\ncompiled"},
	} {
		if got := eval(t, newRuntime(t, c.opts...), src); got != c.want {
			t.Errorf("quirks %v:\ngot  %s\nwant %s", c.opts != nil, got, c.want)
		}
	}
}

// TestEnginesApart pins that the JavaScript engine does not import the wasm
// engine, nor the standard library the WebAssembly namespace: a host opts
// in by installing it.
func TestEnginesApart(t *testing.T) {
	for _, pkg := range []string{"github.com/go-quickjs/go-quickjs", "github.com/go-quickjs/go-quickjs/stdlib"} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkg, err)
		}
		for _, dep := range strings.Fields(string(out)) {
			if strings.HasPrefix(dep, "github.com/go-quickjs/go-quickjs/wasm") {
				t.Errorf("%s imports %s", pkg, dep)
			}
		}
	}
}

// TestInstallLazy pins that installing builds nothing until the global is
// first read, and that the global is then one namespace.
func TestInstallLazy(t *testing.T) {
	rt := newRuntime(t)
	got := eval(t, rt, `
		const d = Object.getOwnPropertyDescriptor(globalThis, "WebAssembly");
		var out = [typeof d.get, WebAssembly === WebAssembly, typeof WebAssembly.Module,
			Object.getOwnPropertyDescriptor(globalThis, "WebAssembly").writable].join()`)
	if got != "function,true,function,true" {
		t.Fatalf("got %s", got)
	}
}

// TestJSTagRoundTrip pins that a JavaScript exception crossing wasm is
// caught by JSTag's catch as the same value and leaves as that value.
func TestJSTagRoundTrip(t *testing.T) {
	rt := newRuntime(t)
	// (import "m" "tag" (tag (param externref))) (import "m" "f" (func))
	// (func (export "catch") (result externref)
	//   (block (result externref) (try_table (catch 0 0) (call 0)) (return (ref.null extern))))
	// (func (export "pass") (call 0))
	got := eval(t, rt, `
		const t = WebAssembly.JSTag;
		const bytes = new Uint8Array([0,97,115,109,1,0,0,0,1,15,4,96,1,111,0,96,0,0,96,0,1,111,96,0,0,2,16,2,1,109,3,116,97,103,4,0,0,
			1,109,1,102,0,1,3,3,2,2,3,7,16,2,5,99,97,116,99,104,0,1,4,112,97,115,115,0,2,10,24,2,17,0,2,111,31,64,1,0,0,0,16,0,11,
			208,111,15,11,11,4,0,16,0,11]);
		const thrown = {};
		let throwIt = () => { throw thrown };
		const e = new WebAssembly.Instance(new WebAssembly.Module(bytes), {m: {tag: t, f: () => throwIt()}}).exports;
		const caught = e.catch();
		let escaped;
		try { e.pass() } catch (x) { escaped = x }
		throwIt = () => { throw new WebAssembly.Exception(new WebAssembly.Tag({parameters: []}), []) };
		let other;
		try { e.catch() } catch (x) { other = x }
		var out = [caught === thrown, escaped === thrown, other instanceof WebAssembly.Exception].join();`)
	if got != "true,true,true" {
		t.Fatalf("got %s", got)
	}
}

// promisingStacks counts the goroutines running promising calls' stacks.
// Other tests' goroutines start and end meanwhile, so the process's count
// says nothing of them.
func promisingStacks() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "wasmjs.(*api).callPromising.func")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// TestPromisingStackEnds pins that a promising call whose suspension can
// never resume ends its goroutine once nothing holds the suspension.
func TestPromisingStackEnds(t *testing.T) {
	rt := newRuntime(t)
	before := promisingStacks()
	// (import "m" "f" (func (result i32))) (func (export "run") (result i32) (call 0))
	_ = eval(t, rt, `
		const bytes = new Uint8Array([0,97,115,109,1,0,0,0,1,9,2,96,0,1,127,96,0,1,127,2,7,1,1,109,1,102,0,0,3,2,1,1,
			7,7,1,3,114,117,110,0,1,10,6,1,4,0,16,0,11]);
		const e = new WebAssembly.Instance(new WebAssembly.Module(bytes),
			{m: {f: new WebAssembly.Suspending(() => new Promise(() => {}))}}).exports;
		for (let i = 0; i < 20; i++) WebAssembly.promising(e.run)();
		var out = "";`)
	if n := promisingStacks(); n != before+20 {
		t.Fatalf("%d promising stacks, want the 20 suspended calls' too (%d before)", n, before)
	}
	deadline := time.Now().Add(10 * time.Second)
	for promisingStacks() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d promising stacks remain, %d before", promisingStacks(), before)
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

// TestInterruptWasm pins that a context ending stops wasm as it stops
// script: an infinite wasm loop, called directly and from a promising call,
// is interrupted.
func TestInterruptWasm(t *testing.T) {
	const setup = `
		const spin = new WebAssembly.Instance(new WebAssembly.Module(new Uint8Array([0,97,115,109,1,0,0,0,1,4,1,96,0,0,
			3,2,1,0,7,8,1,4,115,112,105,110,0,0,10,9,1,7,0,3,64,12,0,11,11]))).exports.spin;`
	for _, src := range []string{`spin()`, `WebAssembly.promising(spin)()`} {
		rt := newRuntime(t)
		if _, err := rt.Eval(setup); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := rt.EvalContext(ctx, src)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: %v, want the deadline", src, err)
		}
	}
}

// TestPromisingSuspends pins a promising call's life: an ordinary import
// and a suspending one, called from the call's goroutine, run on the
// runtime's; the call returns its promise at the suspension, resumes when
// the promise settles, and grows a memory whose buffer script holds, which
// is detached.
func TestPromisingSuspends(t *testing.T) {
	rt := newRuntime(t)
	got := eval(t, rt, `
		const log = [];
		const bytes = new Uint8Array([0,97,115,109,1,0,0,0,1,16,3,96,1,127,1,127,96,1,127,1,127,96,1,127,1,127,2,13,2,1,109,
			1,102,0,0,1,109,1,103,0,1,3,2,1,2,5,4,1,1,1,2,7,13,2,3,109,101,109,2,0,3,114,117,110,0,2,10,17,1,15,0,32,0,16,1,16,0,
			16,1,65,1,64,0,26,11]);
		const e = new WebAssembly.Instance(new WebAssembly.Module(bytes), {m: {
			f: new WebAssembly.Suspending(x => { log.push("f" + x); return new Promise(r => queueMicrotask(() => r(x * 10))) }),
			g: x => { log.push("g" + x); return x + 1 },
		}}).exports;
		const buffer = e.mem.buffer;
		const run = WebAssembly.promising(e.run);
		const results = [];
		for (let i = 0; i < 3; i++) run(i).then(v => results.push(v));
		log.push("returned");
		var out;
		Promise.resolve().then(() => Promise.resolve()).then(() => new Promise(r => setTimeoutish(r))).then(() => {
			out = [results.join(), log.join(), buffer.detached, e.mem.buffer.byteLength].join(" ");
		});
		function setTimeoutish(r) { let n = 20; (function tick() { n-- ? queueMicrotask(tick) : r() })() }`)
	if want := "11,21,31 g0,f1,g1,f2,g2,f3,returned,g10,g20,g30 true 131072"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// TestJITRuntimeCompilesWasm checks that a runtime made WithJIT runs its
// WebAssembly natively too, where the wasm JIT runs.
func TestJITRuntimeCompilesWasm(t *testing.T) {
	if !engine.JITAvailable() {
		t.Skip("no native code in this build")
	}
	rt := newRuntime(t, quickjs.WithJIT())
	before := engine.ReadJITStats().Compiled
	_, err := rt.Eval(`
		const bytes = new Uint8Array([0,97,115,109,1,0,0,0,1,6,1,96,1,127,1,127,3,2,1,0,7,7,1,3,105,110,99,0,0,10,9,1,7,0,32,0,65,1,106,11]);
		const inc = new WebAssembly.Instance(new WebAssembly.Module(bytes)).exports.inc;
		let n = 0;
		for (let i = 0; i < 100; i++) n = inc(n);
		if (n !== 100) throw new Error("inc gave " + n);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if engine.ReadJITStats().Compiled == before {
		t.Fatal("no function was compiled to native code")
	}
}
