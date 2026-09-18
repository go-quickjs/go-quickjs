package stdlib

import (
	quickjs "github.com/go-quickjs/go-quickjs"
)

// Extras installs the smaller node modules that need no capability:
// querystring, string_decoder, url, and the promise forms of the timers.
//
// None of them is much more than a rearrangement of something already here --
// url is URL under the name node gives it, timers/promises is setTimeout with a
// promise in front -- but code written for node imports them by name, and a
// missing name is a program that does not run at all.
//
// The timer modules are only installed when there is a loop to run them on.
func Extras(rt *quickjs.Runtime, hasTimers bool) error {
	api, err := evalWithHost(rt, "<extras>", extrasJS, quickjs.Value{})
	if err != nil {
		return err
	}
	mods := []string{"querystring", "string_decoder", "url"}
	if hasTimers {
		mods = append(mods, "timers")
	}
	for _, name := range mods {
		v, err := api.Get(name)
		if err != nil {
			return err
		}
		exports, err := moduleExports(rt, v)
		if err != nil {
			return err
		}
		if err := rt.SetModuleValues(name, exports); err != nil {
			return err
		}
		if err := rt.SetModuleValues("node:"+name, exports); err != nil {
			return err
		}
	}
	if !hasTimers {
		return nil
	}
	// timers/promises is the half of the timers that is worth having: an await
	// rather than a callback.
	promises, err := api.Get("timersPromises")
	if err != nil {
		return err
	}
	exports, err := moduleExports(rt, promises)
	if err != nil {
		return err
	}
	if err := rt.SetModuleValues("timers/promises", exports); err != nil {
		return err
	}
	return rt.SetModuleValues("node:timers/promises", exports)
}

// extrasJS is the four of them.
const extrasJS = `(function () {
  "use strict";

  // --- querystring ----------------------------------------------------------

  // querystring predates URLSearchParams and differs from it: it parses into a
  // plain object, and a key that appears twice becomes an array rather than
  // two entries.
  const querystring = {
    parse(text, sep = "&", eq = "=") {
      const out = Object.create(null);
      if (typeof text !== "string" || text === "") return out;
      for (const piece of text.replace(/^[?]/, "").split(sep)) {
        if (piece === "") continue;
        const at = piece.indexOf(eq);
        const key = querystring.unescape(at < 0 ? piece : piece.slice(0, at));
        const value = at < 0 ? "" : querystring.unescape(piece.slice(at + eq.length));
        if (key in out) {
          if (Array.isArray(out[key])) out[key].push(value);
          else out[key] = [out[key], value];
        } else {
          out[key] = value;
        }
      }
      return out;
    },
    stringify(obj, sep = "&", eq = "=") {
      if (obj === null || typeof obj !== "object") return "";
      const parts = [];
      for (const key of Object.keys(obj)) {
        const value = obj[key];
        const name = querystring.escape(key);
        if (Array.isArray(value)) {
          for (const v of value) parts.push(name + eq + querystring.escape(v));
        } else if (typeof value !== "function" && typeof value !== "undefined") {
          parts.push(name + eq + querystring.escape(value));
        }
      }
      return parts.join(sep);
    },
    // A space is + here, as it is in a form, which is the one place this
    // differs from encodeURIComponent.
    escape: (v) => encodeURIComponent(String(v)).replace(/%20/g, "+"),
    unescape(v) {
      try { return decodeURIComponent(String(v).replace(/\+/g, " ")); }
      catch (e) { return String(v); }
    },
  };
  querystring.decode = querystring.parse;
  querystring.encode = querystring.stringify;
  querystring.default = querystring;

  // --- string_decoder -------------------------------------------------------

  // The same problem the text streams have: bytes arrive in pieces, and a
  // character can be split across two of them.
  class StringDecoder {
    constructor(encoding = "utf8") {
      this.encoding = String(encoding).toLowerCase();
      if (this.encoding !== "utf8" && this.encoding !== "utf-8") {
        throw new TypeError("only utf-8 is supported, not " + encoding);
      }
      this._decoder = new TextDecoder("utf-8");
    }
    write(buffer) {
      if (typeof buffer === "string") return buffer;
      return this._decoder.decode(buffer, {stream: true});
    }
    end(buffer) {
      let out = buffer === undefined ? "" : this.write(buffer);
      out += this._decoder.decode();
      return out;
    }
  }

  const string_decoder = {StringDecoder, default: {StringDecoder}};

  // --- url ------------------------------------------------------------------

  // node's url module is the web's URL under another name, with the two
  // functions that turn a path into one and back.
  function fileURLToPath(url) {
    const u = typeof url === "string" ? new URL(url) : url;
    if (u.protocol !== "file:") {
      throw new TypeError("the URL must use the file scheme: " + u.href);
    }
    return decodeURIComponent(u.pathname);
  }

  function pathToFileURL(path) {
    const text = String(path);
    // Every character a path may contain that a URL may not is escaped, and
    // the separators are not.
    const escaped = encodeURI(text).replace(/[?#]/g, (c) =>
      c === "?" ? "%3F" : "%23");
    return new URL("file://" + (escaped.startsWith("/") ? "" : "/") + escaped);
  }

  const url = {
    URL, URLSearchParams, fileURLToPath, pathToFileURL,
    format: (u) => String(u),
    parse: (text) => new URL(text),
    resolve: (from, to) => new URL(to, from).href,
    domainToASCII: (d) => String(d).toLowerCase(),
    domainToUnicode: (d) => String(d),
  };
  url.default = url;

  // --- timers ---------------------------------------------------------------

  const timers = {
    setTimeout: (...a) => setTimeout(...a),
    clearTimeout: (...a) => clearTimeout(...a),
    setInterval: (...a) => setInterval(...a),
    clearInterval: (...a) => clearInterval(...a),
    setImmediate: (fn, ...args) => setTimeout(() => fn(...args), 0),
    clearImmediate: (id) => clearTimeout(id),
  };
  timers.default = timers;

  // The promise forms, which is what makes a wait readable: await delay(100).
  const timersPromises = {
    setTimeout(ms, value, options = {}) {
      return new Promise((resolve, reject) => {
        const id = setTimeout(() => resolve(value), ms);
        watch(options.signal, id, reject, clearTimeout);
      });
    },
    setImmediate(value, options = {}) {
      return timersPromises.setTimeout(0, value, options);
    },
    // An interval as something to iterate, which is the only shape an interval
    // has that an await can use.
    async *setInterval(ms, value, options = {}) {
      let waiting = null;
      let ticks = 0;
      const id = setInterval(() => {
        ticks++;
        if (waiting) { const w = waiting; waiting = null; w(); }
      }, ms);
      try {
        for (;;) {
          if (options.signal && options.signal.aborted) return;
          if (ticks === 0) await new Promise((resolve) => { waiting = resolve; });
          ticks--;
          yield value;
        }
      } finally {
        clearInterval(id);
      }
    },
    scheduler: {
      wait: (ms, options) => timersPromises.setTimeout(ms, undefined, options),
      yield: () => timersPromises.setTimeout(0),
    },
  };
  timersPromises.default = timersPromises;

  function watch(signal, id, reject, cancel) {
    if (!signal) return;
    if (signal.aborted) {
      cancel(id);
      reject(signal.reason || new Error("This operation was aborted"));
      return;
    }
    if (typeof signal.addEventListener === "function") {
      signal.addEventListener("abort", () => {
        cancel(id);
        reject(signal.reason || new Error("This operation was aborted"));
      }, {once: true});
    }
  }

  return {querystring, string_decoder, url, timers, timersPromises};
})`
