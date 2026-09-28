package stdlib

// workerJS is node:worker_threads, the web's Worker, and a worker's view of
// itself, in script.
const workerJS = `(function (host) {
  "use strict";

  const EventEmitter = host.EventEmitter;
  const info = host.info();
  const kState = Symbol("state");
  const kEvent = Symbol("event");
  const SHARE_ENV = Symbol.for("nodejs.worker_threads.SHARE_ENV");

  function codeError(Kind, code, message) {
    const e = new Kind(message);
    e.code = code;
    return e;
  }

  // isPath is what node's Worker takes a filename for: absolute, or relative
  // to the working directory and saying so.
  function isPath(s) {
    return s.startsWith("./") || s.startsWith("../") || s.startsWith("/") ||
      s.startsWith(".\\") || s.startsWith("..\\") || s.startsWith("\\") || /^[A-Za-z]:[\\/]/.test(s);
  }

  // env is what a worker's process.env starts as: the parent's as it is now,
  // unless the options say otherwise.
  function envOf(env) {
    if (env === undefined || env === SHARE_ENV) {
      if (globalThis.process && globalThis.process.env) return {env: {...globalThis.process.env}, hasEnv: true};
      return {env: {}, hasEnv: false};
    }
    if (env === null || typeof env !== "object") {
      throw codeError(TypeError, "ERR_INVALID_ARG_TYPE", 'The "options.env" property must be of type object.');
    }
    const out = {};
    for (const k of Object.keys(env)) out[k] = String(env[k]);
    return {env: out, hasEnv: true};
  }

  function listOf(transfer) {
    if (transfer === undefined || transfer === null) return [];
    if (typeof transfer !== "object" || typeof transfer[Symbol.iterator] !== "function") {
      throw codeError(TypeError, "ERR_INVALID_ARG_TYPE", 'The "options.transferList" property must be an iterable.');
    }
    return [...transfer];
  }

  const environmentData = new Map();

  // --- node:worker_threads ------------------------------------------------

  class Worker extends EventEmitter {
    constructor(filename, options) {
      super();
      options = options === undefined || options === null ? {} : options;
      const isEval = !!options.eval;
      let spec;
      if (typeof filename === "string") {
        spec = filename;
        if (!isEval && !isPath(spec) && !spec.startsWith("file:") && !spec.startsWith("data:")) {
          throw codeError(TypeError, "ERR_WORKER_PATH",
            "The worker script or module filename must be an absolute path or a relative path " +
            "starting with './' or '../'. Received \"" + spec + "\"");
        }
      } else if (filename !== null && typeof filename === "object" && typeof filename.href === "string" && !isEval) {
        spec = filename.href;
      } else {
        throw codeError(TypeError, "ERR_INVALID_ARG_TYPE",
          'The "filename" argument must be of type string or an instance of URL.');
      }
      const {env, hasEnv} = envOf(options.env);
      const argv = options.argv === undefined ? [] : [...options.argv].map(String);
      const started = host.start(this, spec, [options.workerData, environmentData],
        listOf(options.transferList),
        {eval: isEval, argv, env, hasEnv, name: options.name === undefined ? "" : String(options.name), web: false, type: ""});
      const state = {port: started.port, threadId: started.threadId, exited: false, waiting: []};
      Object.defineProperty(this, kState, {value: state});
      state.port.addEventListener("message", (e) => this.emit("message", e.data));
      state.port.addEventListener("messageerror", (e) => this.emit("messageerror", e.data));
    }
    get threadId() { return this[kState].exited ? -1 : this[kState].threadId; }
    postMessage(value, transfer) { this[kState].port.postMessage(value, transfer); }
    // terminate stops the worker, and settles with its exit code once it has.
    terminate() {
      const state = this[kState];
      if (state.exited) return Promise.resolve(undefined);
      host.terminate(this);
      return new Promise((resolve) => state.waiting.push(resolve));
    }
    ref() { host.ref(this, true); }
    unref() { host.ref(this, false); }
    [kEvent](type, value) {
      const state = this[kState];
      switch (type) {
        case "online":
          this.emit("online");
          break;
        case "error":
          this.emit("error", value);
          break;
        case "exit":
          state.exited = true;
          for (const resolve of state.waiting.splice(0)) resolve(value);
          this.emit("exit", value);
          break;
      }
    }
  }

  function setEnvironmentData(key, value) {
    if (value === undefined) environmentData.delete(key);
    else environmentData.set(key, value);
  }
  function getEnvironmentData(key) { return environmentData.get(key); }

  // --- The web's Worker ---------------------------------------------------

  const errorEventState = new WeakMap();
  function errorEventOf(e) {
    const s = errorEventState.get(e);
    if (s === undefined) throw new TypeError('Value of "this" must be of type ErrorEvent');
    return s;
  }

  class ErrorEvent extends Event {
    constructor(type, init = {}) {
      super(type, init);
      const {message = "", filename = "", lineno = 0, colno = 0, error = undefined} = init || {};
      errorEventState.set(this, {message: String(message), filename: String(filename),
        lineno: lineno >>> 0, colno: colno >>> 0, error});
    }
    get message() { return errorEventOf(this).message; }
    get filename() { return errorEventOf(this).filename; }
    get lineno() { return errorEventOf(this).lineno; }
    get colno() { return errorEventOf(this).colno; }
    get error() { return errorEventOf(this).error; }
  }

  const WebWorker = class Worker extends EventTarget {
    constructor(url, options = {}) {
      if (arguments.length === 0) throw new TypeError("Worker constructor: At least 1 argument required, but only 0 passed");
      super();
      const spec = url !== null && typeof url === "object" && typeof url.href === "string" ? url.href : String(url);
      options = options === undefined || options === null ? {} : options;
      const type = options.type === undefined ? "classic" : String(options.type);
      if (type !== "classic" && type !== "module") {
        throw new TypeError("Worker constructor: '" + type + "' is not a valid value for enumeration WorkerType.");
      }
      const {env, hasEnv} = envOf(undefined);
      const started = host.start(this, spec, [undefined, new Map()], [],
        {eval: false, argv: [], env, hasEnv, name: options.name === undefined ? "" : String(options.name), web: true, type});
      Object.defineProperty(this, kState, {value: {port: started.port}});
      this.onmessage = null;
      this.onmessageerror = null;
      this.onerror = null;
      started.port.addEventListener("message", (e) =>
        this.dispatchEvent(new MessageEvent("message", {data: e.data, ports: e.ports})));
      started.port.addEventListener("messageerror", (e) =>
        this.dispatchEvent(new MessageEvent("messageerror", {data: e.data})));
    }
    postMessage(message, transfer) { this[kState].port.postMessage(message, transfer); }
    terminate() { host.terminate(this); }
    // A worker's error is an error event here, and one nothing handles is
    // uncaught in the runtime that started it.
    [kEvent](type, value) {
      if (type !== "error") return;
      const message = value !== null && typeof value === "object" && "message" in value ? String(value.message) : String(value);
      if (this.dispatchEvent(new ErrorEvent("error", {message, error: value, cancelable: true}))) throw value;
    }
  };

  host.bindWorkers((worker, type, value) => worker[kEvent](type, value));

  // --- Inside a worker ----------------------------------------------------

  let parentPort = null;
  let workerData = null;
  if (!info.isMainThread) {
    parentPort = info.parentPort;
    workerData = info.data[0];
    for (const [k, v] of info.data[1]) environmentData.set(k, v);
    if (info.web) webWorkerScope(parentPort, info.name);
  }

  // webWorkerScope makes the global object a web worker's: self, name,
  // postMessage and close, and the target of what the parent posts, which
  // is dispatched once something listens.
  function webWorkerScope(port, name) {
    const g = globalThis;
    const define = (key, value) => Object.defineProperty(g, key, {value, writable: true, configurable: true});
    define("self", g);
    define("name", name);
    Object.defineProperty(g, "_listeners", {value: new Map()});
    define("addEventListener", function addEventListener(type, fn, options) {
      EventTarget.prototype.addEventListener.call(g, type, fn, options);
      if (String(type) === "message") port.start();
    });
    define("removeEventListener", function removeEventListener(type, fn) {
      EventTarget.prototype.removeEventListener.call(g, type, fn);
    });
    define("dispatchEvent", function dispatchEvent(event) {
      return EventTarget.prototype.dispatchEvent.call(g, event);
    });
    define("postMessage", function postMessage(message, transfer) { port.postMessage(message, transfer); });
    define("close", function close() { host.close(); });
    let onmessage = null;
    let onmessageerror = null;
    Object.defineProperty(g, "onmessage", {
      get() { return onmessage; },
      set(fn) {
        onmessage = typeof fn === "function" ? fn : null;
        if (onmessage) port.start();
      },
      configurable: true,
    });
    Object.defineProperty(g, "onmessageerror", {
      get() { return onmessageerror; },
      set(fn) { onmessageerror = typeof fn === "function" ? fn : null; },
      configurable: true,
    });
    port.addEventListener("message", (e) => g.dispatchEvent(new MessageEvent("message", {data: e.data, ports: e.ports})));
    port.addEventListener("messageerror", (e) => g.dispatchEvent(new MessageEvent("messageerror", {data: e.data})));
  }

  const worker_threads = {
    isMainThread: info.isMainThread,
    threadId: info.threadId,
    parentPort,
    workerData,
    resourceLimits: {},
    SHARE_ENV,
    Worker,
    MessageChannel: globalThis.MessageChannel,
    MessagePort: globalThis.MessagePort,
    BroadcastChannel: globalThis.BroadcastChannel,
    receiveMessageOnPort: host.receiveMessageOnPort,
    markAsUntransferable() {},
    isMarkedAsUntransferable() { return false; },
    setEnvironmentData,
    getEnvironmentData,
  };

  return {worker_threads, Worker: WebWorker, ErrorEvent};
})`
