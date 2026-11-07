// JSON.parse and JSON.stringify against the specification's algorithms,
// written out in JavaScript with no use of JSON, over generated documents and
// object graphs. The same seed makes the same cases, so that a graph is built
// once for the reference and three times for the built-in: what
// JSON.stringify keeps of a layout it uses from the second time it meets it.
// jsonDifferential(seed, count) returns the failures, as text. Node runs this
// file too (see json_differential_test.go), which is what shows the reference
// itself right.
var jsonDifferential = (function () {
  "use strict";
  var BS = String.fromCharCode(92), QUOTE = String.fromCharCode(34);
  var HIGH = String.fromCharCode(0xD83D), LOW = String.fromCharCode(0xDE00);

  // ---- The reference.

  function refSyntaxError() { throw new SyntaxError("reference"); }

  function refParse(text) {
    var i = 0, n = text.length;
    function ws() {
      while (i < n) {
        var c = text.charCodeAt(i);
        if (c !== 32 && c !== 9 && c !== 10 && c !== 13) break;
        i++;
      }
    }
    function digits() {
      var from = i;
      while (i < n && text.charCodeAt(i) >= 48 && text.charCodeAt(i) <= 57) i++;
      return i > from;
    }
    function number() {
      var from = i;
      if (text[i] === "-") i++;
      if (text[i] === "0") {
        i++;
      } else if (!digits()) {
        refSyntaxError();
      }
      if (text[i] === ".") {
        i++;
        if (!digits()) refSyntaxError();
      }
      if (text[i] === "e" || text[i] === "E") {
        i++;
        if (text[i] === "+" || text[i] === "-") i++;
        if (!digits()) refSyntaxError();
      }
      return Number(text.slice(from, i));
    }
    function string() {
      var out = "";
      i++;
      for (;;) {
        if (i >= n) refSyntaxError();
        var c = text.charCodeAt(i);
        if (c === 34) { i++; return out; }
        if (c < 32) refSyntaxError();
        if (c !== 92) { out += text[i++]; continue; }
        i++;
        var e = text[i++];
        switch (e) {
          case QUOTE: case "/": case BS: out += e; break;
          case "b": out += String.fromCharCode(8); break;
          case "f": out += String.fromCharCode(12); break;
          case "n": out += String.fromCharCode(10); break;
          case "r": out += String.fromCharCode(13); break;
          case "t": out += String.fromCharCode(9); break;
          case "u":
            var h = text.slice(i, i + 4);
            if (h.length !== 4) refSyntaxError();
            for (var k = 0; k < 4; k++) {
              var d = h.charCodeAt(k);
              if (!(d >= 48 && d <= 57 || d >= 65 && d <= 70 || d >= 97 && d <= 102)) refSyntaxError();
            }
            out += String.fromCharCode(parseInt(h, 16));
            i += 4;
            break;
          default: refSyntaxError();
        }
      }
    }
    function value() {
      ws();
      if (i >= n) refSyntaxError();
      var c = text[i];
      if (c === "{") {
        var o = {};
        i++;
        ws();
        if (text[i] === "}") { i++; return o; }
        for (;;) {
          ws();
          if (text[i] !== QUOTE) refSyntaxError();
          var key = string();
          ws();
          if (text[i] !== ":") refSyntaxError();
          i++;
          Object.defineProperty(o, key, { value: value(), writable: true, enumerable: true, configurable: true });
          ws();
          if (text[i] === ",") { i++; continue; }
          if (text[i] === "}") { i++; return o; }
          refSyntaxError();
        }
      }
      if (c === "[") {
        var a = [];
        i++;
        ws();
        if (text[i] === "]") { i++; return a; }
        for (;;) {
          a.push(value());
          ws();
          if (text[i] === ",") { i++; continue; }
          if (text[i] === "]") { i++; return a; }
          refSyntaxError();
        }
      }
      if (c === QUOTE) return string();
      if (text.startsWith("true", i)) { i += 4; return true; }
      if (text.startsWith("false", i)) { i += 5; return false; }
      if (text.startsWith("null", i)) { i += 4; return null; }
      if (c === "-" || c >= "0" && c <= "9") return number();
      refSyntaxError();
    }
    var v = value();
    ws();
    if (i !== n) refSyntaxError();
    return v;
  }

  function refRevive(holder, key, reviver) {
    var val = holder[key];
    if (typeof val === "object" && val !== null) {
      if (Array.isArray(val)) {
        var len = Math.min(Math.max(Math.trunc(Number(val.length)) || 0, 0), 2 ** 53 - 1);
        for (var i = 0; i < len; i++) {
          var e = refRevive(val, String(i), reviver);
          if (e === undefined) Reflect.deleteProperty(val, String(i));
          else Reflect.defineProperty(val, String(i), { value: e, writable: true, enumerable: true, configurable: true });
        }
      } else {
        var keys = Object.keys(val);
        for (var j = 0; j < keys.length; j++) {
          var f = refRevive(val, keys[j], reviver);
          if (f === undefined) Reflect.deleteProperty(val, keys[j]);
          else Reflect.defineProperty(val, keys[j], { value: f, writable: true, enumerable: true, configurable: true });
        }
      }
    }
    return reviver.call(holder, key, val);
  }

  function refJSONParse(text, reviver) {
    var v = refParse(String(text));
    if (typeof reviver !== "function") return v;
    var root = {};
    root[""] = v;
    return refRevive(root, "", reviver);
  }

  function hex4(c) { return ("000" + c.toString(16)).slice(-4); }

  function refQuote(s) {
    var out = QUOTE;
    for (var i = 0; i < s.length; i++) {
      var c = s.charCodeAt(i);
      if (c === 8) out += BS + "b";
      else if (c === 9) out += BS + "t";
      else if (c === 10) out += BS + "n";
      else if (c === 12) out += BS + "f";
      else if (c === 13) out += BS + "r";
      else if (c === 34) out += BS + QUOTE;
      else if (c === 92) out += BS + BS;
      else if (c < 32) out += BS + "u" + hex4(c);
      else if (c >= 0xD800 && c <= 0xDBFF) {
        var d = s.charCodeAt(i + 1);
        if (d >= 0xDC00 && d <= 0xDFFF) { out += s[i] + s[i + 1]; i++; }
        else out += BS + "u" + hex4(c);
      } else if (c >= 0xDC00 && c <= 0xDFFF) out += BS + "u" + hex4(c);
      else out += s[i];
    }
    return out + QUOTE;
  }

  function refStringify(value, replacer, space) {
    var fn, list, stack = [], indent = "", gap = "";
    if (typeof replacer === "function") {
      fn = replacer;
    } else if (Array.isArray(replacer)) {
      list = [];
      var n = replacer.length;
      for (var i = 0; i < n; i++) {
        var v = replacer[i], item;
        if (typeof v === "string") item = v;
        else if (typeof v === "number") item = String(v);
        else if (v instanceof String || v instanceof Number) item = String(v);
        if (item !== undefined && list.indexOf(item) < 0) list.push(item);
        item = undefined;
      }
    }
    if (space instanceof Number) space = Number(space);
    else if (space instanceof String) space = String(space);
    if (typeof space === "number") gap = " ".repeat(Math.max(Math.min(10, Math.trunc(space)), 0) || 0);
    else if (typeof space === "string") gap = space.slice(0, 10);
    var open = String.fromCharCode(10);

    function property(holder, key) {
      var v = holder[key];
      if (typeof v === "object" && v !== null || typeof v === "bigint" || typeof v === "function") {
        var tj = v.toJSON;
        if (typeof tj === "function") v = tj.call(v, key);
      }
      if (fn) v = fn.call(holder, key, v);
      if (typeof v === "object" && v !== null) {
        if (v instanceof Number) v = Number(v);
        else if (v instanceof String) v = String(v);
        else if (v instanceof Boolean) v = Boolean.prototype.valueOf.call(v);
        else if (v instanceof BigInt) v = BigInt.prototype.valueOf.call(v);
      }
      if (v === null) return "null";
      if (v === true) return "true";
      if (v === false) return "false";
      if (typeof v === "string") return refQuote(v);
      if (typeof v === "number") return isFinite(v) ? String(v) : "null";
      if (typeof v === "bigint") throw new TypeError("reference");
      if (typeof v === "object") return Array.isArray(v) ? array(v) : object(v);
      return undefined;
    }
    function enter(v) {
      if (stack.indexOf(v) >= 0) throw new TypeError("reference");
      stack.push(v);
    }
    function object(v) {
      enter(v);
      var stepback = indent;
      indent += gap;
      var keys = list || Object.keys(v), parts = [];
      for (var i = 0; i < keys.length; i++) {
        var s = property(v, keys[i]);
        if (s !== undefined) parts.push(refQuote(keys[i]) + (gap ? ": " : ":") + s);
      }
      var out = "{}";
      if (parts.length) {
        out = gap ? "{" + open + indent + parts.join("," + open + indent) + open + stepback + "}" : "{" + parts.join(",") + "}";
      }
      stack.pop();
      indent = stepback;
      return out;
    }
    function array(v) {
      enter(v);
      var stepback = indent;
      indent += gap;
      var n = v.length, parts = [];
      for (var i = 0; i < n; i++) {
        var s = property(v, String(i));
        parts.push(s === undefined ? "null" : s);
      }
      var out = "[]";
      if (parts.length) {
        out = gap ? "[" + open + indent + parts.join("," + open + indent) + open + stepback + "]" : "[" + parts.join(",") + "]";
      }
      stack.pop();
      indent = stepback;
      return out;
    }
    var wrapper = {};
    wrapper[""] = value;
    return property(wrapper, "");
  }

  // ---- What is generated.

  function Rand(seed) { this.s = seed >>> 0 || 1; }
  Rand.prototype.next = function () {
    // xorshift32.
    var x = this.s;
    x ^= x << 13; x >>>= 0;
    x ^= x >>> 17;
    x ^= x << 5; x >>>= 0;
    this.s = x;
    return x;
  };
  Rand.prototype.n = function (k) { return this.next() % k; };
  Rand.prototype.pick = function (a) { return a[this.n(a.length)]; };
  Rand.prototype.chance = function (k) { return this.n(k) === 0; };

  // Few keys, so that layouts repeat and hints are met.
  var plainKeys = ["id", "name", "a", "b", "c", "value", "items", "é", "😀", "k" + HIGH, "", "__proto__", "toJSON", "0", "1", "7", "10", "4294967295", "4294967294", "01", "-1", "1.5", "a b", "q" + QUOTE, "x" + String.fromCharCode(10) + "y", "length", "constructor"];
  function rawKey(r) {
    var k = r.pick(plainKeys);
    return r.chance(16) ? k + r.n(80) : k;
  }
  function escapeText(r, s, sometimes) {
    var out = "";
    for (var i = 0; i < s.length; i++) {
      var c = s.charCodeAt(i);
      if (c === 34 || c === 92) out += BS + s[i];
      else if (c < 32) out += BS + "u" + hex4(c);
      else if (sometimes && r.chance(sometimes)) out += BS + "u" + (r.chance(2) ? hex4(c) : hex4(c).toUpperCase());
      else out += s[i];
    }
    return QUOTE + out + QUOTE;
  }
  var pieces = ["a", "text", "é", "中", "😀", HIGH, LOW, String.fromCharCode(0xD7FF), String.fromCharCode(0xE000), String.fromCharCode(10), String.fromCharCode(9), QUOTE, BS, String.fromCharCode(1), String.fromCharCode(0x7F), String.fromCharCode(0x80), "/", String.fromCharCode(0x2028), "x".repeat(7), "y".repeat(31), "z".repeat(64)];
  function text(r) {
    var n = r.n(6), s = "";
    for (var i = 0; i < n; i++) s += r.pick(pieces);
    return r.chance(40) ? s + "w".repeat(r.n(300)) : s;
  }
  var numberTexts = ["0", "-0", "1", "-1", "0.5", "12.25", "1e3", "1E-3", "1e21", "1e-7", "0.000001", "1.7976931348623157e308", "5e-324", "1e400", "-1e400", "1e-400", "9007199254740993", "123456789012345678901234567890", "0.1", "0.30000000000000004", "2.5e-5", "1.0", "100", "1234.5678", "999999999.999999", "4.35", "1e22", "1e23", "0e5", "-0.0"];
  function numberText(r) {
    switch (r.n(4)) {
      case 0: return r.pick(numberTexts);
      case 1: return String(r.n(100000));
      case 2: return r.n(100000) + "." + r.n(1000000);
      default: return (r.chance(2) ? "-" : "") + r.n(1000) + "." + r.next() + "e" + (r.n(700) - 350);
    }
  }
  var spaces = [" ", "  ", String.fromCharCode(10), String.fromCharCode(9), String.fromCharCode(13, 10, 32), String.fromCharCode(32, 9)];
  function space(r) { return r.chance(4) ? r.pick(spaces) : ""; }
  function makeDocument(r, depth) {
    var sp = space(r), parts, n, i;
    switch (depth > 5 ? r.n(5) : r.n(8)) {
      case 0: return r.pick(["null", "true", "false"]);
      case 1: case 2: return numberText(r);
      case 3: case 4: return escapeText(r, text(r), r.chance(3) ? 3 : 0);
      case 5:
        n = r.n(5);
        parts = [];
        for (i = 0; i < n; i++) parts.push(sp + makeDocument(r, depth + 1) + sp);
        return "[" + (n ? parts.join(",") : sp) + "]";
      default:
        n = r.n(7);
        parts = [];
        for (i = 0; i < n; i++) {
          var key = rawKey(r);
          // A key given twice, and one of those before it.
          if (i > 0 && r.chance(8)) key = parts[r.n(i)].key;
          parts.push({ key: key, text: sp + escapeText(r, key, r.chance(6) ? 2 : 0) + sp + ":" + sp + makeDocument(r, depth + 1) + sp });
        }
        return "{" + (n ? parts.map(function (p) { return p.text; }).join(",") : sp) + "}";
    }
  }
  // Text that may no longer be JSON.
  var inserted = [QUOTE, ",", ":", "}", "]", "{", "[", BS, String.fromCharCode(10), String.fromCharCode(1), "x", "0", "-", ".", "e", "é"];
  function damaged(r, s) {
    var at = r.n(s.length + 1);
    switch (r.n(4)) {
      case 0: return s.slice(0, at);
      case 1: return s.slice(0, at) + r.pick(inserted) + s.slice(at);
      case 2: return s.slice(0, at) + s.slice(at + 1);
      default: return s + r.pick(["", " ", "x", ",", "]", "}", "0"]);
    }
  }

  // A prototype the graphs share, as a program's objects do, which a change
  // under the walk adds a key to.
  var sharedProto = { inherited: 1 };

  // Classes the graphs share, so that their layouts are met again.
  function Point(x, y) { this.x = x; this.y = y; }
  Point.prototype.norm = function () { return this.x; };
  function Tagged(a) { this.a = a; this.b = a; }
  Tagged.prototype.toJSON = function (k) { return { tagged: this.a, key: k }; };
  class Base { constructor() { this.m = 1; } }
  class Derived extends Base { #hidden = 1; constructor() { super(); this.n = 2; } get acc() { return this.#hidden; } }

  // An object graph, the same from the same seed: what walks it -- getters,
  // toJSON methods -- is told by the seed what to do to the object it is on,
  // so that the built-in and the reference are given the same changes at the
  // same moments.
  function makeGraph(seed) {
    var r = new Rand(seed), budget = 40;
    var shared = { s: 1 };
    delete sharedProto.lateKey;
    delete sharedProto.toJSON;
    delete Point.prototype.toJSON;
    var mutations = [
      function (o) { delete o.b; },
      function (o) { o.b = 99; },
      function (o) { o.z = 1; delete o.a; },
      function (o) { Object.defineProperty(o, "b", { get: function () { return "from getter"; }, enumerable: true, configurable: true }); },
      function (o) { Object.defineProperty(o, "b", { enumerable: false }); },
      function (o) { Object.setPrototypeOf(o, { b: "inherited", c: "inherited" }); delete o.b; },
      function (o) { Object.freeze(o); },
      function (o) { for (var k in o) if (k !== "a") { delete o[k]; o[k] = "again"; } },
      function (o) { if (Array.isArray(o)) o.length = 1; else o.toJSON = function () { return "own toJSON, late"; }; },
      function (o) { if (Array.isArray(o)) o.push("pushed"); else Object.getPrototypeOf(o).lateKey = 1; },
      function (o) { if (Array.isArray(o)) delete o[1]; else delete Object.getPrototypeOf(o).lateKey; },
      function (o) { sharedProto.toJSON = function () { return "toJSON of a prototype, late"; }; },
      function (o) { delete sharedProto.toJSON; },
      function (o) { Point.prototype.toJSON = function () { return "toJSON of a class, late"; }; },
    ];
    function mutator() {
      var m = r.pick(mutations);
      return function (o) { try { m(o); } catch (e) { return e.name; } };
    }
    function leaf() {
      switch (r.n(12)) {
        case 0: return r.n(1000);
        case 1: return r.n(100000) / 1000;
        case 2: return text(r);
        case 3: return null;
        case 4: return r.chance(2);
        case 5: return undefined;
        case 6: return function () { };
        case 7: return Symbol("s");
        case 8: return r.pick([NaN, Infinity, -0, 1e21, 1e-7, 2 ** 53]);
        case 9: return r.pick([new Number(1.5), new String("boxed é"), new Boolean(false), Object(1n)]);
        case 10: return r.chance(8) ? 1n : "plain";
        default: return new Date(r.n(1000000) * 1000);
      }
    }
    function value(depth) {
      if (depth > 4 || budget-- <= 0) return leaf();
      switch (r.n(14)) {
        case 0: case 1: return leaf();
        case 2: case 3: return record(depth);
        case 4: return new Point(value(depth + 1), value(depth + 1));
        case 5: return new Tagged(value(depth + 1));
        case 6: return new Derived();
        case 7: return array(depth);
        case 8: return mutating(depth);
        case 9: return r.chance(2) ? shared : inheriting(depth);
        case 10: var o = Object.create(null); o.a = value(depth + 1); o.b = 1; return o;
        case 11: return new Proxy(r.chance(2) ? record(depth) : array(depth), {});
        case 12: return JSON.parse(r.pick(['{"a":1,"b":2}', '{"a":1,"b":{"c":[1,2,{"a":3}]}}', '{"b":1,"2":"x","a":3,"1":"y"}', '[1,[2],{"a":3}]']));
        default: return r.pick([new Uint8Array(2), new Map(), new Set(), /x/, new Error("e"), [], {}]);
      }
    }
    function record(depth) {
      var o = {}, n = r.n(6), kinds = ["a", "b", "c", "d", "1", "0", "é"];
      for (var i = 0; i < n; i++) {
        var k = kinds[r.chance(4) ? r.n(kinds.length) : i % kinds.length];
        if (r.chance(10)) Object.defineProperty(o, k, { value: value(depth + 1), enumerable: r.chance(2), writable: true, configurable: true });
        else o[k] = value(depth + 1);
      }
      if (r.chance(12)) o[Symbol.iterator] = 1;
      return o;
    }
    function array(depth) {
      var a = [], n = r.n(5);
      for (var i = 0; i < n; i++) {
        if (r.chance(6)) a.length++;
        else a.push(value(depth + 1));
      }
      return a;
    }
    function inheriting(depth) {
      var o = Object.create(sharedProto);
      o.a = value(depth + 1);
      return o;
    }
    function mutating(depth) {
      var change = mutator(), inner = r.chance(2) ? array(depth) : Object.create(sharedProto);
      if (!Array.isArray(inner)) { inner.a = 1; inner.b = 2; inner.c = 3; }
      if (r.chance(2)) {
        var target = inner;
        return [{ toJSON: function () { change(target); return "changed by toJSON"; } }, target];
      }
      if (Array.isArray(inner)) {
        var keep = inner;
        Object.defineProperty(keep, 0, { get: function () { change(keep); return "changed by a getter"; }, enumerable: true, configurable: true });
      } else {
        var obj = inner, n = 0;
        delete obj.a;
        delete obj.b;
        delete obj.c;
        Object.defineProperty(obj, "a", { get: function () { if (n++ === 0) change(obj); return "changed by a getter"; }, enumerable: true, configurable: true });
        obj.b = 2;
        obj.c = 3;
      }
      return inner;
    }
    var root = value(0);
    return {
      root: root,
      replacer: r.pick([undefined, undefined, undefined,
        function (k, v) { return typeof v === "number" ? v + 1 : v; },
        function (k, v) { return k === "a" ? undefined : v; },
        function (k, v) { if (k === "a" && this && typeof this === "object") { try { delete this.b; } catch (e) { } } return v; },
        ["a", "b", 1, "1", 0],
        ["c", "c", new String("a"), new Number(0)],
        []]),
      space: r.pick([undefined, undefined, 0, 2, 11, -1, "", " ", "--", "é", "abcdefghijkl", new Number(1), new String("x")]),
    };
  }

  // ---- Comparison.

  // same says whether two values parsed from one text are alike, by nothing
  // JSON does.
  function same(a, b) {
    if (typeof a !== typeof b) return false;
    if (typeof a !== "object" || a === null || b === null) return Object.is(a, b);
    if (Array.isArray(a) !== Array.isArray(b)) return false;
    if (Object.getPrototypeOf(a) !== Object.getPrototypeOf(b)) return false;
    var ka = Reflect.ownKeys(a), kb = Reflect.ownKeys(b);
    if (ka.length !== kb.length) return false;
    for (var i = 0; i < ka.length; i++) {
      if (ka[i] !== kb[i]) return false;
      var da = Object.getOwnPropertyDescriptor(a, ka[i]), db = Object.getOwnPropertyDescriptor(b, kb[i]);
      if (da.writable !== db.writable || da.enumerable !== db.enumerable || da.configurable !== db.configurable) return false;
      if (!same(da.value, db.value)) return false;
    }
    return true;
  }
  function attempt(f) {
    try { return { value: f() }; } catch (e) { return { error: e && e.name }; }
  }
  function show(s) {
    var out = "";
    s = String(s);
    for (var i = 0; i < s.length && i < 400; i++) {
      var c = s.charCodeAt(i);
      out += c >= 32 && c < 127 ? s[i] : BS + "u" + hex4(c);
    }
    return out;
  }

  return function (seed, count) {
    var failures = [], r = new Rand(seed);
    function fail(kind, seedOf, detail) {
      if (failures.length < 20) failures.push(kind + " " + seedOf + ": " + detail);
    }
    function checkParse(text, reviver) {
      for (var run = 0; run < 2; run++) {
        var want = attempt(function () { return refJSONParse(text, reviver); });
        var got = attempt(function () { return JSON.parse(text, reviver); });
        if (want.error !== got.error) fail("parse error", text.length, show(text) + ": " + want.error + " / " + got.error);
        else if (!want.error && !same(want.value, got.value)) fail("parse", text.length, show(text));
      }
    }
    var revivers = [
      undefined, undefined,
      function (k, v) { return v; },
      function (k, v) { return k === "a" ? undefined : v; },
      function (k, v) { if (k === "a" && Object(this) === this) delete this.b; return v; },
      function (k, v) { if (k === "id" && Object(this) === this) this.value = [1]; return Array.isArray(v) ? v.length : v; },
    ];
    for (var i = 0; i < count; i++) {
      var s = r.next();
      var d = new Rand(s), doc = makeDocument(d, 0);
      checkParse(doc, d.pick(revivers));
      if (d.chance(3)) checkParse(damaged(d, doc));

      var g = r.next();
      var plan = makeGraph(g);
      var want = attempt(function () { return refStringify(plan.root, plan.replacer, plan.space); });
      for (var run = 0; run < 3; run++) {
        plan = makeGraph(g);
        var got = attempt(function () { return JSON.stringify(plan.root, plan.replacer, plan.space); });
        if (want.error !== got.error) fail("stringify error", g, want.error + " / " + got.error + " run " + run);
        else if (want.value !== got.value) fail("stringify", g, show(want.value) + " / " + show(got.value) + " run " + run);
      }
    }
    return failures.join(String.fromCharCode(10));
  };
})();
