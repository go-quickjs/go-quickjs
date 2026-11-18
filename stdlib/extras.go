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
// url's pathToFileURL and fileURLToPath are node's, path for path and error
// for error (TestURLMatchesNode): a path is taken as path.posix takes it,
// relative ones against process.cwd(), unless options.windows asks for
// path.win32 -- which Install's Config.WindowsPaths makes the default.
//
// The timer modules are only installed when there is a loop to run them on.
func Extras(rt *quickjs.Runtime, hasTimers bool) error {
	return installExtras(rt, hasTimers, false)
}

// installExtras installs them, url's functions taking a path as path.win32
// does where windows is set, as node's do on Windows (Config.WindowsPaths),
// and as path.posix does otherwise.
func installExtras(rt *quickjs.Runtime, hasTimers, windows bool) error {
	pathFn, err := evalInternal(rt, "<eval>", pathJS)
	if err != nil {
		return err
	}
	paths, err := pathFn.Call(windows)
	if err != nil {
		return err
	}
	host := rt.NewObject()
	if err := host.Set("paths", paths); err != nil {
		return err
	}
	if err := host.Set("windows", windows); err != nil {
		return err
	}
	api, err := evalWithHost(rt, "<extras>", extrasJS, host)
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
		exports, err := moduleExports(v)
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
	exports, err := moduleExports(promises)
	if err != nil {
		return err
	}
	if err := rt.SetModuleValues("timers/promises", exports); err != nil {
		return err
	}
	return rt.SetModuleValues("node:timers/promises", exports)
}

