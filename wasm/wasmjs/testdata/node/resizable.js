// toResizableBuffer and toFixedLengthBuffer of a shared memory: a growable
// SharedArrayBuffer that follows the memory however it grows -- by wasm, by
// Memory.prototype.grow, by the buffer's grow, in any thread.
const W = WebAssembly;
const builder = new WasmModuleBuilder();
builder.addImportedMemory("m", "mem", 1, 4, true);
builder.addFunction("grow", kSig_i_i).addBody([kExprLocalGet, 0, kExprMemoryGrow, 0]).exportFunc();
builder.addFunction("size", kSig_i_v).addBody([kExprMemorySize, 0]).exportFunc();
const mod = builder.toModule();
const desc = (b) => [Object.prototype.toString.call(b), b.growable, b.byteLength, b.maxByteLength,
  Object.isFrozen(b), Object.isExtensible(b)].join();

const m = new W.Memory({ initial: 1, maximum: 4, shared: true });
const fixed = m.buffer;
let rb;
p("toResizableBuffer", () => { rb = m.toResizableBuffer(); return desc(rb); });
p("again the same", () => m.toResizableBuffer() === rb);
p("buffer is it", () => m.buffer === rb);
p("the fixed one stays", () => [desc(fixed), fixed === rb].join());
for (const [label, n] of [["a byte more", 65537], ["smaller", 100], ["the same", 65536], ["past the maximum", 65536 * 5],
  ["-1", -1], ["2^53", 2 ** 53], ["NaN", NaN], ["a string", "131072"], ["missing", undefined], ["Infinity", Infinity]]) {
  p("grow " + label, () => { rb.grow(n); return [rb.byteLength, m.buffer === rb].join(); });
}
p("grow a BigInt", () => rb.grow(1n));
p("grow a Symbol", () => rb.grow(Symbol()));
p("grow valueOf throws", () => rb.grow({ valueOf() { throw new SyntaxError("v") } }));
p("after", () => [rb.byteLength, m.buffer === rb, m.buffer.byteLength, fixed.byteLength].join());
p("Memory.prototype.grow", () => [m.grow(1), rb.byteLength, m.buffer === rb].join());
p("wasm grows it", () => {
  const i = new W.Instance(mod, { m: { mem: m } });
  return [i.exports.grow(0), i.exports.size(), rb.byteLength].join();
});
p("tracking view", () => {
  const v = new Int32Array(rb);
  const before = v.length;
  return [before, Atomics.notify(v, 0, 1), Atomics.wait(v, 0, 1, 0), Atomics.wait(v, 0, 0, 0)].join();
});
p("slice", () => desc(rb.slice(0, 10)));
let fixed2;
p("toFixedLengthBuffer", () => { fixed2 = m.toFixedLengthBuffer(); return [desc(fixed2), fixed2 === rb, m.buffer === fixed2, rb.byteLength].join(); });
p("toFixedLengthBuffer again", () => m.toFixedLengthBuffer() === fixed2);
p("the growable one still grows", () => { rb.grow(65536 * 4); return [rb.byteLength, fixed2.byteLength, m.buffer.byteLength].join(); });
p("toResizableBuffer anew", () => { const r2 = m.toResizableBuffer(); return [r2 === rb, desc(r2)].join(); });
p("grow past the maximum", () => m.toResizableBuffer().grow(65536 * 5));
p("grow at the maximum", () => m.toResizableBuffer().grow(65536 * 4));
p("no pages", () => desc(new W.Memory({ initial: 0, maximum: 0, shared: true }).toResizableBuffer()));
p("memory64", () => {
  const b = new W.Memory({ initial: 1n, maximum: 3n, shared: true, address: "i64" }).toResizableBuffer();
  b.grow(65536 * 3);
  return desc(b);
});
p("a maximum of 65536 pages", () => new W.Memory({ initial: 1, maximum: 65536, shared: true }).toResizableBuffer().maxByteLength);
p("clone it", () => desc(structuredClone(m.toResizableBuffer())));
p("clone in a view", () => structuredClone({ v: new Int32Array(m.toResizableBuffer()) }).v.length);
p("clone the fixed one", () => desc(structuredClone(m.toFixedLengthBuffer())));
p("transfer", () => structuredClone(rb, { transfer: [rb] }));
p("not a memory", () => W.Memory.prototype.toResizableBuffer.call({}));
p("resizable unshared", () => desc(new W.Memory({ initial: 1, maximum: 2 }).toResizableBuffer()));

const m2 = new W.Memory({ initial: 1, maximum: 4, shared: true });
const rb2 = m2.toResizableBuffer();
const workerCode = `
import { parentPort } from "node:worker_threads";
parentPort.once("message", ({ mod, mem }) => {
  const out = [];
  const i = new WebAssembly.Instance(mod, { m: { mem } });
  const b = mem.toResizableBuffer();
  out.push(b.growable, b.byteLength, b.maxByteLength);
  out.push(i.exports.grow(1), b.byteLength);
  b.grow(65536 * 3);
  out.push(b.byteLength, i.exports.size(), mem.buffer === b);
  new Int32Array(b)[40000] = 7;
  parentPort.postMessage(out.join());
});`;
pa("worker grows it", async () => {
  const got = await inWorker(workerCode, { mod, mem: m2 });
  return [got, rb2.byteLength, new Int32Array(rb2)[40000], m2.buffer === rb2].join(" | ");
});
pa("worker hears a messageerror", () => inWorker(`
import { parentPort } from "node:worker_threads";
parentPort.on("message", (m) => parentPort.postMessage("message " + m.byteLength));
parentPort.on("messageerror", (e) => parentPort.postMessage("messageerror " + e.constructor.name + ": " + e.message));`,
  m2.toResizableBuffer()));
