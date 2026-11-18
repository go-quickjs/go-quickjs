// Structured cloning: a Module and a shared Memory clone, in one runtime and
// to a worker; nothing else of the namespace does.
const W = WebAssembly;
const builder = new WasmModuleBuilder();
builder.addImportedMemory("m", "mem", 1, 4, true);
builder.exportMemoryAs("mem");
builder.addFunction("load", kSig_i_i).addBody([kExprLocalGet, 0, kExprI32LoadMem, 2, 0]).exportFunc();
builder.addFunction("grow", kSig_i_i).addBody([kExprLocalGet, 0, kExprMemoryGrow, 0]).exportFunc();
const mod = builder.toModule();
const smem = new W.Memory({ initial: 1, maximum: 4, shared: true });
const tag = new W.Tag({ parameters: [] });
const uncloneable = {
  Instance: new W.Instance(new W.Module(emptyModule)),
  Memory: new W.Memory({ initial: 1 }),
  "Memory with a maximum": new W.Memory({ initial: 1, maximum: 2 }),
  Table: new W.Table({ initial: 1, element: "anyfunc" }),
  Global: new W.Global({ value: "i32" }, 1),
  Tag: tag,
  Exception: new W.Exception(tag, []),
  Suspending: new W.Suspending(() => 0),
  "exported function": new W.Instance(mod, { m: { mem: smem } }).exports.load,
};
for (const [name, v] of Object.entries(uncloneable)) {
  p("clone " + name, () => structuredClone(v));
  p("clone nested " + name, () => structuredClone({ a: [v] }));
}
p("clone error kinds", () => [new W.CompileError("c"), new W.LinkError("l"), new W.RuntimeError("r")]
  .map((e) => { const c = structuredClone(e); return [c.constructor.name, c.name, c.message].join(":"); }).join());
p("transfer Module", () => structuredClone(mod, { transfer: [mod] }));
p("transfer Memory", () => structuredClone(smem, { transfer: [smem] }));

p("Module clone", () => {
  const c = structuredClone(mod);
  return [Object.prototype.toString.call(c), c === mod, Object.getPrototypeOf(c) === W.Module.prototype,
    c instanceof W.Module, JSON.stringify(W.Module.exports(c)), JSON.stringify(W.Module.imports(c))].join();
});
p("Module clone instantiates", () => {
  const i = new W.Instance(structuredClone(mod), { m: { mem: smem } });
  new Int32Array(smem.buffer)[1] = 7;
  return i.exports.load(4);
});
p("Module clone keeps own properties out", () => { mod.x = 1; return "x" in structuredClone(mod); });
p("Module cloned twice in one message", () => { const c = structuredClone([mod, mod]); return c[0] === c[1]; });
p("Module subclass", () => {
  class M extends W.Module {}
  const c = structuredClone(new M(emptyModule));
  return [c instanceof M, Object.getPrototypeOf(c) === W.Module.prototype].join();
});

p("Memory clone", () => {
  const c = structuredClone(smem);
  return [Object.prototype.toString.call(c), c === smem, c.buffer === smem.buffer,
    Object.prototype.toString.call(c.buffer), c.buffer.byteLength, Object.isFrozen(c.buffer)].join();
});
p("Memory clone shares bytes", () => {
  const c = structuredClone(smem);
  new Uint8Array(c.buffer)[100] = 42;
  return new Uint8Array(smem.buffer)[100];
});
p("Memory clone grows both", () => {
  const c = structuredClone(smem);
  const before = smem.buffer;
  return [c.grow(1), smem.buffer.byteLength, c.buffer.byteLength, before.byteLength, smem.buffer === before].join();
});
p("Memory clone imported and exported", () => {
  const c = structuredClone(smem);
  const i = new W.Instance(mod, { m: { mem: c } });
  return [i.exports.mem === c, i.exports.mem === smem, i.exports.grow(0)].join();
});
p("Memory clone of an export", () => {
  const i = new W.Instance(mod, { m: { mem: smem } });
  const c = structuredClone(i.exports.mem);
  const j = new W.Instance(mod, { m: { mem: c } });
  return [j.exports.mem === c, i.exports.mem === smem].join();
});
p("Memory own property", () => { smem.y = 1; return "y" in structuredClone(smem); });
p("memory64 clone", () => {
  const m = new W.Memory({ initial: 1n, maximum: 3n, shared: true, address: "i64" });
  const c = structuredClone(m);
  return [c.grow(1n), m.buffer.byteLength].join();
});
p("buffer clone", () => {
  const b = structuredClone(smem.buffer);
  return [Object.prototype.toString.call(b), b === smem.buffer, b.byteLength === smem.buffer.byteLength,
    Object.isFrozen(b), b.growable].join();
});

const workerCode = `
import { parentPort } from "node:worker_threads";
parentPort.once("message", ({ mod, mem, list }) => {
  const out = [];
  const t = (f) => { try { out.push(String(f())); } catch (e) { out.push(e.constructor.name + ": " + e.message); } };
  t(() => Object.prototype.toString.call(mod) + " " + Object.prototype.toString.call(mem));
  t(() => [mem.buffer.byteLength, mem.buffer instanceof SharedArrayBuffer, Object.isFrozen(mem.buffer)].join());
  const i = new WebAssembly.Instance(mod, { m: { mem } });
  t(() => i.exports.mem === mem);
  t(() => i.exports.load(8));
  t(() => { new Int32Array(mem.buffer)[3] = 99; return i.exports.grow(1); });
  t(() => mem.buffer.byteLength);
  t(() => list[0] === list[1]);
  parentPort.postMessage({ out, back: mem, mod });
});`;
pa("worker", async () => {
  new Int32Array(smem.buffer)[2] = 1234;
  const before = smem.buffer.byteLength;
  const { out, back, mod: m2 } = await inWorker(workerCode, { mod, mem: smem, list: [mod, mod] });
  return [...out, before, smem.buffer.byteLength, new Int32Array(smem.buffer)[3], back === smem,
    back.buffer.byteLength, Object.prototype.toString.call(m2), m2 === mod].join(" | ");
});
pa("worker refuses", async () => {
  const w = new (workerThreads().Worker)("", { eval: true });
  const out = [];
  for (const v of [new W.Memory({ initial: 1 }), new W.Global({ value: "i32" }, 1), tag]) {
    try { w.postMessage(v); out.push("posted"); } catch (e) { out.push(e.constructor.name + " " + e.name + ": " + e.message); }
  }
  try { w.postMessage(mod, [mod]); out.push("posted"); } catch (e) { out.push(e.name + ": " + e.message); }
  await w.terminate();
  return out.join(" | ");
});
