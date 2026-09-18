package stdlib

import (
	"fmt"
	"io"
	"strings"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Console installs the console object, writing to the given streams.
//
// log, info, debug and dir go to out; warn, error and trace go to errOut. A nil
// stream discards what is written to it, which is how a host silences one half
// without losing the other.
//
// What a value looks like is decided in script rather than here: the formatting
// has to reach into objects, follow prototypes and notice cycles, and script is
// where that is expressed exactly.
func Console(rt *quickjs.Runtime, out, errOut io.Writer) error {
	write := func(w io.Writer) func(string) {
		if w == nil {
			return func(string) {}
		}
		return func(s string) { fmt.Fprintln(w, s) }
	}
	host := rt.NewObject()
	if err := host.Set("out", write(out)); err != nil {
		return err
	}
	if err := host.Set("err", write(errOut)); err != nil {
		return err
	}
	if err := host.Set("now", func() float64 {
		return float64(time.Now().UnixNano()) / 1e6
	}); err != nil {
		return err
	}
	console, err := evalWithHost(rt, "<console>", consoleJS, host)
	if err != nil {
		return err
	}
	return rt.Set("console", console)
}

// evalWithHost evaluates source that is a function of one argument and calls it
// with the host object.
//
// The host object is never a global: what the script gets is whatever the
// function returns, and the hooks it was built out of are reachable only
// through the closure. A script cannot get at the raw ones.
func evalWithHost(rt *quickjs.Runtime, name, src string, host quickjs.Value) (quickjs.Value, error) {
	fn, err := rt.EvalFile(name, src)
	if err != nil {
		return quickjs.Value{}, err
	}
	return fn.Call(host)
}

// consoleJS is the console, in script.
//
// The formatting follows what a developer expects from a console rather than
// anything specified: quoted strings inside a structure and bare at the top,
// a class name in front of an object that has one, the contents of a Map or a
// Set, and a marker where a structure refers to itself.
const consoleJS = `(function (host) {
  "use strict";

  const hasOwn = Object.prototype.hasOwnProperty;
  const toString = Object.prototype.toString;

  function quote(s) {
    let out = "'";
    for (const ch of s) {
      switch (ch) {
        case "'": out += "\\'"; break;
        case "\\": out += "\\\\"; break;
        case "\n": out += "\\n"; break;
        case "\r": out += "\\r"; break;
        case "\t": out += "\\t"; break;
        default: out += ch;
      }
    }
    return out + "'";
  }

  function typeTag(v) {
    return toString.call(v).slice(8, -1);
  }

  function className(v) {
    const proto = Object.getPrototypeOf(v);
    if (proto === null) return "[Object: null prototype] ";
    if (proto === Object.prototype) return "";
    const ctor = proto.constructor;
    if (typeof ctor === "function" && ctor.name && ctor.name !== "Object") {
      return ctor.name + " ";
    }
    return "";
  }

  function inspect(v, depth, seen) {
    switch (typeof v) {
      case "undefined": return "undefined";
      case "boolean": case "number": return String(v);
      case "bigint": return String(v) + "n";
      case "symbol": return v.toString();
      case "string": return depth === 0 ? v : quote(v);
      case "function": {
        const kind = v.prototype && v.prototype.constructor === v &&
          /^class[\s{]/.test(Function.prototype.toString.call(v)) ? "class" : "function";
        return v.name ? "[" + kind + ": " + v.name + "]" : "[" + kind + " (anonymous)]";
      }
    }
    if (v === null) return "null";

    if (seen.has(v)) return "[Circular]";
    if (depth > 4) return typeTag(v) === "Array" ? "[Array]" : "[Object]";
    seen.add(v);
    try {
      return inspectObject(v, depth, seen);
    } finally {
      seen.delete(v);
    }
  }

  function inspectObject(v, depth, seen) {
    const tag = typeTag(v);
    switch (tag) {
      case "Array": {
        const parts = [];
        let empty = 0;
        for (let i = 0; i < v.length; i++) {
          if (!hasOwn.call(v, i)) { empty++; continue; }
          if (empty > 0) { parts.push("<" + empty + " empty>"); empty = 0; }
          parts.push(inspect(v[i], depth + 1, seen));
        }
        if (empty > 0) parts.push("<" + empty + " empty>");
        for (const k of Object.keys(v)) {
          if (String(Number(k)) === k) continue;
          parts.push(key(k) + ": " + inspect(v[k], depth + 1, seen));
        }
        return parts.length === 0 ? "[]" : "[ " + parts.join(", ") + " ]";
      }
      case "Error": {
        const stack = v.stack;
        return typeof stack === "string" && stack !== "" ? stack : String(v);
      }
      case "Date": return isNaN(v.getTime()) ? "Invalid Date" : v.toISOString();
      case "RegExp": return String(v);
      case "String": return "[String: " + quote(String(v)) + "]";
      case "Number": return "[Number: " + String(v) + "]";
      case "Boolean": return "[Boolean: " + String(v) + "]";
      case "Symbol": return "[Symbol: " + String(v) + "]";
      case "Map": {
        if (v.size === 0) return "Map(0) {}";
        const parts = [];
        for (const [k, val] of v) {
          parts.push(inspect(k, depth + 1, seen) + " => " + inspect(val, depth + 1, seen));
        }
        return "Map(" + v.size + ") { " + parts.join(", ") + " }";
      }
      case "Set": {
        if (v.size === 0) return "Set(0) {}";
        const parts = [];
        for (const val of v) parts.push(inspect(val, depth + 1, seen));
        return "Set(" + v.size + ") { " + parts.join(", ") + " }";
      }
      case "WeakMap": return "WeakMap { <items unknown> }";
      case "WeakSet": return "WeakSet { <items unknown> }";
      case "Promise": return "Promise { <state unknown> }";
    }
    if (ArrayBuffer.isView(v) && !(v instanceof DataView)) {
      const parts = [];
      for (let i = 0; i < v.length; i++) parts.push(String(v[i]));
      return v.constructor.name + "(" + v.length + ") [ " + parts.join(", ") + " ]";
    }
    if (v instanceof ArrayBuffer) return "ArrayBuffer { byteLength: " + v.byteLength + " }";

    // An ordinary object: its own enumerable properties, and the symbol-keyed
    // ones after them, which is the order Object.keys reports.
    const parts = [];
    for (const k of Object.keys(v)) {
      parts.push(key(k) + ": " + inspect(v[k], depth + 1, seen));
    }
    for (const s of Object.getOwnPropertySymbols(v)) {
      const d = Object.getOwnPropertyDescriptor(v, s);
      if (!d.enumerable) continue;
      parts.push("[" + String(s) + "]: " + inspect(v[s], depth + 1, seen));
    }
    const prefix = className(v);
    if (parts.length === 0) return prefix + "{}";
    return prefix + "{ " + parts.join(", ") + " }";
  }

  // A key that is a plain identifier is printed bare, and anything else is
  // quoted, which is what makes the output readable as source.
  function key(k) {
    return /^[A-Za-z_$][A-Za-z0-9_$]*$/.test(k) ? k : quote(k);
  }

  function format(args) {
    // A first argument with % directives in it consumes the ones after it, as
    // it does in a browser.
    if (typeof args[0] === "string" && /%[sdifjoOc%]/.test(args[0])) {
      let i = 1;
      const head = args[0].replace(/%([sdifjoOc%])/g, (m, c) => {
        if (c === "%") return "%";
        if (i >= args.length) return m;
        const v = args[i++];
        switch (c) {
          case "s": return typeof v === "string" ? v : inspect(v, 1, new Set());
          case "d": case "i":
            return typeof v === "bigint" ? String(v) + "n" : String(Math.trunc(Number(v)));
          case "f": return String(Number(v));
          case "j": try { return JSON.stringify(v); } catch (e) { return "[Circular]"; }
          case "o": case "O": return inspect(v, 1, new Set());
          case "c": return "";
        }
        return m;
      });
      const rest = args.slice(i).map(a => inspect(a, 0, new Set()));
      return [head].concat(rest).join(" ");
    }
    return args.map(a => inspect(a, 0, new Set())).join(" ");
  }

  let groupDepth = 0;
  function emit(write, args) {
    const text = format(args);
    const pad = "  ".repeat(groupDepth);
    write(pad === "" ? text : text.split("\n").map(l => pad + l).join("\n"));
  }

  const counts = new Map();
  const timers = new Map();

  const console = {
    log: (...args) => emit(host.out, args),
    info: (...args) => emit(host.out, args),
    debug: (...args) => emit(host.out, args),
    dir: (v, opts) => emit(host.out, [v]),
    warn: (...args) => emit(host.err, args),
    error: (...args) => emit(host.err, args),
    trace: (...args) => {
      const where = new Error().stack || "";
      emit(host.err, ["Trace:" + (args.length ? " " + format(args) : "")]);
      const lines = where.split("\n").slice(2);
      if (lines.length) host.err(lines.join("\n"));
    },
    assert: (ok, ...args) => {
      if (ok) return;
      emit(host.err, ["Assertion failed" + (args.length ? ": " + format(args) : "")]);
    },
    group: (...args) => {
      if (args.length) emit(host.out, args);
      groupDepth++;
    },
    groupCollapsed: (...args) => {
      if (args.length) emit(host.out, args);
      groupDepth++;
    },
    groupEnd: () => { if (groupDepth > 0) groupDepth--; },
    count: (label = "default") => {
      const n = (counts.get(label) || 0) + 1;
      counts.set(label, n);
      emit(host.out, [label + ": " + n]);
    },
    countReset: (label = "default") => { counts.delete(label); },
    time: (label = "default") => { timers.set(label, host.now()); },
    timeLog: (label = "default", ...args) => {
      const started = timers.get(label);
      if (started === undefined) {
        emit(host.err, ["Timer '" + label + "' does not exist"]);
        return;
      }
      emit(host.out, [label + ": " + (host.now() - started).toFixed(3) + "ms"].concat(args));
    },
    timeEnd: (label = "default") => {
      const started = timers.get(label);
      if (started === undefined) {
        emit(host.err, ["Timer '" + label + "' does not exist"]);
        return;
      }
      timers.delete(label);
      emit(host.out, [label + ": " + (host.now() - started).toFixed(3) + "ms"]);
    },
    table: (data) => emit(host.out, [data]),
  };
  // The formatter is what a host needs to print a value the way the console
  // does -- a REPL result, an uncaught error -- so it travels with it.
  console[Symbol.for("quickjs.inspect")] = (v) => inspect(v, 1, new Set());
  return console;
})`

// Inspect formats a value the way console.log does.
//
// A host that prints values itself -- a REPL showing a result, a runner
// reporting an uncaught error -- wants them to look the same as what the script
// printed, which means asking the console rather than reimplementing it.
// Console must have been installed.
func Inspect(rt *quickjs.Runtime, v quickjs.Value) string {
	console, err := rt.Get("console")
	if err != nil || !console.IsObject() {
		return v.String()
	}
	fn, err := rt.Eval(`console[Symbol.for("quickjs.inspect")]`)
	if err != nil || !fn.IsFunction() {
		return v.String()
	}
	out, err := fn.Call(v)
	if err != nil {
		return v.String()
	}
	return strings.TrimSuffix(out.String(), "\n")
}
