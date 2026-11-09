// Link errors, import lookup and instantiation failures.
const W = WebAssembly;
function mod(f) { const b = new WasmModuleBuilder(); f(b); return new W.Module(b.toBuffer()); }
const mods = {
  fn: mod(b => b.addImport("m", "f", kSig_i_i)),
  tab: mod(b => b.addImportedTable("m", "t", 2, 10)),
  mem: mod(b => b.addImportedMemory("m", "mem", 2, 10)),
  glob: mod(b => b.addImportedGlobal("m", "g", kWasmI32)),
  mglob: mod(b => b.addImportedGlobal("m", "g", kWasmI32, true)),
  i64glob: mod(b => b.addImportedGlobal("m", "g", kWasmI64)),
  refglob: mod(b => b.addImportedGlobal("m", "g", kWasmFuncRef)),
  extglob: mod(b => b.addImportedGlobal("m", "g", wasmRefType(kWasmExternRef))),
  i31glob: mod(b => b.addImportedGlobal("m", "g", kWasmI31Ref)),
  tag: mod(b => b.addImportedTag("m", "tag", kSig_v_i)),
  shmem: mod(b => b.addImportedMemory("m", "mem", 1, 10, true)),
};
const values = {
  "undefined": undefined, "null": null, "number": 1, "fraction": 1.5, "bigint": 1n, "string": "s", "object": {},
  "function": () => 1, "memory": new W.Memory({ initial: 2, maximum: 10 }),
  "small memory": new W.Memory({ initial: 1, maximum: 10 }), "unbounded memory": new W.Memory({ initial: 2 }),
  "large memory": new W.Memory({ initial: 2, maximum: 20 }),
  "shared memory": new W.Memory({ initial: 2, maximum: 10, shared: true }),
  "memory64": new W.Memory({ initial: 2n, maximum: 10n, address: "i64" }),
  "table": new W.Table({ element: "anyfunc", initial: 2, maximum: 10 }),
  "small table": new W.Table({ element: "anyfunc", initial: 1, maximum: 10 }),
  "unbounded table": new W.Table({ element: "anyfunc", initial: 2 }),
  "large table": new W.Table({ element: "anyfunc", initial: 2, maximum: 20 }),
  "externref table": new W.Table({ element: "externref", initial: 2, maximum: 10 }),
  "table64": new W.Table({ element: "anyfunc", initial: 2n, maximum: 10n, address: "i64" }),
  "global": new W.Global({ value: "i32" }, 1),
  "mutable global": new W.Global({ value: "i32", mutable: true }, 1),
  "f64 global": new W.Global({ value: "f64" }, 1),
  "tag": new W.Tag({ parameters: ["i32"] }), "other tag": new W.Tag({ parameters: ["f32"] }),
  "JSTag": W.JSTag,
};
for (const [mn, m] of Object.entries(mods)) {
  for (const [vn, v] of Object.entries(values)) {
    p(mn + " <- " + vn, () => { new W.Instance(m, { m: { f: v, t: v, mem: v, g: v, tag: v } }); return "linked"; });
  }
}
p("missing imports", () => new W.Instance(mods.fn));
p("missing module", () => new W.Instance(mods.fn, {}));
p("module not object", () => new W.Instance(mods.fn, { m: 1 }));
p("module function", () => { new W.Instance(mods.fn, { m: Object.assign(() => 1, { f: () => 2 }) }); return "linked"; });
p("getter throws", () => new W.Instance(mods.fn, { get m() { throw new RangeError("getter") } }));
p("import getter throws", () => new W.Instance(mods.fn, { m: { get f() { throw new RangeError("inner") } } }));
const two = mod(b => { b.addImportedGlobal("a", "x", kWasmI32); b.addImportedMemory("b", "mem", 1); b.addImport("a", "f", kSig_v_v); });
p("link order", () => new W.Instance(two, { a: { x: 1, f: 5 }, b: { mem: 3 } }));
p("lookup order", () => {
  const log = [];
  try {
    new W.Instance(two, {
      get a() { log.push("a"); return { get x() { log.push("x"); return 1 }, get f() { log.push("f"); return 1 } } },
      get b() { log.push("b"); return { get mem() { log.push("mem"); return 1 } } },
    });
  } catch (e) { log.push(show(e)); }
  return log.join(" ");
});
pa("instantiate link error", () => W.instantiate(mods.fn, { m: { f: 1 } }));
const fnBytes = (() => { const b = new WasmModuleBuilder(); b.addImport("m", "f", kSig_v_v); return b.toBuffer(); })();
pa("instantiate bytes link error", () => W.instantiate(fnBytes, { m: { f: 1 } }));
pa("instantiate bytes no imports", () => W.instantiate(fnBytes));
const exp = (() => {
  const b = new WasmModuleBuilder();
  b.addFunction("e", kSig_i_i).addBody([kExprLocalGet, 0]).exportFunc();
  b.addFunction("v", kSig_v_v).addBody([]).exportFunc();
  return new W.Instance(new W.Module(b.toBuffer())).exports;
})();
p("exported function right type", () => { new W.Instance(mods.fn, { m: { f: exp.e } }); return "linked"; });
p("exported function wrong type", () => new W.Instance(mods.fn, { m: { f: exp.v } }));
p("exported function as global", () => { new W.Instance(mods.refglob, { m: { g: exp.e } }); return "linked"; });
p("JS function as funcref global", () => new W.Instance(mods.refglob, { m: { g: () => 1 } }));
const seg = mod(b => { b.addMemory(1); b.addDataSegment(65535, [1, 2, 3]); });
p("data segment out of bounds", () => new W.Instance(seg));
pa("data segment async", () => W.instantiate(seg));
const elem = mod(b => { b.addFunction("f", kSig_v_v).addBody([]); b.setTableBounds(1); b.addElementSegment(0, 5, false, [0]); });
p("element segment out of bounds", () => new W.Instance(elem));
const start = mod(b => { b.addStart(b.addFunction("s", kSig_v_v).addBody([kExprUnreachable]).index); });
p("start traps", () => new W.Instance(start));
pa("start traps async", () => W.instantiate(start));
const jsstart = mod(b => { b.addStart(b.addImport("m", "s", kSig_v_v)); });
p("imported start throws", () => new W.Instance(jsstart, { m: { s() { throw new SyntaxError("from start") } } }));
p("re-exported memory is the same", () => {
  const m = mod(b => { b.addImportedMemory("m", "mem", 1); b.addExportOfKind("mem", kExternalMemory, 0); });
  const mem = new W.Memory({ initial: 1 });
  return new W.Instance(m, { m: { mem } }).exports.mem === mem;
});
p("re-exported function", () => {
  const m = mod(b => { b.addImport("m", "f", kSig_i_i); b.addExportOfKind("f", kExternalFunction, 0); });
  const f = x => x + 1;
  const e = new W.Instance(m, { m: { f } }).exports.f;
  const e2 = new W.Instance(m, { m: { f: exp.e } }).exports.f;
  return [e === f, e.name, e.length, e(2), e2 === exp.e].join();
});
