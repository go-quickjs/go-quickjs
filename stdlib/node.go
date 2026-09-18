package stdlib

import (
	quickjs "github.com/go-quickjs/go-quickjs"
)

// NodeModules installs the modules a program written for node expects to find
// that need no capability at all: events, util, assert and buffer.
//
// None of them can reach outside the process -- an EventEmitter is a list of
// functions, a Buffer is bytes, assert throws -- so they are installed with the
// rest of the pure computation rather than being asked for.
//
// Buffer is also a global, as it is in node, because a great deal of code
// assumes it is there.
func NodeModules(rt *quickjs.Runtime) error {
	host := rt.NewObject()
	if err := host.Set("inspect", func(r *quickjs.Runtime, v quickjs.Value) string {
		return Inspect(r, v)
	}); err != nil {
		return err
	}
	api, err := evalWithHost(rt, "<node>", nodeJS, host)
	if err != nil {
		return err
	}

	for _, mod := range []struct {
		name   string
		member string
	}{
		{"events", "events"},
		{"util", "util"},
		{"assert", "assert"},
		{"buffer", "buffer"},
	} {
		v, err := api.Get(mod.member)
		if err != nil {
			return err
		}
		exports, err := moduleExports(rt, v)
		if err != nil {
			return err
		}
		if err := rt.SetModuleValues(mod.name, exports); err != nil {
			return err
		}
		if err := rt.SetModuleValues("node:"+mod.name, exports); err != nil {
			return err
		}
	}

	// Buffer is a global in node, and code that uses it rarely imports it.
	buffer, err := api.Get("buffer")
	if err != nil {
		return err
	}
	b, err := buffer.Get("Buffer")
	if err != nil {
		return err
	}
	return rt.Set("Buffer", b)
}

