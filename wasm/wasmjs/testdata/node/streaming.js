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
// The body read as it arrives, in chunks of any size, and what a chunk may be.
function chunked(bytes, size) {
  return new ReadableStream({ start(c) { for (let i = 0; i < bytes.length; i += size) c.enqueue(bytes.slice(i, i + size)); c.close(); } });
}
const bytesOf = new Uint8Array(wasm);
function badModule(f) { const b = new WasmModuleBuilder(); f(b); return new Uint8Array(b.toBuffer()); }
const invalid = {
  "bad body": badModule(b => { b.addFunction("f", kSig_i_v).addBody([kExprI32Const, 1, kExprI32Add]); b.addFunction("g", kSig_i_v).addBody([kExprNop]); }),
  "truncated": bytesOf.slice(0, bytesOf.length - 3),
  "truncated header": bytesOf.slice(0, 6),
  "trailing byte": new Uint8Array([...bytesOf, 0xff]),
  "data after the code": (() => { const x = badModule(b => { b.addFunction("f", kSig_i_v).addBody([kExprNop]); b.addMemory(1); b.addDataSegment(0, [1]); }); return new Uint8Array([...x, 11, 1, 9]); })(),
};
for (const size of [1, 3, 17, 4096]) {
  pa("chunks of " + size, () => W.instantiateStreaming(new Response(chunked(bytesOf, size), wasmType), { m: { f: () => 1 } }).then(r => r.instance.exports.g()));
  for (const [n, b] of Object.entries(invalid)) {
    pa(n + ", chunks of " + size, () => W.compileStreaming(new Response(chunked(b, size), wasmType)));
  }
}
for (const [n, chunk] of Object.entries({ string: "abc", number: 1, undefined: undefined, "SharedArrayBuffer": new SharedArrayBuffer(4),
    "view of a SharedArrayBuffer": new Uint8Array(new SharedArrayBuffer(4)), "DataView": new DataView(new ArrayBuffer(4)),
    "Int16Array": new Int16Array(2), "ArrayBuffer": new ArrayBuffer(4) })) {
  pa("chunk " + n, () => W.compileStreaming(new Response(new ReadableStream({ start(c) { c.enqueue(bytesOf.slice(0, 4)); c.enqueue(chunk); c.enqueue(bytesOf.slice(4)); c.close(); } }), wasmType)).then(m => W.Module.exports(m).length, e => show(e) + " " + e.code));
}
pa("errored body", () => W.compileStreaming(new Response(new ReadableStream({ start(c) { c.enqueue(bytesOf.slice(0, 5)); c.error(new RangeError("broken")); } }), wasmType)));
pa("null body", () => W.compileStreaming(new Response(null, wasmType)));
pa("response url", () => {
  const trap = badModule(b => b.addFunction("t", kSig_v_v).addBody([kExprUnreachable]).exportFunc());
  const fetched = new Response(trap, wasmType);
  Object.defineProperty(fetched, "url", { value: "https://example.com/m.wasm" });
  return W.instantiateStreaming(fetched).then(r => {
    try { r.instance.exports.t(); } catch (e) { return e.stack.split("\n")[1].trim(); }
  });
});
