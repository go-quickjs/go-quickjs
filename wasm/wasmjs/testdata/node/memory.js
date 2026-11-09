// Memory: the constructor, grow and the life of its buffers.
const W = WebAssembly;
const descs = [
  {}, { initial: 1 }, { initial: -1 }, { initial: NaN }, { initial: Infinity }, { initial: 2 ** 32 },
  { initial: 65536 }, { initial: 65537 }, { initial: "2" }, { initial: true }, { initial: 2, maximum: 1 },
  { initial: 1, maximum: 65537 }, { initial: 1, maximum: undefined }, { initial: 1, shared: true },
  { initial: 1, maximum: 2, shared: true }, { initial: 1, address: "i64" }, { initial: 1n, address: "i64" },
  { initial: -1n, address: "i64" }, { initial: 2n ** 64n, address: "i64" }, { initial: 1n, maximum: 2n ** 64n - 1n, address: "i64" },
  { initial: 262145n, address: "i64" }, { initial: 1, address: "i16" }, { initial: 1, address: "I32" },
  { initial: 1, address: undefined }, { initial: 1n, address: "i64", maximum: 1 },
  { initial: { valueOf() { throw new SyntaxError("valueOf") } } },
  { get initial() { throw new SyntaxError("getter") } },
];
for (const [i, d] of descs.entries()) {
  const label = "Memory " + i;
  p(label, () => { const m = new W.Memory(d); return [m.buffer.byteLength, Object.prototype.toString.call(m.buffer)].join(); });
}
p("descriptor order", () => {
  const log = [];
  const d = new Proxy({}, { get(t, k) { log.push(String(k)); return k === "initial" ? 1 : undefined } });
  new W.Memory(d);
  return log.join();
});
const m = new W.Memory({ initial: 1, maximum: 3 });
const b0 = m.buffer;
p("buffer idempotent", () => m.buffer === b0);
p("buffer props", () => [b0.byteLength, b0.resizable, b0.detached, Object.isExtensible(b0), Object.isFrozen(b0)].join());
p("transfer", () => b0.transfer());
p("transferToFixedLength", () => b0.transferToFixedLength());
p("structuredClone transfer", () => structuredClone(b0, { transfer: [b0] }));
p("grow 0", () => m.grow(0));
p("after grow 0", () => [b0.detached, b0.byteLength, m.buffer === b0, m.buffer.byteLength].join());
const b1 = m.buffer;
p("grow 1", () => m.grow(1));
p("after grow 1", () => [b1.detached, m.buffer.byteLength].join());
p("grow past max", () => m.grow(2));
p("grow -1", () => m.grow(-1));
p("grow 2^32", () => m.grow(2 ** 32));
p("grow string", () => m.grow("1"));
p("grow missing", () => m.grow());
p("grow bigint", () => m.grow(1n));
p("unbounded grow", () => new W.Memory({ initial: 1 }).grow(65536));
p("unbounded grow to the limit", () => new W.Memory({ initial: 0 }).grow(65537));
p("bounded at the limit", () => new W.Memory({ initial: 1, maximum: 65536 }).grow(65536));
p("toResizableBuffer without max", () => new W.Memory({ initial: 1 }).toResizableBuffer());
const r = new W.Memory({ initial: 1, maximum: 4 });
const fixed = r.buffer;
const rb = r.toResizableBuffer();
p("resizable", () => [rb.resizable, rb.maxByteLength, rb.byteLength, fixed.detached, r.buffer === rb, r.toResizableBuffer() === rb].join());
p("resize not page", () => rb.resize(65536 + 1));
p("resize shrink", () => rb.resize(0));
p("resize past max", () => rb.resize(5 * 65536));
p("resize", () => { rb.resize(2 * 65536); return [rb.byteLength, r.buffer === rb].join(); });
p("grow keeps resizable", () => { r.grow(1); return [rb.byteLength, r.buffer === rb, rb.detached].join(); });
p("resizable transfer", () => rb.transfer());
const fl = r.toFixedLengthBuffer();
p("toFixedLengthBuffer", () => [fl.resizable, fl.byteLength, rb.detached, r.buffer === fl, r.toFixedLengthBuffer() === fl].join());
p("views across grow", () => {
  const mm = new W.Memory({ initial: 1, maximum: 2 });
  const u = new Uint8Array(mm.buffer);
  u[5] = 9;
  mm.grow(1);
  return [u.length, new Uint8Array(mm.buffer)[5], mm.buffer.byteLength].join();
});
p("resizable views", () => {
  const mm = new W.Memory({ initial: 1, maximum: 2 });
  const buf = mm.toResizableBuffer();
  const u = new Uint8Array(buf);
  mm.grow(1);
  return [u.length, buf.byteLength].join();
});
const s = new W.Memory({ initial: 1, maximum: 2, shared: true });
const sb = s.buffer;
p("shared buffer", () => [Object.prototype.toString.call(sb), sb.byteLength, Object.isFrozen(sb), sb.growable, s.buffer === sb].join());
p("shared grow", () => s.grow(1));
p("after shared grow", () => [sb.byteLength, s.buffer === sb, s.buffer.byteLength].join());
const m64 = new W.Memory({ initial: 1n, maximum: 3n, address: "i64" });
p("memory64 grow", () => m64.grow(1n));
p("memory64 grow number", () => m64.grow(1));
p("memory64 grow too far", () => m64.grow(5n));
p("memory64 buffer", () => m64.buffer.byteLength);
p("memory64 grow negative", () => m64.grow(-1n));
// From wasm.
const b = new WasmModuleBuilder();
b.addImportedMemory("m", "mem", 1, 3);
b.addFunction("grow", kSig_i_i).addBody([kExprLocalGet, 0, kExprMemoryGrow, kMemoryZero]).exportFunc();
b.addFunction("size", kSig_i_v).addBody([kExprMemorySize, kMemoryZero]).exportFunc();
b.addFunction("store", kSig_v_ii).addBody([kExprLocalGet, 0, kExprLocalGet, 1, kExprI32StoreMem, 0, 0]).exportFunc();
const wm = new W.Memory({ initial: 1, maximum: 3 });
const e = b.instantiate({ m: { mem: wm } }).exports;
const before = wm.buffer;
p("wasm grow", () => e.grow(1));
p("after wasm grow", () => [before.detached, wm.buffer.byteLength, e.size()].join());
p("wasm grow fails", () => e.grow(5));
p("wasm store seen", () => { e.store(8, 0x01020304); return new Uint8Array(wm.buffer)[8]; });
p("wasm sees JS write", () => { new Uint8Array(wm.buffer)[100] = 7; return new Uint8Array(wm.buffer).slice(100, 101)[0]; });
