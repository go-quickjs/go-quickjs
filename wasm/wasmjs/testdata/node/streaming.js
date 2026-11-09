// compileStreaming and instantiateStreaming over Response.
const W = WebAssembly;
const b = new WasmModuleBuilder();
const f = b.addImport("m", "f", kSig_i_v);
b.addFunction("g", kSig_i_v).addBody([kExprCallFunction, f, kExprI32Const, 1, kExprI32Add]).exportFunc();
const wasm = b.toBuffer();
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
