//go:build quickjs_wasm

package wasmjs_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// streamingModules are the modules streamingCases compiles, by the name of
// the global that holds the bytes of each.
var streamingModules = map[string]string{
	"wasm": `(module
		(import "m" "f" (func $f (result i32)))
		(func (export "g") (result i32) call $f i32.const 1 i32.add))`,
	"badBody": `(module
		(func (export "f") (result i32) i32.const 1 i32.add)
		(func (export "g")))`,
	"dataModule": `(module (memory 1) (func) (data (i32.const 0) "\01"))`,
	"trap":       `(module (func (export "t") unreachable))`,
}

// streamingCases are compileStreaming's and instantiateStreaming's cases:
// those of testdata/node/streaming.js, over modules of their own.
const streamingCases = `
const W = WebAssembly;
const wasmType = { headers: { "content-type": "application/wasm" } };
const ok = () => new Response(wasm, wasmType);
pa("compileStreaming", () => W.compileStreaming(ok()).then(m => Object.prototype.toString.call(m) + " " + W.Module.imports(m).length));
pa("compileStreaming promise", () => W.compileStreaming(Promise.resolve(ok())).then(m => Object.prototype.toString.call(m)));
pa("instantiateStreaming", () => W.instantiateStreaming(ok(), { m: { f: () => 41 } }).then(r => Object.keys(r).join() + " " + r.instance.exports.g()));
pa("instantiateStreaming link error", () => W.instantiateStreaming(ok(), { m: { f: 1 } }));
pa("instantiateStreaming no imports", () => W.instantiateStreaming(ok()));
pa("instantiateStreaming bad imports", () => W.instantiateStreaming(ok(), 5));
for (const [n, v] of Object.entries({ "undefined": undefined, "null": null, "number": 5, "long string": "x".repeat(40), "string": "s",
    "object": {}, "array": [], "function": function named() {}, "null prototype": Object.create(null), "bigint": 5n, "-0": -0 })) {
  pa("compileStreaming " + n, () => W.compileStreaming(v));
}
pa("rejected source", () => W.compileStreaming(Promise.reject(new RangeError("fetch failed"))));
pa("no content type", () => W.compileStreaming(new Response(wasm)));
pa("wrong content type", () => W.compileStreaming(new Response(wasm, { headers: { "content-type": "text/plain" } })));
pa("status", () => W.compileStreaming(new Response(wasm, { status: 404, headers: { "content-type": "application/wasm" } })));
pa("body used", () => { const r = ok(); return r.arrayBuffer().then(() => W.compileStreaming(r)); });
pa("bad bytes", () => W.compileStreaming(new Response(new Uint8Array([1, 2, 3, 4]), wasmType)));
pa("empty body", () => W.compileStreaming(new Response(new Uint8Array(0), wasmType)));
pa("options", () => W.compileStreaming(ok(), { get builtins() { throw new SyntaxError("options") } }));
pa("error code", () => W.compileStreaming(5).catch(e => e.code));
function chunked(bytes, size) {
  return new ReadableStream({ start(c) { for (let i = 0; i < bytes.length; i += size) c.enqueue(bytes.slice(i, i + size)); c.close(); } });
}
const invalid = {
  "bad body": badBody,
  "truncated": wasm.slice(0, wasm.length - 3),
  "truncated header": wasm.slice(0, 6),
  "trailing byte": new Uint8Array([...wasm, 0xff]),
  "data after the code": new Uint8Array([...dataModule, 11, 1, 9]),
};
for (const size of [1, 3, 17, 4096]) {
  pa("chunks of " + size, () => W.instantiateStreaming(new Response(chunked(wasm, size), wasmType), { m: { f: () => 1 } }).then(r => r.instance.exports.g()));
  for (const [n, b] of Object.entries(invalid)) {
    pa(n + ", chunks of " + size, () => W.compileStreaming(new Response(chunked(b, size), wasmType)));
  }
}
for (const [n, chunk] of Object.entries({ string: "abc", number: 1, undefined: undefined, "SharedArrayBuffer": new SharedArrayBuffer(4),
    "view of a SharedArrayBuffer": new Uint8Array(new SharedArrayBuffer(4)), "DataView": new DataView(new ArrayBuffer(4)),
    "Int16Array": new Int16Array(2), "ArrayBuffer": new ArrayBuffer(4) })) {
  pa("chunk " + n, () => W.compileStreaming(new Response(new ReadableStream({ start(c) { c.enqueue(wasm.slice(0, 4)); c.enqueue(chunk); c.enqueue(wasm.slice(4)); c.close(); } }), wasmType)).then(m => W.Module.exports(m).length, e => show(e) + " " + e.code));
}
pa("errored body", () => W.compileStreaming(new Response(new ReadableStream({ start(c) { c.enqueue(wasm.slice(0, 5)); c.error(new RangeError("broken")); } }), wasmType)));
pa("null body", () => W.compileStreaming(new Response(null, wasmType)));
pa("response url", () => {
  const fetched = new Response(trap, wasmType);
  Object.defineProperty(fetched, "url", { value: "https://example.com/m.wasm" });
  return W.instantiateStreaming(fetched).then(r => {
    try { r.instance.exports.t(); } catch (e) { return e.stack.split("\n")[1].trim(); }
  });
});
let n = 0;
pa("slow body", () => W.instantiateStreaming(new Response(new ReadableStream({
  async pull(c) { await new Promise(r => setTimeout(r, 1)); if (n < wasm.length) c.enqueue(wasm.slice(n, n += 7)); else c.close(); }
}), wasmType), { m: { f: () => 2 } }).then(r => r.instance.exports.g()));
`

