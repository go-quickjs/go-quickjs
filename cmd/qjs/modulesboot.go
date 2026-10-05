package main

// modulesJS is the part of node's module system that is about JavaScript
// objects: Module, require, require.cache and the order modules run in, as
// node's own loader keeps them. Finding and reading files is the Go side's,
// which host is.
const modulesJS = `(host) => {
  "use strict";

  const cache = Object.create(null);
  let main;

  function codedError(Ctor, code, message) {
    const e = new Ctor(message);
    e.code = code;
    return e;
  }

  class Module {
    constructor(id = "", parent) {
      this.id = id;
      this.path = id === "" ? host.cwd : host.dirname(id);
      this.exports = {};
      this.filename = null;
      this.loaded = false;
      this.children = [];
      this.paths = host.lookupPaths(this.path);
      // node keeps parent, and calls it deprecated.
      Object.defineProperty(this, "parent", { value: parent, writable: true, configurable: true });
    }
    require(id) {
      return load(id, this);
    }
  }

  // The stack of files whose requires led to a module that was not found,
  // as node lists it.
  function requireStack(parent) {
    const stack = [];
    for (let m = parent; m; m = m.parent) stack.push(m.filename || m.id);
    return stack;
  }

  function resolve(request, parent) {
    if (typeof request !== "string") {
      throw codedError(TypeError, "ERR_INVALID_ARG_TYPE",
        'The "id" argument must be of type string. Received ' + (request === null ? "null" : typeof request));
    }
    if (request === "") {
      throw codedError(TypeError, "ERR_INVALID_ARG_VALUE",
        "The argument 'id' must be a non-empty string. Received ''");
    }
    try {
      return host.resolve(request, parent ? parent.path : host.cwd);
    } catch (e) {
      if (e && e.code === "MODULE_NOT_FOUND") {
        const stack = requireStack(parent);
        if (stack.length > 0) e.message += "\nRequire stack:\n- " + stack.join("\n- ");
        e.requireStack = stack;
      }
      throw e;
    }
  }

  function adopt(parent, child) {
    if (parent && !parent.children.includes(child)) parent.children.push(child);
  }

  function load(request, parent, isMain) {
    if (typeof request === "string" && host.isBuiltin(request)) return host.requireBuiltin(request);
    const filename = resolve(request, parent);
    const cached = cache[filename];
    if (cached !== undefined) {
      adopt(parent, cached);
      return cached.exports;
    }
    const mod = new Module(filename, parent);
    mod.filename = filename;
    if (isMain) {
      main = mod;
      mod.id = ".";
    }
    cache[filename] = mod;
    adopt(parent, mod);
    let ok = false;
    try {
      if (host.extname(filename).toLowerCase() === ".json") {
        try {
          mod.exports = JSON.parse(host.read(filename));
        } catch (e) {
          e.message = filename + ": " + e.message;
          throw e;
        }
      } else if (host.isESM(filename)) {
        const ns = host.requireESM(filename);
        mod.exports = "module.exports" in ns ? ns["module.exports"] : ns;
      } else {
        host.compile(filename).call(mod.exports, mod.exports, makeRequire(mod), mod, filename, mod.path);
      }
      ok = true;
    } finally {
      if (!ok) {
        delete cache[filename];
        if (parent) {
          const i = parent.children.indexOf(mod);
          if (i >= 0) parent.children.splice(i, 1);
        }
      }
    }
    mod.loaded = true;
    return mod.exports;
  }

  function makeRequire(mod) {
    const require = function require(id) {
      return load(id, mod);
    };
    require.resolve = function resolve(request) {
      if (typeof request === "string" && host.isBuiltin(request)) return request;
      return resolveFor(request, mod);
    };
    require.resolve.paths = function paths(request) {
      return host.isBuiltin(request) ? null : host.lookupPaths(mod.path);
    };
    require.cache = cache;
    Object.defineProperty(require, "main", { get: () => main, enumerable: true });
    require.extensions = Object.create(null);
    for (const ext of [".js", ".json", ".node"]) require.extensions[ext] = function () {};
    return require;
  }
  const resolveFor = resolve;

  function createRequire(from) {
    if (from === null || (typeof from !== "string" && typeof from !== "object")) {
      throw codedError(TypeError, "ERR_INVALID_ARG_VALUE",
        "The argument 'filename' must be a file URL object, file URL string, or absolute path string. Received " + String(from));
    }
    const filename = host.toPath(String(from && from.href !== undefined ? from.href : from));
    const mod = new Module(filename, null);
    mod.filename = filename;
    return makeRequire(mod);
  }

  Module._cache = cache;
  Module.builtinModules = Object.freeze(host.builtinModules.slice());
  Module.isBuiltin = (name) => typeof name === "string" && host.isBuiltin(name);
  Module.createRequire = createRequire;
  Module.Module = Module;

  // The require and module of code that is not a file, as node gives -e and
  // the REPL theirs: resolving from the working directory.
  function evalGlobals(name) {
    const mod = new Module("", null);
    mod.id = name;
    mod.filename = host.join(host.cwd, name);
    const define = (key, value) =>
      Object.defineProperty(globalThis, key, { value, writable: true, configurable: true });
    define("module", mod);
    define("exports", mod.exports);
    define("require", makeRequire(mod));
    define("__filename", name);
    define("__dirname", ".");
  }

  return {
    Module,
    load,
    createRequire,
    runMain: (filename) => load(filename, null, true),
    evalGlobals,
  };
}`
