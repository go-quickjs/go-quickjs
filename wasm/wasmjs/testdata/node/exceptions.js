// Exceptions crossing between wasm and JavaScript, Tag, Exception and JSTag.
const W = WebAssembly;
// Tag and Exception's own errors.
for (const [i, d] of [{}, { parameters: 1 }, { parameters: {} }, { parameters: { length: 1.5 } }, { parameters: { length: "2", 0: "i32", 1: "f64" } },
    { parameters: ["x"] }, { parameters: ["i32", "v128", "externref", "exnref"] }, { parameters: new Array(1001).fill("i32") },
    { parameters: [] }, { get parameters() { throw new EvalError("params") } }].entries()) {
  p("Tag " + i, () => Object.prototype.toString.call(new W.Tag(d)));
}
const tag = new W.Tag({ parameters: ["i32", "i64", "f32", "f64", "externref", "anyfunc"] });
const vtag = new W.Tag({ parameters: ["v128"] });
const etag = new W.Tag({ parameters: ["exnref"] });
for (const [i, args] of [[], [tag], [tag, 1], [tag, []], [tag, [1, 2n, 3, 4, {}, null]], [tag, [1, 2n, 3, 4, {}, null], 5],
    [tag, [1, 2n, 3, 4, {}, null], null], [tag, [1, 2n, 3, 4, {}, () => 1]], [tag, ["1", "2", "3", "4", 5, null]],
    [tag, { length: 6 }], [W.JSTag, [1]], [vtag, [1]], [etag, [null]], [{}, []], [tag, [1, 1, 3, 4, {}, null]]].entries()) {
  p("Exception " + i, () => Object.prototype.toString.call(new W.Exception(...args)));
}
const x = new W.Exception(tag, [1.5, 2n ** 63n, 0.1, -0, "ext", null]);
for (let i = 0; i < 7; i++) p("getArg " + i, () => x.getArg(tag, i));
p("getArg wrong tag", () => x.getArg(vtag, 0));
p("getArg not tag", () => x.getArg({}, 0));
p("getArg bad index", () => x.getArg(tag, -1));
p("getArg v128", () => new W.Exception(vtag, [0]).getArg(vtag, 0));
p("is", () => [x.is(tag), x.is(vtag), x.is(W.JSTag)].join());
p("is not tag", () => x.is(1));
p("traceStack", () => { const t = new W.Exception(tag, [1, 2n, 3, 4, {}, null], { traceStack: true }); return [typeof t.stack, t.stack.split("\n")[0], Object.getOwnPropertyNames(t).join()].join("|"); });
p("no traceStack", () => { const t = new W.Exception(tag, [1, 2n, 3, 4, {}, null]); return [typeof t.stack, Object.getOwnPropertyNames(t).length].join(); });
p("Exception props", () => [x instanceof Error, x instanceof W.Exception, String(x), x.message].join());
// A module that throws, catches and rethrows.
const b = new WasmModuleBuilder();
const jstag = b.addImportedTag("m", "jstag", makeSig([kWasmExternRef], []));
const t0 = b.addTag(kSig_v_i);
b.addExportOfKind("tag", kExternalTag, t0);
const f = b.addImport("m", "f", kSig_v_v);
const g = b.addImport("m", "g", kSig_v_v);
b.addFunction("throw", kSig_v_i).addBody([kExprLocalGet, 0, kExprThrow, t0]).exportFunc();
b.addFunction("throwJS", makeSig([kWasmExternRef], [])).addBody([kExprLocalGet, 0, kExprThrow, jstag]).exportFunc();
// Calls f; on catch_all returns 1, on a JSTag catch returns the value as externref.
b.addFunction("catchAll", kSig_i_v).addBody([
  kExprBlock, kWasmVoid,
    kExprTryTable, kWasmVoid, 1, kCatchAllNoRef, 0,
      kExprCallFunction, f,
    kExprEnd,
    kExprI32Const, 0, kExprReturn,
  kExprEnd,
  kExprI32Const, 1]).exportFunc();
b.addFunction("catchJS", makeSig([], [kWasmExternRef])).addBody([
  kExprBlock, kExternRefCode,
    kExprTryTable, kWasmVoid, 1, kCatchNoRef, jstag, 0,
      kExprCallFunction, f,
    kExprEnd,
    kExprRefNull, kExternRefCode, kExprReturn,
  kExprEnd]).exportFunc();
b.addFunction("catchTag", kSig_i_v).addBody([
  kExprBlock, kWasmI32,
    kExprTryTable, kWasmVoid, 1, kCatchNoRef, t0, 0,
      kExprCallFunction, f,
    kExprEnd,
    kExprI32Const, ...wasmSignedLeb(-1), kExprReturn,
  kExprEnd]).exportFunc();