// streamingWant is what streamingCases records, which is what Node 26
// records (TestCompareNodeStreaming).
const streamingWant = `compileStreaming -> "[object WebAssembly.Module] 1"
compileStreaming promise -> "[object WebAssembly.Module]"
instantiateStreaming -> "module,instance 42"
instantiateStreaming link error -> LinkError: WebAssembly.instantiate(): Import #0 "m" "f": function import requires a callable
instantiateStreaming no imports -> TypeError: WebAssembly.instantiate(): Imports argument must be present and must be an object
instantiateStreaming bad imports -> TypeError: WebAssembly.instantiateStreaming(): Argument 1 must be an object
compileStreaming undefined -> TypeError: The "source" argument must be an instance of Response or an Promise resolving to Response. Received undefined
compileStreaming null -> TypeError: The "source" argument must be an instance of Response or an Promise resolving to Response. Received null
compileStreaming number -> TypeError: The "source" argument must be an instance of Response or an Promise resolving to Response. Received type number (5)
compileStreaming long string -> TypeError: The "source" argument must be an instance of Response or an Promise resolving to Response. Received type string ('xxxxxxxxxxxxxxxxxxxxxxxxx...')
compileStreaming string -> TypeError: The "source" argument must be an instance of Response or an Promise resolving to Response. Received type string ('s')
compileStreaming object -> TypeError: The "source" argument must be an instance of Response or an Promise resolving to Response. Received an instance of Object
compileStreaming array -> TypeError: The "source" argument must be an instance of Response or an Promise resolving to Response. Received an instance of Array
compileStreaming function -> TypeError: The "source" argument must be an instance of Response or an Promise resolving to Response. Received function named
compileStreaming null prototype -> TypeError: The "source" argument must be an instance of Response or an Promise resolving to Response. Received [Object: null prototype] {}
compileStreaming bigint -> TypeError: The "source" argument must be an instance of Response or an Promise resolving to Response. Received type bigint (5n)
compileStreaming -0 -> TypeError: The "source" argument must be an instance of Response or an Promise resolving to Response. Received type number (-0)
rejected source -> RangeError: fetch failed
no content type -> TypeError: WebAssembly response has unsupported MIME type 'null'
wrong content type -> TypeError: WebAssembly response has unsupported MIME type 'text/plain'
status -> TypeError: WebAssembly response has status code 404
body used -> TypeError: WebAssembly response body has already been used
bad bytes -> CompileError: WebAssembly.compileStreaming(): expected magic word 00 61 73 6d, found 01 02 03 04 @+0
empty body -> CompileError: WebAssembly.compileStreaming(): expected 4 bytes, fell off end @+0
options -> SyntaxError: options
error code -> "ERR_INVALID_ARG_TYPE"
`

