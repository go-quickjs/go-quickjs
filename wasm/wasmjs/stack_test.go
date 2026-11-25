//go:build quickjs_wasm

package wasmjs_test

import (
	"testing"
)

// stackModule is
//
//	(module $m (import "e" "f" (func $f (param i32)))
//	  (func $a (export "a") (param i32)
//	    (if (local.get 0)
//	      (then (call $f (local.get 0)))
//	      (else (nop) (call $f (local.get 0))))))
//
// whose two calls of $f are at 0x2e and 0x34.
const stackModule = `new Uint8Array([0,97,115,109,1,0,0,0,1,5,1,96,1,127,0,2,7,1,1,101,1,102,0,0,3,2,1,0,7,5,1,1,97,0,1,10,19,1,17,0,32,0,4,64,32,0,16,0,5,1,32,0,16,0,11,11,0,18,4,110,97,109,101,0,2,1,109,1,7,2,0,1,102,1,1,97])`

// TestStackWasmFrames pins that a stack trace shows the wasm frames of an
// exported function's call in the place of the function's own frame: an
// error thrown by an import that wasm called, two calls deep
// in one exported function, shows each call's frame at its own call site,
// innermost first.
func TestStackWasmFrames(t *testing.T) {
	rt := newRuntime(t)
	got := eval(t, rt, `
		let e;
		e = new WebAssembly.Instance(new WebAssembly.Module(`+stackModule+`),
			{e: {f(n) { if (n) e.a(n - 1); else throw new Error("deep"); }}}).exports;
		let stack;
		try { e.a(1); } catch (x) { stack = x.stack; }
		var out = stack.split("\n").filter(l => l.includes("wasm://") || l.includes(" f ")).map(l => l.replace(/\(.*eval.*\)/, "(script)")).join("\n");`)
	want := `    at f (script)
    at m.a (wasm://wasm/m-51454cee:wasm-function[1]:0x34)
    at f (script)
    at m.a (wasm://wasm/m-51454cee:wasm-function[1]:0x2e)`
	if got != want {
		t.Fatalf("stack:\n%s\nwant:\n%s", got, want)
	}
}

// TestStackWasmCallSite pins what Error.prepareStackTrace's CallSites answer
// for a wasm frame: the function's index for getFunction, its
// own name, the module's URL, line 1 and the offset as the column, the
// function's start, and no receiver type or method name.
func TestStackWasmCallSite(t *testing.T) {
	rt := newRuntime(t)
	got := eval(t, rt, `
		let e;
		e = new WebAssembly.Instance(new WebAssembly.Module(`+stackModule+`),
			{e: {f(n) { if (n) e.a(n - 1); else throw new Error("deep"); }}}).exports;
		Error.prepareStackTrace = (_, cs) => cs;
		let sites;
		try { e.a(1); } catch (x) { sites = x.stack; }
		Error.prepareStackTrace = undefined;
		const s = sites[1];
		var out = [s.getFunction(), s.getFunctionName(), s.getFileName(), s.getLineNumber(), s.getColumnNumber(),
			s.getPosition(), s.getEnclosingLineNumber(), s.getEnclosingColumnNumber(), s.getTypeName(),
			s.getMethodName(), s.isToplevel(), s.isNative(), s.getThis() instanceof WebAssembly.Instance,
			s.getScriptNameOrSourceURL(), String(s)].join("|");`)
	want := "1|a|wasm://wasm/m-51454cee|1|53|52|1|39|||false|false|true|wasm://wasm/m-51454cee|m.a (wasm://wasm/m-51454cee:wasm-function[1]:0x34)"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// TestStackTrapFrames pins a trap's stack: the wasm frames, named by the
// module's name section, the innermost at the trapping instruction and its
// caller at its call.
func TestStackTrapFrames(t *testing.T) {
	rt := newRuntime(t)
	bytes := watBytes(t, `(module $mod (func $inner (export "inner") unreachable) (func (export "outer") call $inner))`)
	got := eval(t, rt, `
		const bytes = `+bytes+`;
		const e = new WebAssembly.Instance(new WebAssembly.Module(bytes)).exports;
		let stack;
		try { e.outer(); } catch (x) { stack = x.stack; }
		var out = stack.split("\n").slice(0, 3).join("\n");`)
	want := `RuntimeError: unreachable
    at mod.inner (wasm://wasm/mod-07f3ce6a:wasm-function[0]:0x2b)
    at mod (wasm://wasm/mod-07f3ce6a:wasm-function[1]:0x2f)`
	if got != want {
		t.Fatalf("stack:\n%s\nwant:\n%s", got, want)
	}
}

// TestStackPromisingFrames pins that a call through WebAssembly.promising
// shows its wasm frames in a stack trace as a direct call does: an error an
// import throws, a trap, and the stack's exhaustion, each before the call
// suspends. Node 26 prints the same lines for the same script.
func TestStackPromisingFrames(t *testing.T) {
	rt := newRuntime(t)
	bytes := watBytes(t, `(module $mod
		(import "e" "f" (func $f))
		(func $inner (export "inner") call $f)
		(func $trap (export "trap") unreachable)
		(func $deep (export "deep") call $deep))`)
	got := eval(t, rt, promisingFramesScript(bytes))
	want := `inner: TypeError: from f | at f (script) | at mod.inner (wasm://wasm/mod-0f7e68ce:wasm-function[1]:0x3b)
trap: RuntimeError: unreachable | at mod.trap (wasm://wasm/mod-0f7e68ce:wasm-function[2]:0x40)
deep: RangeError: Maximum call stack size exceeded | at mod.deep (wasm://wasm/mod-0f7e68ce:wasm-function[3]:0x43) | at mod.deep (wasm://wasm/mod-0f7e68ce:wasm-function[3]:0x44)`
	if got != want {
		t.Fatalf("stacks:\n%s\nwant:\n%s", got, want)
	}
}

// promisingFramesScript is a script that calls each of the exports inner,
// trap and deep of the module of bytes through WebAssembly.promising and
// sets out to the head of each rejection's stack -- its message and the
// frames down to the first that is neither the import's nor wasm's --
// noting where it differs from a direct call's.
func promisingFramesScript(bytes string) string {
	return `
		const bytes = ` + bytes + `;
		const e = new WebAssembly.Instance(new WebAssembly.Module(bytes),
			{e: {f() { throw new TypeError("from f"); }}}).exports;
		const head = x => {
			const ls = x.stack.split("\n").map(l => l.trim().replace(/^at f \(.*\)$/, "at f (script)"));
			const n = ls.findIndex((l, i) => i > 0 && !l.includes("wasm://") && l !== "at f (script)");
			return ls.slice(0, Math.min(n < 0 ? ls.length : n, 3)).join(" | ");
		};
		const lines = [];
		for (const name of ["inner", "trap", "deep"]) {
			let direct;
			try { e[name](); } catch (x) { direct = head(x); }
			WebAssembly.promising(e[name])().catch(x => {
				const h = head(x);
				lines.push(name + ": " + h + (h === direct ? "" : " (direct: " + direct + ")"));
			});
		}
		Promise.resolve().then(() => {}).then(() => { globalThis.out = lines.join("\n"); });`
}
