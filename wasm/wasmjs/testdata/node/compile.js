// Compile errors, synchronous and asynchronous, and validate.
const W = WebAssembly;
const cases = {
  "bad magic": bytes(1, 2, 3, 4),
  "bad version": bytes(0, 0x61, 0x73, 0x6d, 2, 0, 0, 0),
  "truncated header": bytes(0, 0x61, 0x73),
  "truncated section": bytes(0, 0x61, 0x73, 0x6d, 1, 0, 0, 0, 1),
  "unknown section": bytes(0, 0x61, 0x73, 0x6d, 1, 0, 0, 0, 0x20, 0),
};
function build(f) { const b = new WasmModuleBuilder(); f(b); return b.toBuffer(); }
cases["type mismatch"] = build(b => b.addFunction("f", kSig_i_v).addBody([kExprI32Const, 1, kExprI64Const, 1, kExprI32Add]));
cases["bad local"] = build(b => b.addFunction("f", kSig_v_v).addBody([kExprLocalGet, 3]));
cases["bad call"] = build(b => b.addFunction("f", kSig_v_v).addBody([kExprCallFunction, 7]));
cases["missing result"] = build(b => b.addFunction("f", kSig_i_v).addBody([]));
cases["load without memory"] = build(b => b.addFunction("f", kSig_v_v).addBody([kExprI32Const, 0, kExprI32LoadMem, 0, 0, kExprDrop]));
cases["memory too large"] = build(b => b.addMemory(70000, 70000));
cases["duplicate export"] = build(b => {
  b.addFunction("f", kSig_v_v).addBody([kExprNop]).exportAs("a");
  b.addFunction("g", kSig_v_v).addBody([kExprNop]).exportAs("a");
});
cases["builtins and body"] = build(b => {
  b.addImport("wasm:js-string", "cast", kSig_i_i);
  b.addImportedGlobal("str", "x".repeat(60), kWasmI32);
  b.addFunction("f", kSig_v_v).addBody([kExprLocalGet, 0]);
});
cases["builtins only"] = build(b => {
  b.addImport("wasm:js-string", "cast", kSig_i_i);
});
cases["string constant"] = build(b => {
  b.addImportedGlobal("str", "x".repeat(60), kWasmI32);
});
for (const [name, buf] of Object.entries(cases)) {
  p("Module " + name, () => new W.Module(buf));
  p("validate " + name, () => W.validate(buf));
  pa("compile " + name, () => W.compile(buf));
  pa("instantiate " + name, () => W.instantiate(buf));
}
const opts = { builtins: ["js-string"], importedStringConstants: "str" };
for (const name of ["builtins and body", "builtins only", "string constant"]) {
  const b = cases[name];
  p("Module with options " + name, () => new W.Module(b, opts));
  p("validate with options " + name, () => W.validate(b, opts));
  pa("compile with options " + name, () => W.compile(b, opts));
  pa("instantiate with options " + name, () => W.instantiate(b, {}, opts));
}
p("options getter throws", () => new W.Module(emptyModule, { get builtins() { throw new RangeError("opt") } }));
p("validate options throw", () => W.validate(emptyModule, { get builtins() { throw new RangeError("opt") } }));
pa("compile options throw", () => W.compile(emptyModule, { get builtins() { throw new RangeError("opt") } }));
p("options builtins not array", () => !!new W.Module(emptyModule, { builtins: "js-string" }));
p("options builtins array-like", () => !!new W.Module(emptyModule, { builtins: { length: 1, 0: "js-string" } }));
p("customSections", () => {
  const b = new WasmModuleBuilder();
  b.addCustomSection("name1", [1, 2, 3]);
  b.addCustomSection("name1", [4]);
  b.addCustomSection("other", [5]);
  const m = new W.Module(b.toBuffer());
  return W.Module.customSections(m, "name1").map(s => new Uint8Array(s).join("+")).join(",") + " " +
    W.Module.customSections(m, "none").length;
});
p("imports and exports", () => {
  const b = new WasmModuleBuilder();
  b.addImport("m", "f", kSig_v_v);
  b.addImportedMemory("m", "mem", 1);
  b.addImportedTable("m", "t", 1);
  b.addImportedGlobal("m", "g", kWasmI32);
  b.addImportedTag("m", "tag", kSig_v_v);
  b.addFunction("e", kSig_v_v).addBody([]).exportFunc();
  b.addExportOfKind("mem", kExternalMemory, 0);
  const m = new W.Module(b.toBuffer());
  return JSON.stringify([W.Module.imports(m), W.Module.exports(m)]);
});
