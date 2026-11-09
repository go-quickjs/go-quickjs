// wasmbench's worker for Node: given a request on its standard input, it
// compiles a module, instantiates it, runs it and checks each run's result,
// and writes what it measured to its standard output as JSON, as the Go
// runners' worker package does. WASI programs run on node:wasi.
import { closeSync, openSync, readFileSync, unlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { performance } from "node:perf_hooks";
import { WASI } from "node:wasi";

const req = JSON.parse(readFileSync(0, "utf8"));
const res = { compile_ns: 0, runs_ns: [], runs_cpu_ns: [] };
const ns = (ms) => Math.round(ms * 1e6);

class CheckError extends Error {}

let stage = "compile";
try {
  const bin = readFileSync(req.wasm);
  let t0 = performance.now();
  const mod = new WebAssembly.Module(bin);
  res.compile_ns = ns(performance.now() - t0);

  let run;
  if (req.kind === "call") {
    stage = "instantiate";
    t0 = performance.now();
    const inst = new WebAssembly.Instance(mod, {});
    res.instantiate_ns = ns(performance.now() - t0);
    const f = inst.exports[req.export];
    const arg = req.arg ?? 0;
    run = () => {
      const got = BigInt.asIntN(64, f(arg)).toString();
      if (got !== req.want) {
        throw new CheckError(`${req.export}(${arg}) = ${got}, want ${req.want}`);
      }
    };
  } else if (req.kind === "wasi") {
    const out = join(tmpdir(), `wasmbench-${process.pid}.out`);
    run = () => {
      const fd = openSync(out, "w+");
      let code;
      try {
        const wasi = new WASI({
          version: "preview1",
          args: req.args,
          env: {},
          stdout: fd,
          returnOnExit: true,
        });
        const inst = new WebAssembly.Instance(mod, wasi.getImportObject());
        code = wasi.start(inst);
      } finally {
        closeSync(fd);
      }
      const got = readFileSync(out, "utf8");
      unlinkSync(out);
      if (code !== 0) {
        throw new Error(`exit code ${code}`);
      }
      if (got !== req.want) {
        throw new CheckError(`${JSON.stringify(req.args)} wrote ${JSON.stringify(got)}, want ${JSON.stringify(req.want)}`);
      }
    };
  } else {
    stage = "request";
    throw new Error(`unknown kind ${req.kind}`);
  }

  for (let i = 0; i < req.warmup + req.reps; i++) {
    stage = "run";
    const c0 = process.cpuUsage();
    const t0 = performance.now();
    try {
      run();
    } finally {
      const t1 = performance.now();
      const c = process.cpuUsage(c0);
      res.runs_ns.push(ns(t1 - t0));
      res.runs_cpu_ns.push((c.user + c.system) * 1000);
    }
  }
} catch (e) {
  res.stage = e instanceof CheckError ? "check" : stage;
  res.error = e instanceof CheckError ? e.message : String(e && e.stack ? e.stack : e);
}
res.peak_rss = process.resourceUsage().maxRSS * 1024;
process.stdout.write(JSON.stringify(res) + "\n");
process.exitCode = res.error ? 1 : 0;
