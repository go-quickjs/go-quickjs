// Traps, the stack's exhaustion and the conversions of calls.
const W = WebAssembly;
const b = new WasmModuleBuilder();
b.addMemory(1, 1);
b.addType(kSig_i_i);
b.setTableBounds(2, 2);
const sig = 0;
const fns = {
  unreachable: [kSig_v_v, [kExprUnreachable]],
  oob: [kSig_i_v, [kExprI32Const, ...wasmSignedLeb(70000), kExprI32LoadMem, 0, 0]],
  divz: [kSig_i_v, [kExprI32Const, 1, kExprI32Const, 0, kExprI32DivS]],
  divov: [kSig_i_v, [...wasmI32Const(-0x80000000), kExprI32Const, ...wasmSignedLeb(-1), kExprI32DivS]],
  remz: [kSig_i_v, [kExprI32Const, 1, kExprI32Const, 0, kExprI32RemU]],
  trunc: [kSig_i_v, [...wasmF64Const(1e30), kExprI32SConvertF64]],
  nullcall: [kSig_i_v, [kExprI32Const, 0, kExprI32Const, 0, kExprCallIndirect, sig, kTableZero]],
  tableoob: [kSig_i_v, [kExprI32Const, 0, kExprI32Const, 5, kExprCallIndirect, sig, kTableZero]],
  recurse: [kSig_v_v, [kExprCallFunction, 8]],
  add: [kSig_i_ii, [kExprLocalGet, 0, kExprLocalGet, 1, kExprI32Add]],
  i64: [makeSig([kWasmI64], [kWasmI64]), [kExprLocalGet, 0, kExprI64Const, 1, kExprI64Add]],
  f32: [makeSig([kWasmF32], [kWasmF32]), [kExprLocalGet, 0]],
  f64: [makeSig([kWasmF64], [kWasmF64]), [kExprLocalGet, 0]],
  multi: [makeSig([kWasmI32], [kWasmI32, kWasmI64, kWasmF64]), [kExprLocalGet, 0, kExprI64Const, 7, ...wasmF64Const(1.5)]],
  ext: [makeSig([kWasmExternRef], [kWasmExternRef]), [kExprLocalGet, 0]],
  any: [makeSig([kWasmAnyRef], [kWasmAnyRef]), [kExprLocalGet, 0]],
  func: [makeSig([kWasmFuncRef], [kWasmFuncRef]), [kExprLocalGet, 0]],
  eq: [makeSig([kWasmEqRef], [kWasmEqRef]), [kExprLocalGet, 0]],
  i31: [makeSig([kWasmI31Ref], [kWasmI31Ref]), [kExprLocalGet, 0]],
  nnext: [makeSig([wasmRefType(kWasmExternRef)], [kWasmI32]), [kExprI32Const, 1]],
  v128: [makeSig([], [kWasmS128]), [kSimdPrefix, 0x0c, ...new Array(16).fill(0)]],
  exn: [makeSig([kWasmExnRef], []), []],
};
for (const [name, [s, body]] of Object.entries(fns)) b.addFunction(name, s).addBody(body).exportFunc();
const e = b.instantiate().exports;
for (const name of ["unreachable", "oob", "divz", "divov", "remz", "trunc", "nullcall", "tableoob", "recurse"]) {
  p(name, () => e[name]());
  p(name + " instance", () => { try { e[name]() } catch (x) { return [x instanceof W.RuntimeError, x instanceof Error, Object.prototype.toString.call(x), typeof x.stack].join() } });
  // The innermost frames: the trapping instruction's, and the caller's call.
  p(name + " frames", () => { try { e[name]() } catch (x) { return x.stack.split("\n").slice(1, 3).filter(l => l.includes("wasm://")).join(" | ") } });
}
p("add", () => e.add(1, 2));
p("add strings", () => e.add("3", "4"));
p("add objects", () => e.add({ valueOf() { return 5 } }, [6]));
p("add missing", () => e.add());
p("add wrap", () => e.add(2 ** 31, 2 ** 32 + 1));
p("add symbol", () => e.add(Symbol()));
p("add bigint", () => e.add(1n));
p("i64", () => e.i64(41n));
p("i64 wraps", () => e.i64(2n ** 64n - 1n));
p("i64 number", () => e.i64(1));
p("i64 string", () => e.i64("5"));
p("i64 bool", () => e.i64(true));
p("i64 missing", () => e.i64());
p("f32", () => e.f32(0.1));
p("f32 big", () => e.f32(1e40));
p("f64 nan", () => e.f64(NaN));
p("f64 -0", () => e.f64(-0));
p("multi", () => e.multi(3));
for (const [label, v] of [["undefined", undefined], ["null", null], ["number", 5], ["big number", 2 ** 40], ["fraction", 1.5],
    ["-0", -0], ["string", "s"], ["object", {}], ["function", () => 1], ["exported", e.add], ["bigint", 5n], ["symbol", Symbol.iterator]]) {
  for (const f of ["ext", "any", "func", "eq", "i31", "nnext"]) {
    p(f + "(" + label + ")", () => e[f](v));
  }
}
p("ext identity", () => { const o = {}; return e.ext(o) === o; });
p("any identity", () => { const o = {}; return e.any(o) === o; });
p("func identity", () => e.func(e.add) === e.add);
p("v128", () => e.v128());
p("exn", () => e.exn(null));
p("function shape", () => [e.add.name, e.add.length, typeof e.add.prototype, Object.getPrototypeOf(e.add) === Function.prototype,
  e.add === e.add, Object.getOwnPropertyNames(e.add).join("+")].join());
