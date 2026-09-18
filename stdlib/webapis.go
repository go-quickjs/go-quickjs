package stdlib

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"io"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// WebAPIs installs the things a browser has that the language does not, as far
// as they are pure computation: URL and URLSearchParams, TextEncoder and
// TextDecoder, atob and btoa, structuredClone, performance, AbortController,
// and the parts of crypto that need nothing but entropy.
//
// None of them can reach outside the process. random is where getRandomValues
// draws from, and nil means the system source.
func WebAPIs(rt *quickjs.Runtime, random io.Reader) error {
	if random == nil {
		random = rand.Reader
	}
	started := time.Now()

	host := rt.NewObject()
	if err := setAll(host, map[string]any{
		"now": func() float64 {
			return float64(time.Since(started).Nanoseconds()) / 1e6
		},
		"timeOrigin": float64(started.UnixNano()) / 1e6,
		"randomBytes": func(r *quickjs.Runtime, n float64) (quickjs.Value, error) {
			if n < 0 || n > 65536 {
				return quickjs.Value{}, rt.Throw(rt.NewError("QuotaExceededError",
					"getRandomValues may not be asked for more than 65536 bytes"))
			}
			b := make([]byte, int(n))
			if _, err := io.ReadFull(random, b); err != nil {
				return quickjs.Value{}, err
			}
			return r.NewBytes(b), nil
		},
		"btoa": func(s string) (string, error) {
			// btoa works on bytes, spelled as a string of code units below 256.
			b := make([]byte, 0, len(s))
			for _, r := range []rune(s) {
				if r > 0xFF {
					return "", errors.New(
						"the string contains a character outside the byte range")
				}
				b = append(b, byte(r))
			}
			return base64.StdEncoding.EncodeToString(b), nil
		},
		"atob": func(s string) (string, error) {
			b, err := decodeBase64Loosely(s)
			if err != nil {
				return "", err
			}
			out := make([]rune, len(b))
			for i, c := range b {
				out[i] = rune(c)
			}
			return string(out), nil
		},
		"digest": func(r *quickjs.Runtime, name string, data quickjs.Value) (quickjs.Value, error) {
			b, ok := data.Bytes()
			if !ok {
				return quickjs.Value{}, rt.Throw(rt.NewError("TypeError",
					"the data must be a typed array or an ArrayBuffer"))
			}
			sum, err := digest(name, b)
			if err != nil {
				return quickjs.Value{}, err
			}
			return r.NewBytes(sum), nil
		},
	}); err != nil {
		return err
	}

	api, err := evalWithHost(rt, "<webapis>", webAPIsJS, host)
	if err != nil {
		return err
	}
	// Each name goes on the global object, which is where a program written for
	// a browser looks for it.
	for _, name := range []string{
		"URL", "URLSearchParams", "TextEncoder", "TextDecoder",
		"atob", "btoa", "structuredClone", "performance", "crypto",
		"AbortController", "AbortSignal", "Event", "EventTarget",
	} {
		v, err := api.Get(name)
		if err != nil {
			return err
		}
		if err := rt.Set(name, v); err != nil {
			return err
		}
	}
	return nil
}

// decodeBase64Loosely decodes what atob accepts: padding is optional, and
// whitespace anywhere is ignored.
func decodeBase64Loosely(s string) ([]byte, error) {
	clean := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ', '\t', '\n', '\r', '\f':
		default:
			clean = append(clean, c)
		}
	}
	for len(clean)%4 != 0 {
		clean = append(clean, '=')
	}
	b, err := base64.StdEncoding.DecodeString(string(clean))
	if err != nil {
		return nil, errors.New("the string is not correctly encoded")
	}
	return b, nil
}

// digest is the one-shot hash behind crypto.subtle.digest.
func digest(name string, data []byte) ([]byte, error) {
	switch name {
	case "SHA-1":
		sum := sha1.Sum(data)
		return sum[:], nil
	case "SHA-256":
		sum := sha256.Sum256(data)
		return sum[:], nil
	case "SHA-384":
		sum := sha512.Sum384(data)
		return sum[:], nil
	case "SHA-512":
		sum := sha512.Sum512(data)
		return sum[:], nil
	}
	return nil, errors.New("unsupported digest algorithm: " + name)
}

