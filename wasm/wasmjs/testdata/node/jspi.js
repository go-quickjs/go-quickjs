// The JS Promise Integration: Suspending, promising, and suspending through
// JavaScript and without a promising call.
const W = WebAssembly;
const log = [];
const b = new WasmModuleBuilder();
const imp = b.addImport("m", "f", kSig_i_i);
const plain = b.addImport("m", "plain", kSig_i_i);
b.addFunction("run", kSig_i_i).addBody([kExprLocalGet, 0, kExprCallFunction, imp, kExprI32Const, 1, kExprI32Add]).exportFunc();
b.addFunction("two", kSig_i_v).addBody([kExprI32Const, 5, kExprCallFunction, imp, kExprI32Const, 6, kExprCallFunction, imp, kExprI32Add]).exportFunc();
b.addFunction("mixed", kSig_i_i).addBody([kExprLocalGet, 0, kExprCallFunction, plain, kExprCallFunction, imp, kExprCallFunction, plain]).exportFunc();
b.addFunction("i64", makeSig([kWasmI64], [kWasmI32])).addBody([kExprI32Const, 1]).exportFunc();
b.addFunction("trap", kSig_i_i).addBody([kExprLocalGet, 0, kExprCallFunction, imp, kExprUnreachable]).exportFunc();
b.addFunction("multi", makeSig([], [kWasmI32, kWasmI32])).addBody([kExprI32Const, 1, kExprI32Const, 2]).exportFunc();
const inst = f => b.instantiate({ m: { f, plain: new W.Suspending(x => x) } }).exports;
const e1 = inst(new W.Suspending(x => { log.push("f" + x); return Promise.resolve(x * 10) }));
p("Suspending shape", () => { const s = new W.Suspending(() => 1); return [Object.prototype.toString.call(s), Object.getOwnPropertyNames(s).length, Object.getPrototypeOf(s) === W.Suspending.prototype].join(); });
p("promising shape", () => { const r = W.promising(e1.run); return [r.name, r.length, typeof r.prototype, Object.getPrototypeOf(r) === Function.prototype, r === W.promising(e1.run)].join(); });
p("promising returns a promise", () => { const r = W.promising(e1.run)(4); log.push("returned"); return Object.prototype.toString.call(r); });
pa("promising result", () => W.promising(e1.run)(4).then(v => v + " " + log.join(",")));
pa("two suspensions", () => W.promising(e1.two)());
p("without promising", () => e1.run(1));
const e2 = inst(new W.Suspending(x => x + 100));
pa("non-promise result", () => W.promising(e2.run)(1));
const e3 = inst(new W.Suspending(() => Promise.reject(new URIError("boom"))));
pa("rejected", () => W.promising(e3.run)(1));
const e4 = inst(new W.Suspending(() => { throw new EvalError("sync") }));
pa("throws", () => W.promising(e4.run)(1));
const e5 = inst(new W.Suspending(() => Promise.resolve("7")));
pa("converted result", () => W.promising(e5.run)(1));
const e6 = inst(new W.Suspending(() => Promise.resolve(Symbol())));
pa("unconvertible result", () => W.promising(e6.run)(1));
pa("trap after resuming", () => W.promising(e1.trap)(1));
pa("mixed imports", () => W.promising(e1.mixed)(3));
pa("multi-value", () => W.promising(e1.multi)());
p("argument conversion", () => W.promising(e1.i64)(5));
p("Suspending errors", () => [() => new W.Suspending(5), () => W.Suspending(() => 1), () => new W.Suspending(e1.run)].map(f => { try { f() } catch (x) { return show(x) } }).join(" | "));
p("promising errors", () => [() => W.promising(5), () => W.promising(() => 1), () => W.promising(W.promising(e1.run))].map(f => { try { f() } catch (x) { return show(x) } }).join(" | "));
p("Suspending import of plain function", () => typeof inst(new W.Suspending(function named() { return 1 })).run);
// Suspending through JavaScript frames.
const ob = new WasmModuleBuilder();
const g = ob.addImport("m", "g", kSig_i_i);
ob.addFunction("outer", kSig_i_i).addBody([kExprLocalGet, 0, kExprCallFunction, g]).exportFunc();
const oe = ob.instantiate({ m: { g: x => e1.run(x) } }).exports;
pa("through JS frames", () => W.promising(oe.outer)(1));
const oe2 = ob.instantiate({ m: { g: x => W.promising(e1.run)(x).then(() => 0) && 7 } }).exports;
pa("nested promising", () => W.promising(oe2.outer)(1));
// A suspension caught in wasm.
const cb = new WasmModuleBuilder();
const cf = cb.addImport("m", "f", kSig_i_i);
cb.addFunction("caught", kSig_i_i).addBody([
  kExprBlock, kWasmVoid,
    kExprTryTable, kWasmI32, 1, kCatchAllNoRef, 0,
      kExprLocalGet, 0, kExprCallFunction, cf,
    kExprEnd,
    kExprReturn,
  kExprEnd,
  kExprI32Const, ...wasmSignedLeb(-1)]).exportFunc();
const ce = cb.instantiate({ m: { f: new W.Suspending(x => x > 0 ? Promise.resolve(x) : Promise.reject(new Error("no"))) } }).exports;
pa("caught resolve", () => W.promising(ce.caught)(5));
pa("caught reject", () => W.promising(ce.caught)(-5));
p("SuspendError caught in wasm", () => ce.caught(1));
