// Tables and globals: constructors, accessors and their conversions.
const W = WebAssembly;
const fb = new WasmModuleBuilder();
fb.addFunction("f", kSig_i_v).addBody([kExprI32Const, 5]).exportFunc();
const struct = fb.addStruct([makeField(kWasmI32, true)]);
fb.addFunction("s", makeSig([], [wasmRefType(struct)])).addBody([kExprI32Const, 1, ...GCInstr(kExprStructNew), struct]).exportFunc();
const fx = fb.instantiate().exports;
const values = { "undefined": undefined, "null": null, "number": 3, "big number": 2 ** 31, "fraction": 0.5, "-0": -0,
  "string": "x", "object": {}, "function": () => 1, "exported": fx.f, "struct": fx.s(), "bigint": 3n, "true": true };
const elements = ["anyfunc", "funcref", "externref", "anyref", "eqref", "structref", "arrayref", "i31ref", "exnref", "i32", "nullref", undefined];
for (const element of elements) {
  p("Table " + element, () => new W.Table({ element, initial: 1 }).length);
}
const tdescs = [
  {}, { element: "anyfunc" }, { element: "anyfunc", initial: -1 }, { element: "anyfunc", initial: 10000001 },
  { element: "anyfunc", initial: 2, maximum: 1 }, { element: "anyfunc", initial: 1, maximum: 2 ** 32 },
  { element: "anyfunc", initial: 1n, address: "i64" }, { element: "anyfunc", initial: 1, address: "i64" },
  { element: "anyfunc", initial: 1, address: "x" },
];
for (const [i, d] of tdescs.entries()) {
  p("Table desc " + i, () => new W.Table(d).length);
}
for (const element of ["anyfunc", "externref", "anyref", "eqref", "i31ref", "structref"]) {
  for (const [vn, v] of Object.entries(values)) {
    p("Table " + element + " init " + vn, () => fmt(new W.Table({ element, initial: 1 }, v).get(0)));
    p("Table " + element + " set " + vn, () => { const t = new W.Table({ element, initial: 1 }); t.set(0, v); return fmt(t.get(0)); });
    p("Table " + element + " grow " + vn, () => { const t = new W.Table({ element, initial: 0 }); t.grow(1, v); return fmt(t.get(0)); });
  }
  p("Table " + element + " defaults", () => { const t = new W.Table({ element, initial: 1 }); t.grow(1); t.set(0); return [fmt(t.get(0)), fmt(t.get(1))].join(); });
}
const t = new W.Table({ element: "anyfunc", initial: 2, maximum: 3 });
p("get out of range", () => t.get(2));
p("get negative", () => t.get(-1));
p("get string", () => t.get("1"));
p("set out of range", () => t.set(5, null));
p("grow past max", () => t.grow(2));
p("grow huge", () => new W.Table({ element: "anyfunc", initial: 1 }).grow(10000000));
p("grow returns", () => t.grow(1));
p("length", () => t.length);
const t64 = new W.Table({ element: "externref", initial: 1n, address: "i64" });
p("table64 length", () => t64.length);
p("table64 grow", () => t64.grow(1n, "v"));
p("table64 get", () => t64.get(1n));
p("table64 get number", () => t64.get(1));
p("table64 get out of range", () => t64.get(5n));
// Globals.
const types = ["i32", "i64", "f32", "f64", "v128", "externref", "anyfunc", "funcref", "anyref", "eqref", "i31ref", "structref", "arrayref", "exnref", "nullref", undefined];
for (const value of types) {
  p("Global " + value, () => fmt(new W.Global({ value }).value));
  p("Global mutable " + value, () => fmt(new W.Global({ value, mutable: true }).value));
}
for (const value of ["i32", "i64", "f32", "f64", "externref", "anyfunc", "anyref", "eqref", "i31ref"]) {
  for (const [vn, v] of Object.entries(values)) {
    p("Global " + value + " init " + vn, () => fmt(new W.Global({ value }, v).value));
    p("Global " + value + " set " + vn, () => { const g = new W.Global({ value, mutable: true }); g.value = v; return fmt(g.value); });
  }
}
p("immutable set", () => { const g = new W.Global({ value: "i32" }, 1); g.value = 2; return g.value; });
p("strict immutable set", () => { "use strict"; const g = new W.Global({ value: "i32" }, 1); g.value = 2; return g.value; });
p("valueOf", () => new W.Global({ value: "f64" }, 1.5).valueOf());
p("global descriptor order", () => {
  const log = [];
  const d = new Proxy({}, { get(t, k) { log.push(String(k)); return k === "value" ? "i32" : undefined } });
  new W.Global(d);
  return log.join();
});
p("global non-object", () => new W.Global("i32"));
// Exported globals and tables keep their identity and see wasm's writes.
const gb = new WasmModuleBuilder();
const g0 = gb.addGlobal(kWasmI32, true, wasmI32Const(7)).exportAs("g");
gb.addGlobal(kWasmI64, false, [kExprI64Const, 9]).exportAs("c");
gb.addFunction("inc", kSig_v_v).addBody([kExprGlobalGet, 0, kExprI32Const, 1, kExprI32Add, kExprGlobalSet, 0]).exportFunc();
gb.addTable(kWasmFuncRef, 2, 4).exportAs("t");
const ge = gb.instantiate().exports;
p("exported global", () => { ge.inc(); return [ge.g.value, ge.g === ge.g, fmt(ge.c.value)].join(); });
p("exported immutable set", () => { ge.c.value = 1n; });
p("exported table", () => [ge.t.length, fmt(ge.t.get(0)), ge.t.grow(1), ge.t.length].join());
p("global import identity", () => {
  const ib = new WasmModuleBuilder();
  ib.addImportedGlobal("m", "g", kWasmI32, true);
  ib.addExportOfKind("g", kExternalGlobal, 0);
  ib.addFunction("set", kSig_v_i).addBody([kExprLocalGet, 0, kExprGlobalSet, 0]).exportFunc();
  const g = new W.Global({ value: "i32", mutable: true }, 1);
  const e = ib.instantiate({ m: { g } }).exports;
  e.set(42);
  return [e.g === g, g.value].join();
});
