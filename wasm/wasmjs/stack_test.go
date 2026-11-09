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