// moduleExports turns an object into the exports of a module: every own
// property by name, and the object itself as the default.
func moduleExports(rt *quickjs.Runtime, o quickjs.Value) (map[string]quickjs.Value, error) {
	out := map[string]quickjs.Value{"default": o}
	for _, k := range o.Keys() {
		v, err := o.Get(k)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// nodeJS is the four modules, in script.
const nodeJS = `(function (host) {
  "use strict";

  // --- events -------------------------------------------------------------

  class EventEmitter {
    constructor() {
      Object.defineProperty(this, "_events", {value: new Map(), writable: true});
      Object.defineProperty(this, "_max", {value: 10, writable: true});
    }
    on(name, fn) { return this.addListener(name, fn); }
    addListener(name, fn) {
      checkListener(fn);
      const list = this._events.get(name) || [];
      list.push({fn, once: false});
      this._events.set(name, list);
      this.emit("newListener", name, fn);
      return this;
    }
    prependListener(name, fn) {
      checkListener(fn);
      const list = this._events.get(name) || [];
      list.unshift({fn, once: false});
      this._events.set(name, list);
      return this;
    }
    once(name, fn) {
      checkListener(fn);
      const list = this._events.get(name) || [];
      list.push({fn, once: true});
      this._events.set(name, list);
      return this;
    }
    off(name, fn) { return this.removeListener(name, fn); }
    removeListener(name, fn) {
      const list = this._events.get(name);
      if (!list) return this;
      const at = list.findIndex(l => l.fn === fn);
      if (at >= 0) {
        list.splice(at, 1);
        this.emit("removeListener", name, fn);
      }
      if (list.length === 0) this._events.delete(name);
      return this;
    }
    removeAllListeners(name) {
      if (name === undefined) this._events.clear();
      else this._events.delete(name);
      return this;
    }
    emit(name, ...args) {
      const list = this._events.get(name);
      if (!list || list.length === 0) {
        // An error with nobody listening is thrown, which is what makes an
        // unheard error a failure rather than a silence.
        if (name === "error") {
          throw args[0] instanceof Error ? args[0] : new Error("Unhandled error: " + args[0]);
        }
        return false;
      }
      for (const l of list.slice()) {
        if (l.once) this.removeListener(name, l.fn);
        l.fn.apply(this, args);
      }
      return true;
    }
    listenerCount(name) { return (this._events.get(name) || []).length; }
    listeners(name) { return (this._events.get(name) || []).map(l => l.fn); }
    rawListeners(name) { return this.listeners(name); }
    eventNames() { return [...this._events.keys()]; }
    setMaxListeners(n) { this._max = n; return this; }
    getMaxListeners() { return this._max; }
  }

  function checkListener(fn) {
    if (typeof fn !== "function") {
      throw new TypeError("the listener must be a function");
    }
  }

  // once(emitter, name) waits for one event, which is how an emitter is used
  // from an async function.
  function once(emitter, name) {
    return new Promise((resolve, reject) => {
      emitter.once(name, (...args) => resolve(args));
      if (name !== "error" && typeof emitter.once === "function") {
        emitter.once("error", reject);
      }
    });
  }

  const events = {EventEmitter, once, default: EventEmitter};
  events.EventEmitter.EventEmitter = EventEmitter;

  // --- util ---------------------------------------------------------------

  function format(...args) {
    if (typeof args[0] !== "string") {
      return args.map(a => typeof a === "string" ? a : host.inspect(a)).join(" ");
    }
    let i = 1;
    const head = args[0].replace(/%([sdifjoO%])/g, (m, c) => {
      if (c === "%") return "%";
      if (i >= args.length) return m;
      const v = args[i++];
      switch (c) {
        case "s": return typeof v === "string" ? v : host.inspect(v);
        case "d": case "i": return String(Math.trunc(Number(v)));
        case "f": return String(Number(v));
        case "j": try { return JSON.stringify(v); } catch (e) { return "[Circular]"; }
        case "o": case "O": return host.inspect(v);
      }
      return m;
    });
    const rest = args.slice(i).map(a => typeof a === "string" ? a : host.inspect(a));
    return [head].concat(rest).join(" ");
  }

  // promisify turns a function whose last argument is a callback taking
  // (error, value) into one that returns a promise, which is what most of
  // node's older interfaces need to be usable with await.
  function promisify(fn) {
    if (typeof fn !== "function") throw new TypeError("a function is required");
    const out = function (...args) {
      return new Promise((resolve, reject) => {
        fn.call(this, ...args, (err, value) => {
          if (err) reject(err); else resolve(value);
        });
      });
    };
    Object.defineProperty(out, "name", {value: fn.name, configurable: true});
    return out;
  }

  function callbackify(fn) {
    if (typeof fn !== "function") throw new TypeError("a function is required");
    return function (...args) {
      const cb = args.pop();
      Promise.resolve(fn.apply(this, args)).then(
        v => cb(null, v),
        e => cb(e || new Error("rejected with a falsy value")));
    };
  }

  const types = {
    isDate: v => Object.prototype.toString.call(v) === "[object Date]",
    isRegExp: v => Object.prototype.toString.call(v) === "[object RegExp]",
    isMap: v => Object.prototype.toString.call(v) === "[object Map]",
    isSet: v => Object.prototype.toString.call(v) === "[object Set]",
    isPromise: v => Object.prototype.toString.call(v) === "[object Promise]",
    isTypedArray: v => ArrayBuffer.isView(v) && !(v instanceof DataView),
    isArrayBuffer: v => v instanceof ArrayBuffer,
    isNativeError: v => v instanceof Error,
  };

  const util = {
    format,
    inspect: (v) => host.inspect(v),
    promisify, callbackify, types,
    isDeepStrictEqual: (a, b) => deepEqual(a, b, true),
    deprecate: (fn) => fn,
    inherits(ctor, parent) {
      Object.setPrototypeOf(ctor.prototype, parent.prototype);
      Object.setPrototypeOf(ctor, parent);
    },
  };

  // --- assert -------------------------------------------------------------

  class AssertionError extends Error {
    constructor(options) {
      super(options.message || (host.inspect(options.actual) + " " +
            options.operator + " " + host.inspect(options.expected)));
      this.name = "AssertionError";
      this.actual = options.actual;
      this.expected = options.expected;
      this.operator = options.operator;
      this.generatedMessage = !options.message;
    }
  }

  function fail(message) {
    throw new AssertionError({
      message: message === undefined ? "Failed" : message,
      operator: "fail",
    });
  }

  function assert(value, message) {
    if (!value) {
      throw new AssertionError({
        message, actual: value, expected: true, operator: "==",
      });
    }
  }

  function deepEqual(a, b, strict, seen = new Map()) {
    if (strict ? Object.is(a, b) : a == b) return true;
    if (typeof a !== "object" || typeof b !== "object" || a === null || b === null) {
      // Two NaNs are the same value for the purposes of comparing structures,
      // which is what Object.is says and what == does not.
      return strict ? Object.is(a, b) : a == b;
    }
    if (seen.get(a) === b) return true;
    seen.set(a, b);

    const ta = Object.prototype.toString.call(a);
    const tb = Object.prototype.toString.call(b);
    if (ta !== tb) return false;
    if (strict && Object.getPrototypeOf(a) !== Object.getPrototypeOf(b)) return false;

    switch (ta) {
      case "[object Date]": return a.getTime() === b.getTime();
      case "[object RegExp]": return a.source === b.source && a.flags === b.flags;
      case "[object Error]": return a.name === b.name && a.message === b.message;
      case "[object Map]": {
        if (a.size !== b.size) return false;
        for (const [k, v] of a) {
          if (!b.has(k) || !deepEqual(v, b.get(k), strict, seen)) return false;
        }
        return true;
      }
      case "[object Set]": {
        if (a.size !== b.size) return false;
        for (const v of a) if (!b.has(v)) return false;
        return true;
      }
    }
    if (ArrayBuffer.isView(a) || Array.isArray(a)) {
      if (a.length !== b.length) return false;
      for (let i = 0; i < a.length; i++) {
        if (!deepEqual(a[i], b[i], strict, seen)) return false;
      }
      if (ArrayBuffer.isView(a)) return true;
    }
    const ka = Object.keys(a), kb = Object.keys(b);
    if (ka.length !== kb.length) return false;
    for (const k of ka) {
      if (!Object.prototype.hasOwnProperty.call(b, k)) return false;
      if (!deepEqual(a[k], b[k], strict, seen)) return false;
    }
    return true;
  }

  Object.assign(assert, {
    AssertionError,
    ok: assert,
    fail,
    equal(actual, expected, message) {
      if (actual != expected) {
        throw new AssertionError({message, actual, expected, operator: "=="});
      }
    },
    notEqual(actual, expected, message) {
      if (actual == expected) {
        throw new AssertionError({message, actual, expected, operator: "!="});
      }
    },
    strictEqual(actual, expected, message) {
      if (!Object.is(actual, expected)) {
        throw new AssertionError({message, actual, expected, operator: "strictEqual"});
      }
    },
    notStrictEqual(actual, expected, message) {
      if (Object.is(actual, expected)) {
        throw new AssertionError({message, actual, expected, operator: "notStrictEqual"});
      }
    },
    deepEqual(actual, expected, message) {
      if (!deepEqual(actual, expected, false)) {
        throw new AssertionError({message, actual, expected, operator: "deepEqual"});
      }
    },
    deepStrictEqual(actual, expected, message) {
      if (!deepEqual(actual, expected, true)) {
        throw new AssertionError({message, actual, expected, operator: "deepStrictEqual"});
      }
    },
    notDeepStrictEqual(actual, expected, message) {
      if (deepEqual(actual, expected, true)) {
        throw new AssertionError({message, actual, expected, operator: "notDeepStrictEqual"});
      }
    },
    match(value, pattern, message) {
      if (!pattern.test(value)) {
        throw new AssertionError({message, actual: value, expected: pattern, operator: "match"});
      }
    },
    doesNotMatch(value, pattern, message) {
      if (pattern.test(value)) {
        throw new AssertionError({
          message, actual: value, expected: pattern, operator: "doesNotMatch"});
      }
    },
    throws(fn, expected, message) {
      try { fn(); } catch (e) {
        if (expected && !matchesError(e, expected)) throw e;
        return;
      }
      throw new AssertionError({message: message || "Missing expected exception", operator: "throws"});
    },
    doesNotThrow(fn, message) {
      try { fn(); } catch (e) {
        throw new AssertionError({
          message: message || ("Got unwanted exception: " + e.message), operator: "doesNotThrow"});
      }
    },
    async rejects(p, expected, message) {
      try { await (typeof p === "function" ? p() : p); } catch (e) {
        if (expected && !matchesError(e, expected)) throw e;
        return;
      }
      throw new AssertionError({
        message: message || "Missing expected rejection", operator: "rejects"});
    },
    async doesNotReject(p, message) {
      try { await (typeof p === "function" ? p() : p); } catch (e) {
        throw new AssertionError({
          message: message || ("Got unwanted rejection: " + e.message), operator: "doesNotReject"});
      }
    },
  });
  assert.strict = assert;

  function matchesError(e, expected) {
    if (typeof expected === "function") {
      return expected.prototype !== undefined ? e instanceof expected : expected(e);
    }
    if (expected instanceof RegExp) return expected.test(String(e && e.message));
    if (typeof expected === "object" && expected !== null) {
      return Object.keys(expected).every(k => deepEqual(e[k], expected[k], true));
    }
    return true;
  }

  // --- buffer -------------------------------------------------------------

  // A Buffer is a Uint8Array with node's methods on it. Making it a subclass
  // rather than something of its own is what lets it be handed to anything
  // that takes bytes.
  class Buffer extends Uint8Array {
    static alloc(size, fill = 0) {
      const b = new Buffer(size);
      if (fill !== 0) b.fill(typeof fill === "string" ? fill.charCodeAt(0) : fill);
      return b;
    }
    static allocUnsafe(size) { return new Buffer(size); }
    static from(value, encodingOrOffset, length) {
      if (typeof value === "string") return fromString(value, encodingOrOffset || "utf8");
      if (value instanceof ArrayBuffer) {
        const view = new Uint8Array(value, encodingOrOffset || 0,
          length === undefined ? undefined : length);
        const out = new Buffer(view.length);
        out.set(view);
        return out;
      }
      if (ArrayBuffer.isView(value) || Array.isArray(value)) {
        const out = new Buffer(value.length);
        out.set(value);
        return out;
      }
      throw new TypeError("Buffer.from needs a string, an array or a buffer");
    }
    static concat(list, total) {
      let size = total;
      if (size === undefined) {
        size = 0;
        for (const b of list) size += b.length;
      }
      const out = new Buffer(size);
      let at = 0;
      for (const b of list) {
        if (at >= size) break;
        const piece = b.length > size - at ? b.subarray(0, size - at) : b;
        out.set(piece, at);
        at += piece.length;
      }
      return out;
    }
    static isBuffer(v) { return v instanceof Buffer; }
    static byteLength(value, encoding = "utf8") {
      if (typeof value !== "string") return value.length;
      return fromString(value, encoding).length;
    }
    toString(encoding = "utf8", start = 0, end = this.length) {
      const view = this.subarray(start, end);
      switch (String(encoding).toLowerCase()) {
        case "utf8": case "utf-8": return new TextDecoder().decode(view);
        case "hex": {
          let out = "";
          for (const b of view) out += b.toString(16).padStart(2, "0");
          return out;
        }
        case "base64": {
          let s = "";
          for (const b of view) s += String.fromCharCode(b);
          return btoa(s);
        }
        case "latin1": case "binary": case "ascii": {
          let out = "";
          for (const b of view) out += String.fromCharCode(encoding === "ascii" ? b & 0x7f : b);
          return out;
        }
      }
      throw new TypeError("unsupported encoding: " + encoding);
    }
    toJSON() { return {type: "Buffer", data: Array.from(this)}; }
    equals(other) {
      if (this.length !== other.length) return false;
      for (let i = 0; i < this.length; i++) if (this[i] !== other[i]) return false;
      return true;
    }
    write(text, offset = 0, encoding = "utf8") {
      const bytes = fromString(text, encoding);
      const n = Math.min(bytes.length, this.length - offset);
      this.set(bytes.subarray(0, n), offset);
      return n;
    }
  }

  function fromString(text, encoding) {
    switch (String(encoding).toLowerCase()) {
      case "utf8": case "utf-8": {
        const bytes = new TextEncoder().encode(text);
        const out = new Buffer(bytes.length);
        out.set(bytes);
        return out;
      }
      case "hex": {
        const clean = text.length % 2 === 0 ? text : text.slice(0, -1);
        const out = new Buffer(clean.length / 2);
        for (let i = 0; i < out.length; i++) {
          out[i] = parseInt(clean.substr(i * 2, 2), 16);
        }
        return out;
      }
      case "base64": {
        const raw = atob(text);
        const out = new Buffer(raw.length);
        for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i) & 0xff;
        return out;
      }
      case "latin1": case "binary": case "ascii": {
        const out = new Buffer(text.length);
        for (let i = 0; i < text.length; i++) out[i] = text.charCodeAt(i) & 0xff;
        return out;
      }
    }
    throw new TypeError("unsupported encoding: " + encoding);
  }

  const buffer = {Buffer, atob, btoa, default: {Buffer}};

  return {events, util, assert, buffer};
})`
