// The namespace's shape and every operation's argument errors.
const W = WebAssembly;
p("names", () => Object.getOwnPropertyNames(W).join());
p("keys", () => Object.keys(W).join());
for (const c of ["Module", "Instance", "Memory", "Table", "Global", "Tag", "Exception", "CompileError", "LinkError", "RuntimeError", "Suspending", "SuspendError"]) {
  p(c + " length", () => W[c].length);
  p(c + " proto names", () => Object.getOwnPropertyNames(W[c].prototype).join());
  p(c + " tag", () => String(W[c].prototype[Symbol.toStringTag]));
  p(c + " call", () => W[c]());
}
p("Module statics", () => Object.getOwnPropertyNames(W.Module).join());
p("String(WebAssembly)", () => String(W));
const m = new W.Module(emptyModule);
const bad = [undefined, null, true, "", Symbol(), 1, {}, [], m];
for (const [i, v] of bad.entries()) {
  p("validate " + i, () => W.validate(v));
  p("Module " + i, () => new W.Module(v));
  p("Module.imports " + i, () => W.Module.imports(v));
  p("Module.exports " + i, () => W.Module.exports(v));
  p("Module.customSections " + i, () => W.Module.customSections(v, ""));
  p("Instance " + i, () => new W.Instance(v));
  p("Instance imports " + i, () => new W.Instance(m, v));
  p("Memory " + i, () => new W.Memory(v));
  p("Table " + i, () => new W.Table(v));
  p("Global " + i, () => new W.Global(v));
  p("Tag " + i, () => new W.Tag(v));
  p("Exception " + i, () => new W.Exception(v));
  p("Suspending " + i, () => new W.Suspending(v));
  p("promising " + i, () => W.promising(v));
  pa("compile " + i, () => W.compile(v));
  pa("instantiate " + i, () => W.instantiate(v));
  pa("instantiate imports " + i, () => W.instantiate(m, v));
  pa("instantiate bytes imports " + i, () => W.instantiate(emptyModule, v));
}
p("customSections no name", () => W.Module.customSections(m));
p("Module empty", () => new W.Module(new Uint8Array(0)));
p("Module empty buffer", () => new W.Module(new ArrayBuffer(0)));
p("Module DataView", () => new W.Module(new DataView(emptyModule.buffer)));
p("Module SAB view", () => new W.Module(new Uint8Array(new SharedArrayBuffer(8))));
p("validate empty", () => W.validate(new Uint8Array(0)));
p("validate DataView", () => W.validate(new DataView(emptyModule.buffer)));
pa("compile empty", () => W.compile(new Uint8Array(0)));
pa("instantiate empty", () => W.instantiate(new Uint8Array(0)));
const receivers = [undefined, null, 1, {}, W.Memory.prototype];
for (const [i, r] of receivers.entries()) {
  p("exports getter " + i, () => Object.getOwnPropertyDescriptor(W.Instance.prototype, "exports").get.call(r));
  p("buffer getter " + i, () => Object.getOwnPropertyDescriptor(W.Memory.prototype, "buffer").get.call(r));
  p("Memory.grow " + i, () => W.Memory.prototype.grow.call(r, 0));
  p("toFixedLengthBuffer " + i, () => W.Memory.prototype.toFixedLengthBuffer.call(r));
  p("toResizableBuffer " + i, () => W.Memory.prototype.toResizableBuffer.call(r));
  p("length getter " + i, () => Object.getOwnPropertyDescriptor(W.Table.prototype, "length").get.call(r));
  p("Table.grow " + i, () => W.Table.prototype.grow.call(r, 0));
  p("Table.get " + i, () => W.Table.prototype.get.call(r, 0));
  p("Table.set " + i, () => W.Table.prototype.set.call(r, 0));
  p("value getter " + i, () => Object.getOwnPropertyDescriptor(W.Global.prototype, "value").get.call(r));
  p("value setter " + i, () => Object.getOwnPropertyDescriptor(W.Global.prototype, "value").set.call(r, 1));
  p("valueOf " + i, () => W.Global.prototype.valueOf.call(r));
  p("getArg " + i, () => W.Exception.prototype.getArg.call(r, W.JSTag, 0));
  p("is " + i, () => W.Exception.prototype.is.call(r, W.JSTag));
}
p("getter names", () => [Object.getOwnPropertyDescriptor(W.Instance.prototype, "exports").get.name,
  Object.getOwnPropertyDescriptor(W.Global.prototype, "value").set.name].join());
p("instance exports", () => { const i = new W.Instance(m); return [Object.getPrototypeOf(i.exports), Object.isFrozen(i.exports), i.exports === i.exports].join(); });
p("CompileError", () => { const e = new W.CompileError("x", { cause: 1 }); return [String(e), e.cause, e instanceof Error, Object.prototype.toString.call(e)].join(); });
p("LinkError call", () => String(W.LinkError("y")));
p("RuntimeError proto", () => [W.RuntimeError.prototype.name, JSON.stringify(W.RuntimeError.prototype.message), Object.getPrototypeOf(W.RuntimeError) === Error].join());
p("SuspendError", () => String(new W.SuspendError("z")));
p("JSTag", () => [Object.prototype.toString.call(W.JSTag), W.JSTag === W.JSTag, Object.getOwnPropertyDescriptor(W, "JSTag").writable].join());
