// Atomics.wait, Atomics.waitAsync and Atomics.notify on a shared memory's
// buffers, and wasm's memory.atomic.wait32, wait64 and notify on the memory:
// one list of waiters, whichever side waits and whichever notifies, in one
// thread and across workers.
const W = WebAssembly;
const builder = new WasmModuleBuilder();
builder.addImportedMemory("m", "mem", 1, 4, true);
builder.addFunction("wait32", makeSig([kWasmI32, kWasmI32, kWasmI64], [kWasmI32]))
  .addBody([kExprLocalGet, 0, kExprLocalGet, 1, kExprLocalGet, 2, kAtomicPrefix, kExprI32AtomicWait, 2, 0]).exportFunc();
builder.addFunction("wait64", makeSig([kWasmI32, kWasmI64, kWasmI64], [kWasmI32]))
  .addBody([kExprLocalGet, 0, kExprLocalGet, 1, kExprLocalGet, 2, kAtomicPrefix, kExprI64AtomicWait, 3, 0]).exportFunc();
builder.addFunction("notify", kSig_i_ii)
  .addBody([kExprLocalGet, 0, kExprLocalGet, 1, kAtomicPrefix, kExprAtomicNotify, 2, 0]).exportFunc();
const mod = builder.toModule();
const mem = new W.Memory({ initial: 1, maximum: 4, shared: true });
const e = new W.Instance(mod, { m: { mem } }).exports;
const i32 = new Int32Array(mem.buffer);

// In one thread.
p("JS wait not-equal", () => Atomics.wait(i32, 0, 1, 0));
p("JS wait timed-out", () => Atomics.wait(i32, 0, 0, 0));
p("wasm wait32 not-equal", () => e.wait32(0, 1, -1n));
p("wasm wait32 timed-out", () => e.wait32(0, 0, 0n));
p("wasm wait64 timed-out", () => e.wait64(0, 0n, 1000n));
p("wasm notify none", () => e.notify(0, 1));
p("JS waitAsync not-equal", () => { const r = Atomics.waitAsync(i32, 0, 1); return [r.async, r.value].join(); });
p("JS waitAsync no time", () => { const r = Atomics.waitAsync(i32, 0, 0, 0); return [r.async, r.value].join(); });
pa("JS waitAsync, wasm notify", async () => {
  const r = Atomics.waitAsync(i32, 0, 0);
  return [r.async, e.notify(0, 1), await r.value].join();
});
pa("three waiting, notified by both", async () => {
  const log = [];
  const waits = [1, 2, 3].map((n) => Atomics.waitAsync(i32, 1, 0).value.then((v) => log.push(n + v)));
  const counts = [e.notify(4, 2), Atomics.notify(i32, 1, 5), e.notify(4, 1), Atomics.notify(i32, 1)];
  await Promise.all(waits);
  return [counts.join(), log.join()].join(" ");
});
pa("waits at another address", async () => {
  const r = Atomics.waitAsync(i32, 2, 0);
  const counts = [e.notify(0, 1), e.notify(12, 1), e.notify(8, 1)];
  return [counts.join(), await r.value].join(" ");
});
pa("BigInt64Array", async () => {
  const r = Atomics.waitAsync(new BigInt64Array(mem.buffer), 2, 0n);
  return [e.notify(20, 1), e.notify(16, 1), await r.value].join();
});
pa("a buffer of another size", async () => {
  const old = mem.buffer;
  mem.grow(1);
  const r = Atomics.waitAsync(new Int32Array(mem.buffer), 5, 0);
  return [old === mem.buffer, Atomics.notify(new Int32Array(old), 5), await r.value].join();
});
pa("the growable buffer", async () => {
  const r = Atomics.waitAsync(new Int32Array(mem.toResizableBuffer()), 6, 0);
  const n = e.notify(24, 1);
  mem.toFixedLengthBuffer();
  return [n, await r.value].join();
});
pa("a clone of the memory", async () => {
  const c = structuredClone(mem);
  const r = Atomics.waitAsync(new Int32Array(c.buffer), 7, 0);
  return [Atomics.notify(new Int32Array(mem.buffer), 7), await r.value].join();
});
pa("the timeout", async () => {
  // Node's loop is not held by the wait, so a timer holds it.
  const hold = setTimeout(() => {}, 10000);
  const r = Atomics.waitAsync(i32, 8, 0, 5);
  const v = await r.value;
  clearTimeout(hold);
  return [v, e.notify(32, 1)].join();
});
p("an unshared memory", () => {
  const u = new W.Memory({ initial: 1 });
  return [Atomics.notify(new Int32Array(u.buffer), 0, 1)].join();
});

// Across workers.
const { Worker } = workerThreads();
const workerCode = `
import { parentPort } from "node:worker_threads";
parentPort.once("message", async ({ mod, mem, op }) => {
  const e = new WebAssembly.Instance(mod, { m: { mem } }).exports;
  const i32 = new Int32Array(mem.buffer);
  let r;
  switch (op) {
    case "wasm wait": r = e.wait32(64, 0, -1n); break;
    case "wasm wait64": r = e.wait64(64, 0n, -1n); break;
    case "JS wait": r = Atomics.wait(i32, 16, 0); break;
    case "JS waitAsync": {
      // Node's loop is not held by the wait, so a timer holds it.
      const hold = setInterval(() => {}, 1000);
      r = await Atomics.waitAsync(i32, 16, 0).value;
      clearInterval(hold);
      break;
    }
    case "wasm wait times out": r = e.wait64(72, 0n, 10000000n); break;
    case "JS wait times out": r = Atomics.wait(i32, 18, 0, 10); break;
    case "wasm notify": r = e.notify(80, 1); break;
    case "JS notify": r = Atomics.notify(i32, 20, 1); break;
  }
  parentPort.postMessage(r);
});`;
function start(op) {
  return new Promise((resolve, reject) => {
    const w = new Worker(workerCode, { eval: true });
    w.on("error", reject);
    w.on("message", (m) => w.terminate().then(() => resolve(m)));
    w.postMessage({ mod, mem, op });
  });
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// notifyAll notifies until n have been woken.
async function notifyAll(n, notify) {
  let woken = 0;
  for (let i = 0; i < 5000 && woken < n; i++) {
    woken += notify();
    if (woken < n) await sleep(2);
  }
  return woken;
}
for (const [waiter, notifier] of [["wasm wait", "JS"], ["wasm wait", "wasm"], ["wasm wait64", "JS"],
  ["JS wait", "wasm"], ["JS wait", "JS"], ["JS waitAsync", "wasm"]]) {
  pa(waiter + " in a worker, " + notifier + " notify", async () => {
    const done = start(waiter);
    const at = waiter.startsWith("wasm") ? 64 : 16 * 4;
    const woken = await notifyAll(1, notifier === "JS" ? () => Atomics.notify(i32, at / 4, 1) : () => e.notify(at, 1));
    return [woken, await done].join();
  });
}
pa("two workers, one notify for all", async () => {
  const done = [start("wasm wait"), start("JS wait")];
  // Both wait at byte 64: the wasm one there, the JS one at index 16.
  const woken = await notifyAll(2, () => e.notify(64, 2));
  return [woken, ...(await Promise.all(done))].join();
});
pa("timeouts in a worker", async () => [await start("wasm wait times out"), await start("JS wait times out")].join());
pa("a worker notifies", async () => {
  const r1 = Atomics.waitAsync(i32, 20, 0);
  const r2 = Atomics.waitAsync(i32, 20, 0);
  const counts = [await start("wasm notify"), await start("JS notify")];
  return [counts.join(), await r1.value, await r2.value].join();
});