// Catches with catch_all_ref and rethrows with throw_ref, calling g between.
b.addFunction("rethrow", kSig_v_v).addBody([
  kExprBlock, kExnRefCode,
    kExprTryTable, kWasmVoid, 1, kCatchAllRef, 0,
      kExprCallFunction, f,
    kExprEnd,
    kExprReturn,
  kExprEnd,
  kExprCallFunction, g,
  kExprThrowRef]).exportFunc();
const jsTagImport = W.JSTag;
let fImpl = () => {}, gImpl = () => {};
const e = b.instantiate({ m: { jstag: jsTagImport, f: () => fImpl(), g: () => gImpl() } }).exports;
p("exported tag", () => [Object.prototype.toString.call(e.tag), e.tag === e.tag].join());
p("wasm throw", () => { try { e.throw(42) } catch (x) { return [x instanceof W.Exception, x.is(e.tag), x.getArg(e.tag, 0), x instanceof Error, typeof x.stack].join() } });
p("wasm throw identity", () => { let a, b2; try { e.throw(1) } catch (x) { a = x } fImpl = () => { throw a }; try { e.rethrow() } catch (x) { b2 = x } return a === b2; });
p("JSTag throw from wasm", () => { const o = { tag: "js" }; try { e.throwJS(o) } catch (x) { return x === o } });
p("JSTag throw null", () => e.throwJS(null));
p("JSTag throw number", () => e.throwJS(5));
const values = { "error": new TypeError("t"), "number": 7, "big number": 2 ** 40, "string": "s", "undefined": undefined, "null": null, "object": { o: 1 } };
for (const [n, v] of Object.entries(values)) {
  p("catch_all " + n, () => { fImpl = () => { throw v }; return e.catchAll(); });
  p("JSTag catch " + n, () => { fImpl = () => { throw v }; return fmt(e.catchJS()) + " " + (e.catchJS() === v); });
  p("tag catch " + n, () => { fImpl = () => { throw v }; return e.catchTag(); });
  p("rethrow " + n, () => { fImpl = () => { throw v }; try { e.rethrow(); return "no" } catch (x) { return x === v } });
}
p("JS throws a WebAssembly.Exception", () => { fImpl = () => { throw new W.Exception(e.tag, [99]) }; return e.catchTag(); });
p("JS throws an Exception of another tag", () => { fImpl = () => { throw new W.Exception(tag, [1, 2n, 3, 4, {}, null]) }; return [e.catchTag(), e.catchAll()].join(); });
p("Exception through JSTag catch", () => { fImpl = () => { throw new W.Exception(e.tag, [3]) }; try { return fmt(e.catchJS()) } catch (x) { return "escaped " + x.getArg(e.tag, 0) } });
p("trap through JS is not caught", () => { fImpl = () => e.throw2 ? 0 : (() => { const tb = new WasmModuleBuilder(); tb.addFunction("u", kSig_v_v).addBody([kExprUnreachable]).exportFunc(); return tb.instantiate().exports.u(); })(); return e.catchAll(); });
p("RuntimeError from JS is caught", () => { fImpl = () => { throw new W.RuntimeError("js") }; return e.catchAll(); });
p("rethrow keeps the exception", () => { let inner; fImpl = () => e.throw(5); gImpl = () => {}; try { e.rethrow() } catch (x) { inner = x } return [inner instanceof W.Exception, inner.getArg(e.tag, 0)].join(); });
p("g throws instead", () => { fImpl = () => { throw 1 }; gImpl = () => { throw 2 }; try { e.rethrow() } catch (x) { return x } });
// Legacy try/catch, delegate and rethrow.
const lb = new WasmModuleBuilder();
const lt = lb.addTag(kSig_v_i);
const lf = lb.addImport("m", "f", kSig_v_v);
lb.addFunction("legacy", kSig_i_v).addBody([
  kExprTry, kWasmI32,
    kExprCallFunction, lf,
    kExprI32Const, 0,
  kExprCatchAll,
    kExprI32Const, 1,
  kExprEnd]).exportFunc();
lb.addFunction("legacyRethrow", kSig_v_v).addBody([
  kExprTry, kWasmVoid,
    kExprCallFunction, lf,
  kExprCatchAll,
    kExprRethrow, 0,
  kExprEnd]).exportFunc();
let lImpl = () => {};
const le = lb.instantiate({ m: { f: () => lImpl() } }).exports;
p("legacy catch_all", () => { lImpl = () => { throw "x" }; return le.legacy(); });
p("legacy rethrow", () => { const o = {}; lImpl = () => { throw o }; try { le.legacyRethrow() } catch (x) { return x === o } });