// streamingScript writes the script of streamingCases, with its modules'
// bytes, to a file, and returns its path.
func streamingScript(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, name := range []string{"wasm", "badBody", "dataModule", "trap"} {
		bin, err := wat.Compile([]byte(streamingModules[name]))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		fmt.Fprintf(&b, "const %s = new Uint8Array([", name)
		for i, c := range bin {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprint(&b, c)
		}
		b.WriteString("]);\n")
	}
	b.WriteString(streamingCases)
	p := filepath.Join(t.TempDir(), "streaming_cases.js")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestStreaming runs compileStreaming's and instantiateStreaming's cases --
// the checks of the Response, every error, bodies in chunks of every size,
// what a chunk may be, a body that fails or arrives slowly, the URL a
// Response gives a module's stack frames -- and compares what they record
// with what Node records, written down.
func TestStreaming(t *testing.T) {
	got := runQuickJS(t, filepath.Join("testdata", "node_prelude.js"), streamingScript(t))
	want := streamingWant + streamingWantChunks
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestCompareNodeStreaming runs TestStreaming's cases in Node and compares
// what they record with what TestStreaming expects.
func TestCompareNodeStreaming(t *testing.T) {
	if os.Getenv("QUICKJS_COMPARE_NODE_WASM") != "1" {
		t.Skip("set QUICKJS_COMPARE_NODE_WASM=1 to compare with Node")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	out, err := exec.Command(node, filepath.Join("testdata", "node_runner.js"), filepath.Join("testdata", "node_prelude.js"), streamingScript(t)).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	compareLines(t, "streaming_test", streamingWant+streamingWantChunks, string(out))
}

// streamingWantChunks is what the cases of bodies in chunks record.
const streamingWantChunks = `chunks of 1 -> 2
bad body, chunks of 1 -> CompileError: WebAssembly.compileStreaming(): Compiling function #0 failed: not enough arguments on the stack for i32.add (need 2, got 1) @+41
truncated, chunks of 1 -> CompileError: WebAssembly.compileStreaming(): section (code 0, "Unknown") extends past end of the module (length 11, remaining bytes 8) @+46
truncated header, chunks of 1 -> CompileError: WebAssembly.compileStreaming(): expected 4 bytes, fell off end @+4
trailing byte, chunks of 1 -> CompileError: WebAssembly.compileStreaming(): reached end while decoding section length @+60
data after the code, chunks of 1 -> CompileError: WebAssembly.compileStreaming(): unexpected section <Data> @+40
chunks of 3 -> 2
bad body, chunks of 3 -> CompileError: WebAssembly.compileStreaming(): Compiling function #0 failed: not enough arguments on the stack for i32.add (need 2, got 1) @+41
truncated, chunks of 3 -> CompileError: WebAssembly.compileStreaming(): section (code 0, "Unknown") extends past end of the module (length 11, remaining bytes 8) @+46
truncated header, chunks of 3 -> CompileError: WebAssembly.compileStreaming(): expected 4 bytes, fell off end @+4
trailing byte, chunks of 3 -> CompileError: WebAssembly.compileStreaming(): reached end while decoding section length @+60
data after the code, chunks of 3 -> CompileError: WebAssembly.compileStreaming(): unexpected section <Data> @+40
chunks of 17 -> 2
bad body, chunks of 17 -> CompileError: WebAssembly.compileStreaming(): Compiling function #0 failed: not enough arguments on the stack for i32.add (need 2, got 1) @+41
truncated, chunks of 17 -> CompileError: WebAssembly.compileStreaming(): section (code 0, "Unknown") extends past end of the module (length 11, remaining bytes 8) @+46
truncated header, chunks of 17 -> CompileError: WebAssembly.compileStreaming(): expected 4 bytes, fell off end @+4
trailing byte, chunks of 17 -> CompileError: WebAssembly.compileStreaming(): reached end while decoding section length @+60
data after the code, chunks of 17 -> CompileError: WebAssembly.compileStreaming(): unexpected section <Data> @+40
chunks of 4096 -> 2
bad body, chunks of 4096 -> CompileError: WebAssembly.compileStreaming(): Compiling function #0 failed: not enough arguments on the stack for i32.add (need 2, got 1) @+41
truncated, chunks of 4096 -> CompileError: WebAssembly.compileStreaming(): section (code 0, "Unknown") extends past end of the module (length 11, remaining bytes 8) @+46
truncated header, chunks of 4096 -> CompileError: WebAssembly.compileStreaming(): expected 4 bytes, fell off end @+4
trailing byte, chunks of 4096 -> CompileError: WebAssembly.compileStreaming(): reached end while decoding section length @+60
data after the code, chunks of 4096 -> CompileError: WebAssembly.compileStreaming(): unexpected section <Data> @+40
chunk string -> "TypeError: chunk must be an ArrayBufferView or an ArrayBuffer ERR_INVALID_ARG_TYPE"
chunk number -> "TypeError: chunk must be an ArrayBufferView or an ArrayBuffer ERR_INVALID_ARG_TYPE"
chunk undefined -> "TypeError: chunk must be an ArrayBufferView or an ArrayBuffer ERR_INVALID_ARG_TYPE"
chunk SharedArrayBuffer -> "TypeError: chunk must be an ArrayBufferView or an ArrayBuffer ERR_INVALID_ARG_TYPE"
chunk view of a SharedArrayBuffer -> "CompileError: WebAssembly.compileStreaming(): expected version 01 00 00 00, found 00 00 00 00 @+4 undefined"
chunk DataView -> "CompileError: WebAssembly.compileStreaming(): expected version 01 00 00 00, found 00 00 00 00 @+4 undefined"
chunk Int16Array -> "CompileError: WebAssembly.compileStreaming(): expected version 01 00 00 00, found 00 00 00 00 @+4 undefined"
chunk ArrayBuffer -> "CompileError: WebAssembly.compileStreaming(): expected version 01 00 00 00, found 00 00 00 00 @+4 undefined"
errored body -> RangeError: broken
null body -> CompileError: WebAssembly.compileStreaming(): expected 4 bytes, fell off end @+0
response url -> "at https://example.com/m.wasm:wasm-function[0]:0x1e"
slow body -> 3
`

// TestStreamingTiming measures what streaming buys: a module of some 3.6 MB
// arrives as a body of 64 KB chunks a millisecond apart, and the time from
// its last chunk to the module being ready is compared with that of reading
// the whole body and then compiling it. It runs when
// QUICKJS_WASM_STREAM_TIMING=1.
func TestStreamingTiming(t *testing.T) {
	if os.Getenv("QUICKJS_WASM_STREAM_TIMING") != "1" {
		t.Skip("set QUICKJS_WASM_STREAM_TIMING=1 to measure")
	}
	var src strings.Builder
	src.WriteString("(module\n")
	body := "(local.get 0)" + strings.Repeat(" (i32.const 1) i32.add", 300)
	for range 4000 {
		fmt.Fprintf(&src, "(func (param i32) (result i32) %s)\n", body)
	}
	src.WriteString(")")
	bin, err := wat.Compile([]byte(src.String()))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("const big = new Uint8Array([")
	for i, c := range bin {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprint(&b, c)
	}
	b.WriteString(`]);
const H = { headers: { "content-type": "application/wasm" } };
let last;
function slow() {
  let i = 0;
  return new Response(new ReadableStream({ async pull(c) {
    await new Promise(r => setTimeout(r, 1));
    if (i >= big.length) { last = performance.now(); c.close(); return; }
    c.enqueue(big.slice(i, i += 65536));
  } }), H);
}
for (let round = 0; round < 3; round++) {
  pa("streaming " + round, async () => { const t0 = performance.now(); await WebAssembly.compileStreaming(slow()); const t1 = performance.now(); return "after the last chunk " + (t1 - last).toFixed(1) + " ms, in all " + (t1 - t0).toFixed(1) + " ms"; });
  pa("whole body " + round, async () => { const t0 = performance.now(); await WebAssembly.compile(await slow().arrayBuffer()); const t1 = performance.now(); return "after the last chunk " + (t1 - last).toFixed(1) + " ms, in all " + (t1 - t0).toFixed(1) + " ms"; });
}
`)
	p := filepath.Join(t.TempDir(), "timing.js")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("module of %d bytes\n%s", len(bin), runQuickJS(t, filepath.Join("testdata", "node_prelude.js"), p))
}