// webAPIsJS is everything above that is better said in script: the interfaces,
// their getters and setters, and the parsing.
const webAPIsJS = `(function (host) {
  "use strict";

  // --- Text ---------------------------------------------------------------

  // The encoder produces well-formed UTF-8, which means an unpaired surrogate
  // becomes U+FFFD: that is what the standard says, and what makes the result
  // safe to hand to anything expecting UTF-8.
  class TextEncoder {
    get encoding() { return "utf-8"; }
    encode(input = "") {
      const s = String(input);
      const out = [];
      for (let i = 0; i < s.length; i++) {
        let c = s.charCodeAt(i);
        if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) {
          const next = s.charCodeAt(i + 1);
          if (next >= 0xdc00 && next <= 0xdfff) {
            c = 0x10000 + ((c - 0xd800) << 10) + (next - 0xdc00);
            i++;
          }
        }
        if (c >= 0xd800 && c <= 0xdfff) c = 0xfffd;
        if (c < 0x80) out.push(c);
        else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 63));
        else if (c < 0x10000) {
          out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
        } else {
          out.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 63),
                   0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
        }
      }
      return new Uint8Array(out);
    }
    encodeInto(source, dest) {
      const bytes = this.encode(source);
      const n = Math.min(bytes.length, dest.length);
      dest.set(bytes.subarray(0, n));
      // read is in code units of the source; a partial write is reported by
      // written alone, which is enough for the uses this has.
      return {read: source.length, written: n};
    }
  }

  class TextDecoder {
    constructor(label = "utf-8", options = {}) {
      const enc = String(label).toLowerCase();
      if (enc !== "utf-8" && enc !== "utf8" && enc !== "unicode-1-1-utf-8") {
        throw new RangeError("only utf-8 is supported, not " + label);
      }
      this._fatal = !!options.fatal;
      this._bom = !options.ignoreBOM;
      // What a chunk ended in the middle of, kept for the next one.
      this._pending = null;
      this._started = false;
    }
    get encoding() { return "utf-8"; }
    get fatal() { return this._fatal; }
    get ignoreBOM() { return !this._bom; }
    decode(input, options = {}) {
      const streaming = !!options.stream;
      if (input === undefined) {
        // The end of a stream: whatever was left over was never completed.
        const left = this._pending;
        this._pending = null;
        this._started = false;
        return left && !streaming ? this._bad() : "";
      }
      let bytes = input instanceof Uint8Array ? input
        : ArrayBuffer.isView(input)
          ? new Uint8Array(input.buffer, input.byteOffset, input.byteLength)
          : new Uint8Array(input);
      if (this._pending) {
        const joined = new Uint8Array(this._pending.length + bytes.length);
        joined.set(this._pending);
        joined.set(bytes, this._pending.length);
        bytes = joined;
        this._pending = null;
      }
      let out = "";
      let i = 0;
      if (this._bom && !this._started && bytes.length >= 3 &&
          bytes[0] === 0xef && bytes[1] === 0xbb && bytes[2] === 0xbf) {
        i = 3;
      }
      while (i < bytes.length) {
        const b = bytes[i];
        let c, need;
        if (b < 0x80) { c = b; need = 0; }
        else if ((b & 0xe0) === 0xc0) { c = b & 0x1f; need = 1; }
        else if ((b & 0xf0) === 0xe0) { c = b & 0x0f; need = 2; }
        else if ((b & 0xf8) === 0xf0) { c = b & 0x07; need = 3; }
        else { out += this._bad(); i++; continue; }
        if (i + need >= bytes.length) {
          // The chunk ended in the middle of a character. In a stream the rest
          // of it is in the next chunk; otherwise there is no rest.
          if (streaming && need > 0) { this._pending = bytes.slice(i); break; }
          out += this._bad(); i++; continue;
        }
        let ok = true;
        for (let k = 1; k <= need; k++) {
          const cont = bytes[i + k];
          if ((cont & 0xc0) !== 0x80) { ok = false; break; }
          c = (c << 6) | (cont & 63);
        }
        if (!ok) { out += this._bad(); i++; continue; }
        i += need + 1;
        // An overlong form, a surrogate or a value past the last code point is
        // not a character, whatever its bytes say.
        if (c > 0x10ffff || (c >= 0xd800 && c <= 0xdfff) ||
            (need === 1 && c < 0x80) || (need === 2 && c < 0x800) ||
            (need === 3 && c < 0x10000)) {
          out += this._bad();
          continue;
        }
        out += String.fromCodePoint(c);
      }
      this._started = true;
      if (!streaming) {
        // A one-shot decode keeps nothing: the next call starts over.
        this._pending = null;
        this._started = false;
      }
      return out;
    }
    _bad() {
      if (this._fatal) throw new TypeError("the input is not valid utf-8");
      return "�";
    }
  }

  // --- URL ----------------------------------------------------------------

  const specialPorts = {"http:": "80", "https:": "443", "ws:": "80", "wss:": "443", "ftp:": "21"};
  const isSpecial = (scheme) => Object.prototype.hasOwnProperty.call(specialPorts, scheme) || scheme === "file:";

  function encodeSet(s, extra) {
    let out = "";
    for (const ch of s) {
      const code = ch.codePointAt(0);
      if (code <= 0x20 || code >= 0x7f || extra.includes(ch)) {
        for (const b of new TextEncoder().encode(ch)) {
          out += "%" + b.toString(16).toUpperCase().padStart(2, "0");
        }
      } else {
        out += ch;
      }
    }
    return out;
  }

  // Removing dot segments, which is what makes a/b/../c into a/c.
  function normalizePath(path) {
    const parts = path.split("/");
    const out = [];
    for (let i = 0; i < parts.length; i++) {
      const p = parts[i];
      if (p === "." || (p === "" && i > 0 && i < parts.length - 1)) {
        if (p === ".") { if (i === parts.length - 1) out.push(""); }
        continue;
      }
      if (p === "..") {
        if (out.length > 1) out.pop();
        if (i === parts.length - 1) out.push("");
        continue;
      }
      out.push(p);
    }
    let joined = out.join("/");
    if (!joined.startsWith("/")) joined = "/" + joined;
    return joined;
  }

  function parseURL(input, base) {
    let rest = String(input).trim().replace(/[\t\n\r]/g, "");
    const url = {scheme: "", username: "", password: "", host: "", port: "",
                 path: "", query: null, fragment: null};

    const schemeMatch = /^([A-Za-z][A-Za-z0-9+\-.]*):/.exec(rest);
    if (schemeMatch) {
      url.scheme = schemeMatch[1].toLowerCase() + ":";
      rest = rest.slice(schemeMatch[0].length);
    } else if (base) {
      // A relative reference takes everything up to the part it replaces.
      return resolveRelative(rest, base);
    } else {
      throw new TypeError("Invalid URL: " + input);
    }

    if (isSpecial(url.scheme)) rest = rest.replace(/^[\\/]{2}/, "//");
    if (rest.startsWith("//")) {
      rest = rest.slice(2);
      let authority = rest;
      const end = authority.search(/[/?#]/);
      if (end >= 0) { authority = authority.slice(0, end); rest = rest.slice(end); }
      else rest = "";
      const at = authority.lastIndexOf("@");
      if (at >= 0) {
        const creds = authority.slice(0, at);
        authority = authority.slice(at + 1);
        const colon = creds.indexOf(":");
        url.username = colon < 0 ? creds : creds.slice(0, colon);
        url.password = colon < 0 ? "" : creds.slice(colon + 1);
      }
      if (authority.startsWith("[")) {
        const close = authority.indexOf("]");
        url.host = authority.slice(0, close + 1).toLowerCase();
        const after = authority.slice(close + 1);
        if (after.startsWith(":")) url.port = after.slice(1);
      } else {
        const colon = authority.lastIndexOf(":");
        if (colon >= 0) {
          url.host = authority.slice(0, colon).toLowerCase();
          url.port = authority.slice(colon + 1);
        } else {
          url.host = authority.toLowerCase();
        }
      }
      if (url.port !== "" && !/^\d*$/.test(url.port)) {
        throw new TypeError("Invalid URL: " + input);
      }
      if (url.port === specialPorts[url.scheme]) url.port = "";
      if (isSpecial(url.scheme) && url.host === "" && url.scheme !== "file:") {
        throw new TypeError("Invalid URL: " + input);
      }
    } else if (isSpecial(url.scheme) && url.scheme !== "file:") {
      throw new TypeError("Invalid URL: " + input);
    }

    const hash = rest.indexOf("#");
    if (hash >= 0) { url.fragment = rest.slice(hash + 1); rest = rest.slice(0, hash); }
    const q = rest.indexOf("?");
    if (q >= 0) { url.query = rest.slice(q + 1); rest = rest.slice(0, q); }
    url.path = rest;
    if (url.host !== "" || isSpecial(url.scheme)) url.path = normalizePath(url.path);
    return url;
  }

  // A relative reference is resolved against a base, which is RFC 3986's merge.
  function resolveRelative(ref, base) {
    const url = {scheme: base.scheme, username: base.username, password: base.password,
                 host: base.host, port: base.port, path: base.path,
                 query: base.query, fragment: null};
    if (ref === "") { url.query = base.query; return url; }
    if (ref.startsWith("#")) { url.fragment = ref.slice(1); url.query = base.query; return url; }
    if (ref.startsWith("//")) return parseURL(base.scheme + ref);

    let rest = ref;
    const hash = rest.indexOf("#");
    if (hash >= 0) { url.fragment = rest.slice(hash + 1); rest = rest.slice(0, hash); }
    const q = rest.indexOf("?");
    if (q >= 0) { url.query = rest.slice(q + 1); rest = rest.slice(0, q); }
    else url.query = rest === "" ? base.query : null;

    if (rest === "") { url.path = base.path; return url; }
    if (rest.startsWith("/")) url.path = normalizePath(rest);
    else {
      const dir = base.path.slice(0, base.path.lastIndexOf("/") + 1);
      url.path = normalizePath(dir + rest);
    }
    return url;
  }

  function serialize(u, excludeFragment) {
    let out = u.scheme;
    if (u.host !== "" || u.scheme === "file:") {
      out += "//";
      if (u.username !== "" || u.password !== "") {
        out += u.username;
        if (u.password !== "") out += ":" + u.password;
        out += "@";
      }
      out += u.host;
      if (u.port !== "") out += ":" + u.port;
    }
    out += u.path;
    if (u.query !== null) out += "?" + u.query;
    if (!excludeFragment && u.fragment !== null) out += "#" + u.fragment;
    return out;
  }

  const internal = new WeakMap();

  class URLSearchParams {
    constructor(init = "") {
      let pairs = [];
      if (typeof init === "string") {
        pairs = parseQuery(init);
      } else if (init instanceof URLSearchParams) {
        pairs = internal.get(init).pairs.map(p => p.slice());
      } else if (Array.isArray(init)) {
        pairs = init.map(p => [String(p[0]), String(p[1])]);
      } else if (init && typeof init === "object") {
        pairs = Object.entries(init).map(([k, v]) => [String(k), String(v)]);
      }
      internal.set(this, {pairs, url: null});
    }
    get size() { return internal.get(this).pairs.length; }
    append(name, value) {
      internal.get(this).pairs.push([String(name), String(value)]);
      update(this);
    }
    delete(name, value) {
      const state = internal.get(this);
      const n = String(name);
      state.pairs = state.pairs.filter(
        ([k, v]) => k !== n || (value !== undefined && v !== String(value)));
      update(this);
    }
    get(name) {
      const hit = internal.get(this).pairs.find(([k]) => k === String(name));
      return hit === undefined ? null : hit[1];
    }
    getAll(name) {
      return internal.get(this).pairs.filter(([k]) => k === String(name)).map(([, v]) => v);
    }
    has(name, value) {
      return internal.get(this).pairs.some(
        ([k, v]) => k === String(name) && (value === undefined || v === String(value)));
    }
    set(name, value) {
      const state = internal.get(this);
      const n = String(name);
      const at = state.pairs.findIndex(([k]) => k === n);
      if (at < 0) state.pairs.push([n, String(value)]);
      else {
        state.pairs[at] = [n, String(value)];
        state.pairs = state.pairs.filter(([k], i) => k !== n || i === at);
      }
      update(this);
    }
    sort() {
      const state = internal.get(this);
      state.pairs.sort((a, b) => a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0);
      update(this);
    }
    forEach(fn, thisArg) {
      for (const [k, v] of internal.get(this).pairs.slice()) fn.call(thisArg, v, k, this);
    }
    *entries() { for (const p of internal.get(this).pairs.slice()) yield [p[0], p[1]]; }
    *keys() { for (const [k] of internal.get(this).pairs.slice()) yield k; }
    *values() { for (const [, v] of internal.get(this).pairs.slice()) yield v; }
    [Symbol.iterator]() { return this.entries(); }
    toString() {
      return internal.get(this).pairs
        .map(([k, v]) => encodeQuery(k) + "=" + encodeQuery(v)).join("&");
    }
  }

  function parseQuery(s) {
    const out = [];
    for (const part of String(s).replace(/^\?/, "").split("&")) {
      if (part === "") continue;
      const eq = part.indexOf("=");
      const k = eq < 0 ? part : part.slice(0, eq);
      const v = eq < 0 ? "" : part.slice(eq + 1);
      out.push([decodeQuery(k), decodeQuery(v)]);
    }
    return out;
  }

  function decodeQuery(s) {
    try { return decodeURIComponent(s.replace(/\+/g, " ")); }
    catch (e) { return s.replace(/\+/g, " "); }
  }

  function encodeQuery(s) {
    return encodeURIComponent(s).replace(/%20/g, "+").replace(/[!'()~]/g,
      c => "%" + c.charCodeAt(0).toString(16).toUpperCase());
  }

  // Writing to the parameters rewrites the query of the URL they came from.
  function update(params) {
    const state = internal.get(params);
    if (state.url === null) return;
    const text = params.toString();
    internal.get(state.url).url.query = text === "" ? null : text;
  }

  class URL {
    constructor(input, base) {
      let parsedBase = null;
      if (base !== undefined) {
        parsedBase = base instanceof URL ? internal.get(base).url : parseURL(String(base));
      }
      const url = parseURL(String(input), parsedBase);
      const params = new URLSearchParams(url.query === null ? "" : url.query);
      internal.set(this, {url, params});
      internal.get(params).url = this;
    }
    get href() { return serialize(internal.get(this).url); }
    set href(v) {
      const url = parseURL(String(v));
      const state = internal.get(this);
      state.url = url;
      const params = new URLSearchParams(url.query === null ? "" : url.query);
      internal.get(params).url = this;
      state.params = params;
    }
    get protocol() { return internal.get(this).url.scheme; }
    set protocol(v) {
      const s = String(v).replace(/:*$/, "") + ":";
      if (/^[A-Za-z][A-Za-z0-9+\-.]*:$/.test(s)) internal.get(this).url.scheme = s.toLowerCase();
    }
    get username() { return internal.get(this).url.username; }
    set username(v) { internal.get(this).url.username = encodeSet(String(v), ":@/?#"); }
    get password() { return internal.get(this).url.password; }
    set password(v) { internal.get(this).url.password = encodeSet(String(v), ":@/?#"); }
    get host() {
      const u = internal.get(this).url;
      return u.port === "" ? u.host : u.host + ":" + u.port;
    }
    set host(v) {
      const s = String(v);
      const colon = s.lastIndexOf(":");
      const u = internal.get(this).url;
      if (colon > 0 && !s.endsWith("]")) {
        u.host = s.slice(0, colon).toLowerCase();
        u.port = s.slice(colon + 1) === specialPorts[u.scheme] ? "" : s.slice(colon + 1);
      } else {
        u.host = s.toLowerCase();
      }
    }
    get hostname() { return internal.get(this).url.host; }
    set hostname(v) { internal.get(this).url.host = String(v).toLowerCase(); }
    get port() { return internal.get(this).url.port; }
    set port(v) {
      const u = internal.get(this).url;
      const s = String(v);
      if (s === "") { u.port = ""; return; }
      if (/^\d+$/.test(s)) u.port = s === specialPorts[u.scheme] ? "" : s;
    }
    get pathname() { return internal.get(this).url.path; }
    set pathname(v) {
      const u = internal.get(this).url;
      u.path = normalizePath(encodeSet(String(v), "?#"));
    }
    get search() {
      const q = internal.get(this).url.query;
      return q === null || q === "" ? "" : "?" + q;
    }
    set search(v) {
      const s = String(v).replace(/^\?/, "");
      const state = internal.get(this);
      state.url.query = s === "" ? null : encodeSet(s, "#");
      const params = new URLSearchParams(s);
      internal.get(params).url = this;
      state.params = params;
    }
    get searchParams() { return internal.get(this).params; }
    get hash() {
      const f = internal.get(this).url.fragment;
      return f === null || f === "" ? "" : "#" + f;
    }
    set hash(v) {
      const s = String(v).replace(/^#/, "");
      internal.get(this).url.fragment = s === "" ? null : encodeSet(s, "");
    }
    get origin() {
      const u = internal.get(this).url;
      if (!isSpecial(u.scheme) || u.scheme === "file:") return "null";
      return u.scheme + "//" + u.host + (u.port === "" ? "" : ":" + u.port);
    }
    toString() { return this.href; }
    toJSON() { return this.href; }
    static canParse(input, base) {
      try { new URL(input, base); return true; } catch (e) { return false; }
    }
    static parse(input, base) {
      try { return new URL(input, base); } catch (e) { return null; }
    }
  }

  // --- Events and abortion -------------------------------------------------

  class Event {
    constructor(type, init = {}) {
      this.type = String(type);
      this.defaultPrevented = false;
      this.cancelable = !!init.cancelable;
      this.target = null;
    }
    preventDefault() { if (this.cancelable) this.defaultPrevented = true; }
    stopPropagation() {}
    stopImmediatePropagation() {}
  }

  class EventTarget {
    constructor() { Object.defineProperty(this, "_listeners", {value: new Map()}); }
    addEventListener(type, fn, options = {}) {
      if (typeof fn !== "function" && (!fn || typeof fn.handleEvent !== "function")) return;
      const key = String(type);
      if (!this._listeners.has(key)) this._listeners.set(key, []);
      this._listeners.get(key).push({fn, once: !!options.once});
    }
    removeEventListener(type, fn) {
      const list = this._listeners.get(String(type));
      if (!list) return;
      const at = list.findIndex(l => l.fn === fn);
      if (at >= 0) list.splice(at, 1);
    }
    dispatchEvent(event) {
      event.target = this;
      const list = (this._listeners.get(event.type) || []).slice();
      for (const l of list) {
        if (l.once) this.removeEventListener(event.type, l.fn);
        if (typeof l.fn === "function") l.fn.call(this, event);
        else l.fn.handleEvent(event);
      }
      const on = this["on" + event.type];
      if (typeof on === "function") on.call(this, event);
      return !event.defaultPrevented;
    }
  }

  class AbortSignal extends EventTarget {
    constructor() {
      super();
      this.aborted = false;
      this.reason = undefined;
      this.onabort = null;
    }
    throwIfAborted() { if (this.aborted) throw this.reason; }
    static abort(reason) {
      const s = new AbortSignal();
      s.aborted = true;
      s.reason = reason === undefined ? new Error("This operation was aborted") : reason;
      return s;
    }
    static timeout(ms) {
      const s = new AbortSignal();
      if (typeof setTimeout === "function") {
        setTimeout(() => {
          if (s.aborted) return;
          s.aborted = true;
          s.reason = new Error("The operation timed out");
          s.dispatchEvent(new Event("abort"));
        }, ms);
      }
      return s;
    }
  }

  class AbortController {
    constructor() { this.signal = new AbortSignal(); }
    abort(reason) {
      const s = this.signal;
      if (s.aborted) return;
      s.aborted = true;
      s.reason = reason === undefined ? new Error("This operation was aborted") : reason;
      s.dispatchEvent(new Event("abort"));
    }
  }

  // --- Cloning -------------------------------------------------------------

  function structuredClone(value, options) {
    return cloneValue(value, new Map());
  }

  function cloneValue(v, seen) {
    if (v === null || typeof v !== "object") {
      if (typeof v === "function") {
        throw new TypeError("a function could not be cloned");
      }
      if (typeof v === "symbol") {
        throw new TypeError("a symbol could not be cloned");
      }
      return v;
    }
    if (seen.has(v)) return seen.get(v);

    let out;
    const tag = Object.prototype.toString.call(v);
    switch (tag) {
      case "[object Date]": out = new Date(v.getTime()); break;
      case "[object RegExp]": out = new RegExp(v.source, v.flags); break;
      case "[object Array]": out = []; break;
      case "[object Map]": out = new Map(); break;
      case "[object Set]": out = new Set(); break;
      case "[object ArrayBuffer]": return v.slice(0);
      case "[object Error]": {
        out = new v.constructor(v.message);
        if (v.stack !== undefined) out.stack = v.stack;
        break;
      }
      default:
        if (ArrayBuffer.isView(v)) {
          return new v.constructor(cloneValue(v.buffer, seen),
            v.byteOffset, v.length !== undefined ? v.length : undefined);
        }
        out = {};
    }
    seen.set(v, out);
    if (tag === "[object Map]") {
      for (const [k, val] of v) out.set(cloneValue(k, seen), cloneValue(val, seen));
    } else if (tag === "[object Set]") {
      for (const val of v) out.add(cloneValue(val, seen));
    } else if (tag === "[object Array]") {
      for (let i = 0; i < v.length; i++) out[i] = cloneValue(v[i], seen);
    }
    for (const k of Object.keys(v)) {
      if (tag === "[object Array]" && String(Number(k)) === k) continue;
      out[k] = cloneValue(v[k], seen);
    }
    return out;
  }

  // --- The rest ------------------------------------------------------------

  const performance = {
    now: () => host.now(),
    timeOrigin: host.timeOrigin,
    mark() {}, measure() {}, clearMarks() {}, clearMeasures() {},
  };

  const crypto = {
    getRandomValues(view) {
      if (!ArrayBuffer.isView(view) || view instanceof DataView ||
          view instanceof Float32Array || view instanceof Float64Array) {
        throw new TypeError("an integer typed array is required");
      }
      const bytes = host.randomBytes(view.byteLength);
      new Uint8Array(view.buffer, view.byteOffset, view.byteLength).set(bytes);
      return view;
    },
    randomUUID() {
      const b = host.randomBytes(16);
      b[6] = (b[6] & 0x0f) | 0x40;
      b[8] = (b[8] & 0x3f) | 0x80;
      const hex = [];
      for (const x of b) hex.push(x.toString(16).padStart(2, "0"));
      const s = hex.join("");
      return s.slice(0, 8) + "-" + s.slice(8, 12) + "-" + s.slice(12, 16) + "-" +
             s.slice(16, 20) + "-" + s.slice(20);
    },
    subtle: {
      digest(algorithm, data) {
        const name = typeof algorithm === "string" ? algorithm : algorithm && algorithm.name;
        return new Promise((resolve, reject) => {
          try {
            const view = ArrayBuffer.isView(data) ? data : new Uint8Array(data);
            resolve(host.digest(String(name).toUpperCase(), view).buffer);
          } catch (e) { reject(e); }
        });
      },
    },
  };

  return {
    URL, URLSearchParams, TextEncoder, TextDecoder,
    atob: (s) => host.atob(String(s)),
    btoa: (s) => host.btoa(String(s)),
    structuredClone, performance, crypto,
    AbortController, AbortSignal, Event, EventTarget,
  };
})`