p("new exported", () => new e.add());
p("exported toString", () => String(e.add));
// Imports: arguments in, results out, errors.
const ib = new WasmModuleBuilder();
const imp = ib.addImport("m", "f", makeSig([kWasmI32, kWasmI64, kWasmF32, kWasmF64, kWasmExternRef], [kWasmI32]));
const imul = ib.addImport("m", "g", makeSig([], [kWasmI32, kWasmF64]));
const iv = ib.addImport("m", "v", makeSig([kWasmS128], []));
ib.addFunction("call", kSig_i_v).addBody([kExprI32Const, 1, kExprI64Const, 2, ...wasmF32Const(3.5), ...wasmF64Const(4.25), kExprRefNull, kExternRefCode, kExprCallFunction, imp]).exportFunc();
ib.addFunction("callg", makeSig([], [kWasmF64])).addBody([kExprCallFunction, imul, kExprDrop]).exportFunc();
ib.addFunction("callv", kSig_v_v).addBody([kSimdPrefix, 0x0c, ...new Array(16).fill(0), kExprCallFunction, iv]).exportFunc();
const mk = (f, g, v) => ib.instantiate({ m: { f, g, v } }).exports;
p("import args", () => { let got; mk((...a) => { got = a; return 7 }).call(); return got.map(fmt).join(" "); });
p("import returns undefined", () => mk(() => undefined).call());
p("import returns string", () => mk(() => "12").call());
p("import returns bigint", () => mk(() => 1n).call());
p("import throws", () => mk(() => { throw new EvalError("in import") }).call());
p("import throws value", () => mk(() => { throw 42 }).call());
p("multi import", () => mk(null, () => [1, 2.5]).callg());
p("multi import short", () => mk(null, () => [1]).callg());
p("multi import not iterable", () => mk(null, () => 5).callg());
p("multi import iterable", () => mk(null, function* () { yield 1; yield 2 }).callg());
p("v128 import", () => mk(null, null, () => 1).callv());
p("deep recursion through JS", () => {
  const rb = new WasmModuleBuilder();
  const ri = rb.addImport("m", "f", kSig_v_v);
  rb.addFunction("r", kSig_v_v).addBody([kExprCallFunction, ri]).exportFunc();
  const r = rb.instantiate({ m: { f: () => r.exports.r() } });
  return r.exports.r();
});
