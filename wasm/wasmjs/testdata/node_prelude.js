// What every comparison script is run with, in Node and in go-quickjs: p
// records what a call returns or throws, pa what a promise settles with, in
// order, as lines of out.
var out = [];
var chain = Promise.resolve();
function fmt(r) {
  if (typeof r === "bigint") return r + "n";
  if (typeof r === "string") return JSON.stringify(r);
  if (typeof r === "function") return "function " + r.name + "/" + r.length;
  if (typeof r === "object" && r !== null) {
    if (Array.isArray(r)) return "[" + r.map(fmt).join(", ") + "]";
    return Object.prototype.toString.call(r);
  }
  if (Object.is(r, -0)) return "-0";
  return String(r);
}
function show(e) {
  if (e !== null && typeof e === "object" && "message" in e) {
    return (e.constructor && e.constructor.name) + ": " + e.message;
  }
  return "threw " + fmt(e);
}
function p(label, f) {
  try {
    out.push(label + " -> " + fmt(f()));
  } catch (e) {
    out.push(label + " -> " + show(e));
  }
}
function pa(label, f) {
  chain = chain.then(async () => {
    try {
      out.push(label + " -> " + fmt(await f()));
    } catch (e) {
      out.push(label + " -> " + show(e));
    }
  });
}
function bytes(...b) { return new Uint8Array(b); }
const emptyModule = bytes(0, 0x61, 0x73, 0x6d, 1, 0, 0, 0);
// workerThreads is node:worker_threads, which the go-quickjs runner imports
// as require_worker_threads.
function workerThreads() {
  return typeof require_worker_threads === "object" ? require_worker_threads : process.getBuiltinModule("node:worker_threads");
}
// inWorker runs code, a module, in a worker, posts it data, and settles with
// the first message it posts back, once it has ended.
function inWorker(code, data) {
  return new Promise((resolve, reject) => {
    const w = new (workerThreads().Worker)(code, { eval: true });
    w.on("error", reject);
    w.on("message", (m) => w.terminate().then(() => resolve(m)));
    w.postMessage(data);
  });
}