// extrasJS is the four of them.
const extrasJS = `(function (host) {
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
  // functions that turn a path into a file URL and back, as node's
  // lib/internal/url.js has them: a path is resolved by path.win32 or
  // path.posix -- path.win32 where the runtime's paths are Windows'
  // (Config.WindowsPaths), unless options.windows says which -- and the
  // same checks made, with node's errors.
  const paths = host.paths;
  const windowsPaths = host.windows === true;

  function codedError(Ctor, code, message) {
    const e = new Ctor(message);
    e.code = code;
    return e;
  }

  function received(v) {
    if (v === null || v === undefined) return " Received " + v;
    if (typeof v === "function") return " Received function " + (v.name || "<anonymous>");
    if (typeof v === "object") {
      if (v.constructor && v.constructor.name) return " Received an instance of " + v.constructor.name;
      return " Received " + String(v);
    }
    let shown = typeof v === "string" ? "'" + v + "'" : typeof v === "bigint" ? v + "n" : String(v);
    if (shown.length > 28) shown = shown.slice(0, 25) + "...";
    return " Received type " + typeof v + " (" + shown + ")";
  }

  // quoted is a string as an error shows the value it refused: quoted, its
  // backslashes, quotes and line breaks escaped.
  function quoted(s) {
    return "'" + s.replace(/\\/g, "\\\\").replace(/'/g, "\\'").replace(/\n/g, "\\n") + "'";
  }

  // A file URL's path has these percent-encoded, as well as the controls,
  // DEL, everything past ASCII (as UTF-8), and a backslash where it is a
  // character of a name rather than a separator (posix).
  const escaped = new Set([" ", "\"", "#", "%", "<", ">", "?", "[", "]", "^", "\x60", "{", "|", "}", "~"]);

  function hex(c) {
    return "%" + (c < 16 ? "0" : "") + c.toString(16).toUpperCase();
  }

  function encodePath(p, windows) {
    let out = "";
    for (const ch of p) {
      const c = ch.codePointAt(0);
      if (ch === "\\") {
        out += windows ? "/" : "%5C";
      } else if (c < 0x21 || c === 0x7f) {
        out += hex(c);
      } else if (c > 0x7f) {
        // A lone surrogate is the replacement character, as a URL takes it.
        out += c >= 0xd800 && c <= 0xdfff ? "%EF%BF%BD" : encodeURIComponent(ch);
      } else {
        out += escaped.has(ch) ? hex(c) : ch;
      }
    }
    return out;
  }

  function pathToFileURL(filepath, options) {
    if (typeof filepath !== "string") {
      throw codedError(TypeError, "ERR_INVALID_ARG_TYPE",
        'The "path" argument must be of type string.' + received(filepath));
    }
    const windows = options?.windows ?? windowsPaths;
    const isUNC = windows && filepath.startsWith("\\\\");
    let resolved = isUNC ? filepath : windows ? paths.win32.resolve(filepath) : paths.posix.resolve(filepath);
    if (isUNC || (windows && resolved.startsWith("\\\\"))) {
      // \\server\share\resource, or \\?\UNC\server\share\resource: the
      // server is the URL's host.
      const prefix = resolved.startsWith("\\\\?\\UNC\\") ? 8 : 2;
      const end = resolved.indexOf("\\", prefix);
      if (end === -1) {
        throw codedError(TypeError, "ERR_INVALID_ARG_VALUE",
          "The argument 'path' Missing UNC resource path. Received " + quoted(resolved));
      }
      if (end === 2) {
        throw codedError(TypeError, "ERR_INVALID_ARG_VALUE",
          "The argument 'path' Empty UNC servername. Received " + quoted(resolved));
      }
      return new URL("file://" + resolved.slice(prefix, end) + encodePath(resolved.slice(end), true));
    }
    // Resolving drops a trailing separator, which the URL keeps.
    const last = filepath.charCodeAt(filepath.length - 1);
    if ((last === 47 || (windows && last === 92)) && resolved[resolved.length - 1] !== (windowsPaths ? "\\" : "/")) {
      resolved += "/";
    }
    return new URL("file://" + (resolved.startsWith("/") ? "" : "/") + encodePath(resolved, windows));
  }

  function isURL(v) {
    return Boolean(v?.href && v.protocol && v.auth === undefined && v.path === undefined);
  }

  function fileURLToPath(path, options = undefined) {
    const windows = options?.windows ?? windowsPaths;
    if (typeof path === "string") {
      path = new URL(path);
    } else if (!isURL(path)) {
      throw codedError(TypeError, "ERR_INVALID_ARG_TYPE",
        'The "path" argument must be of type string or an instance of URL.' + received(path));
    }
    if (path.protocol !== "file:") {
      throw codedError(TypeError, "ERR_INVALID_URL_SCHEME", "The URL must be of scheme file");
    }
    return windows ? win32PathOf(path) : posixPathOf(path);
  }

  function win32PathOf(url) {
    let pathname = url.pathname;
    for (let n = 0; n < pathname.length; n++) {
      if (pathname[n] === "%") {
        const third = pathname.codePointAt(n + 2) | 0x20;
        if ((pathname[n + 1] === "2" && third === 102) || (pathname[n + 1] === "5" && third === 99)) {
          throw codedError(TypeError, "ERR_INVALID_FILE_URL_PATH",
            "File URL path must not include encoded \\ or / characters");
        }
      }
    }
    pathname = decodeURIComponent(pathname.replace(/\//g, "\\"));
    if (url.hostname !== "") {
      // A host is a UNC path's server.
      return "\\\\" + url.hostname + pathname;
    }
    // Otherwise the path starts with a drive.
    const letter = pathname.codePointAt(1) | 0x20;
    if (letter < 97 || letter > 122 || pathname.charAt(2) !== ":") {
      throw codedError(TypeError, "ERR_INVALID_FILE_URL_PATH", "File URL path must be absolute");
    }
    return pathname.slice(1);
  }

  function posixPathOf(url) {
    if (url.hostname !== "") {
      const platform = globalThis.process?.platform ?? (windowsPaths ? "win32" : "linux");
      throw codedError(TypeError, "ERR_INVALID_FILE_URL_HOST",
        'File URL host must be "localhost" or empty on ' + platform);
    }
    const pathname = url.pathname;
    for (let n = 0; n < pathname.length; n++) {
      if (pathname[n] === "%") {
        const third = pathname.codePointAt(n + 2) | 0x20;
        if (pathname[n + 1] === "2" && third === 102) {
          throw codedError(TypeError, "ERR_INVALID_FILE_URL_PATH",
            "File URL path must not include encoded / characters");
        }
      }
    }
    return decodeURIComponent(pathname);
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
