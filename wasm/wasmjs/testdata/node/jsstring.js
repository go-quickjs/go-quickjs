// The JS String Builtins and imported string constants.
const W = WebAssembly;
const builder = new WasmModuleBuilder();
const a16 = builder.addArray(kWasmI16, true, kNoSuperType, true);
const bs = [
  ["test", [kWasmExternRef], [kWasmI32]],
  ["cast", [kWasmExternRef], [wasmRefType(kWasmExternRef)]],
  ["fromCharCodeArray", [wasmRefNullType(a16), kWasmI32, kWasmI32], [wasmRefType(kWasmExternRef)]],
  ["intoCharCodeArray", [kWasmExternRef, wasmRefNullType(a16), kWasmI32], [kWasmI32]],
  ["fromCharCode", [kWasmI32], [wasmRefType(kWasmExternRef)]],
  ["fromCodePoint", [kWasmI32], [wasmRefType(kWasmExternRef)]],
  ["charCodeAt", [kWasmExternRef, kWasmI32], [kWasmI32]],
  ["codePointAt", [kWasmExternRef, kWasmI32], [kWasmI32]],
  ["length", [kWasmExternRef], [kWasmI32]],
  ["concat", [kWasmExternRef, kWasmExternRef], [wasmRefType(kWasmExternRef)]],
  ["substring", [kWasmExternRef, kWasmI32, kWasmI32], [wasmRefType(kWasmExternRef)]],
  ["equals", [kWasmExternRef, kWasmExternRef], [kWasmI32]],
  ["compare", [kWasmExternRef, kWasmExternRef], [kWasmI32]],
];
for (const bi of bs) bi.push(builder.addImport("wasm:js-string", bi[0], builder.addType({ params: bi[1], results: bi[2] })));
builder.addImportedGlobal("str", "hello", kWasmExternRef);
builder.addImportedGlobal("str", "\u{1F600}", wasmRefType(kWasmExternRef));
for (const bi of bs) {
  const f = builder.addFunction(bi[0] + "X", builder.addType({ params: bi[1], results: bi[2] }));
  const body = [];
  for (let i = 0; i < bi[1].length; i++) body.push(kExprLocalGet, i);
  body.push(kExprCallFunction, bi[3]);
  f.addBody(body).exportAs(bi[0]);
}
builder.addFunction("mk", makeSig([kWasmI32], [wasmRefNullType(a16)])).addBody([kExprLocalGet, 0, ...GCInstr(kExprArrayNewDefault), a16]).exportFunc();
builder.addFunction("at", makeSig([wasmRefNullType(a16), kWasmI32], [kWasmI32])).addBody([kExprLocalGet, 0, kExprLocalGet, 1, ...GCInstr(kExprArrayGetU), a16]).exportFunc();
for (const bi of bs) builder.addExport(bi[0] + "Raw", bi[3]);
builder.addExportOfKind("hello", kExternalGlobal, 0);
builder.addExportOfKind("smile", kExternalGlobal, 1);
const opts = { builtins: ["js-string"], importedStringConstants: "str" };
const m = new W.Module(builder.toBuffer(), opts);
const e = new W.Instance(m, {}).exports;
p("imports", () => JSON.stringify(W.Module.imports(m)));
p("constants", () => [e.hello.value, e.smile.value].join());
p("without the options", () => new W.Instance(new W.Module(builder.toBuffer()), {}));
const vals = { "string": "ab\u{10000}", "empty": "", "number": 5, "null": null, "undefined": undefined, "object": {}, "boxed": new String("s") };
for (const [n, v] of Object.entries(vals)) {
  p("test " + n, () => e.test(v));
  p("cast " + n, () => fmt(e.cast(v)));
  p("length " + n, () => e.length(v));
  p("charCodeAt " + n, () => e.charCodeAt(v, 0));
  p("codePointAt " + n, () => e.codePointAt(v, 2));
  p("concat " + n, () => e.concat(v, "!"));
  p("substring " + n, () => e.substring(v, 1, 2));
  p("equals " + n, () => e.equals(v, "ab\u{10000}"));
  p("compare " + n, () => e.compare(v, "ab"));
}
p("charCodeAt out of range", () => e.charCodeAt("ab", 2));
p("charCodeAt negative", () => e.charCodeAt("ab", -1));
p("codePointAt surrogates", () => [e.codePointAt("\u{1F600}", 0), e.codePointAt("\u{1F600}", 1)].join());
p("fromCharCode", () => [e.fromCharCode(65), e.fromCharCode(0x10041), e.fromCharCode(-1).charCodeAt(0)].join());
p("fromCodePoint", () => [e.fromCodePoint(0x1F600), e.fromCodePoint(65)].join());
p("fromCodePoint too large", () => e.fromCodePoint(0x110000));
p("fromCodePoint negative", () => e.fromCodePoint(-1));
p("substring cases", () => [e.substring("abcd", 3, 1), e.substring("abcd", 1, 9), e.substring("abcd", 5, 6), e.substring("abcd", -1, 2), e.substring("abcd", 0, -1)].map(JSON.stringify).join());
p("equals nulls", () => [e.equals(null, null), e.equals(null, "a"), e.equals("a", "a")].join());
p("equals non-string", () => e.equals(1, null));
p("compare order", () => [e.compare("a", "b"), e.compare("b", "a"), e.compare("a", "a"), e.compare("\uD800", "￿")].join());
const arr = e.mk(4);
p("intoCharCodeArray", () => [e.intoCharCodeArray("hi", arr, 1), e.at(arr, 1), e.at(arr, 2)].join());
p("intoCharCodeArray overflow", () => e.intoCharCodeArray("hello", arr, 0));
p("intoCharCodeArray at end", () => e.intoCharCodeArray("", arr, 4));
p("intoCharCodeArray past end", () => e.intoCharCodeArray("", arr, 5));
p("intoCharCodeArray null", () => e.intoCharCodeArray("a", null, 0));
p("fromCharCodeArray", () => JSON.stringify(e.fromCharCodeArray(arr, 1, 3)));
p("fromCharCodeArray reversed", () => e.fromCharCodeArray(arr, 3, 1));
p("fromCharCodeArray past end", () => e.fromCharCodeArray(arr, 0, 5));
p("fromCharCodeArray null", () => e.fromCharCodeArray(null, 0, 0));
p("raw builtins", () => [e.castRaw.name, e.castRaw.length, e.castRaw === e.castRaw, e.fromCharCodeArrayRaw.length].join());
p("raw cast", () => e.castRaw(1));
p("raw test", () => e.testRaw("x"));
p("constant identity", () => e.hello.value === "hello");
p("bad constant type", () => new W.Module((() => { const b = new WasmModuleBuilder(); b.addImportedGlobal("str", "x", kWasmI32); return b.toBuffer(); })(), opts));
p("mutable constant", () => new W.Module((() => { const b = new WasmModuleBuilder(); b.addImportedGlobal("str", "x", kWasmExternRef, true); return b.toBuffer(); })(), opts));
p("function from constants module", () => new W.Module((() => { const b = new WasmModuleBuilder(); b.addImport("str", "x", kSig_v_v); return b.toBuffer(); })(), opts));
p("unknown builtin", () => { const b = new WasmModuleBuilder(); b.addImport("wasm:js-string", "nope", kSig_v_v); return new W.Instance(new W.Module(b.toBuffer(), opts), { "wasm:js-string": { nope() {} } }) && "linked"; });
p("unknown builtin missing", () => { const b = new WasmModuleBuilder(); b.addImport("wasm:js-string", "nope", kSig_v_v); return new W.Instance(new W.Module(b.toBuffer(), opts), {}); });
p("builtins not asked for", () => { const b = new WasmModuleBuilder(); b.addImport("wasm:js-string", "cast", kSig_i_i); return new W.Instance(new W.Module(b.toBuffer()), {}); });
