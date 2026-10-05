package stdlib

import (
	quickjs "github.com/go-quickjs/go-quickjs"
)

// Path installs the "path" module, which is pure computation: it works on the
// text of a path and never asks the filesystem anything, so a runtime with no
// filesystem at all can still use it.
//
// It is node's: path.posix and path.win32, each as node has it, and the
// "path/posix" and "path/win32" modules. path itself is path.posix, whatever
// the host runs on, which is what a program that also runs in a browser or a
// container expects, and what a root the host confines files to is written
// in. Config.WindowsPaths makes it path.win32, as node's is on Windows.
//
// path.resolve and path.relative resolve a relative path against
// process.cwd(), read when they are called, as node's do; with no process,
// against the root.
func Path(rt *quickjs.Runtime) error {
	return installPath(rt, false)
}

// installPath installs path, as path.win32 if windows is set.
func installPath(rt *quickjs.Runtime, windows bool) error {
	f, err := rt.Eval(pathJS)
	if err != nil {
		return err
	}
	both, err := f.Call(windows)
	if err != nil {
		return err
	}
	for _, flavor := range []string{"posix", "win32"} {
		mod, err := both.Get(flavor)
		if err != nil {
			return err
		}
		if err := setPathModule(rt, "path/"+flavor, mod); err != nil {
			return err
		}
	}
	def := "posix"
	if windows {
		def = "win32"
	}
	mod, err := both.Get(def)
	if err != nil {
		return err
	}
	return setPathModule(rt, "path", mod)
}

// setPathModule installs one flavor of path under a name: its functions as
// named exports and itself as the default, as node's module gives them.
func setPathModule(rt *quickjs.Runtime, name string, mod quickjs.Value) error {
	exports := map[string]any{"default": mod}
	for _, k := range mod.Keys() {
		v, err := mod.Get(k)
		if err != nil {
			return err
		}
		exports[k] = v
	}
	return setNodeModule(rt, name, exports)
}

// copyExports is the default export: the same names, in an object.
//
// A module that is imported both ways -- `import path from "path"` and
// `import {join} from "path"` -- has to offer both, and the default cannot be
// the namespace itself, which is not an ordinary object.
func copyExports(exports map[string]any) map[string]any {
	out := make(map[string]any, len(exports))
	for k, v := range exports {
		if k == "default" {
			continue
		}
		out[k] = v
	}
	return out
}

// pathJS is node's path module, both flavors, as node's lib/path.js has them:
// the same algorithms, character by character, so that every path comes out
// as node's does -- which TestPathMatchesNode checks, over a corpus of the
// awkward ones, against what node answered.
const pathJS = `(function (windows) {
  "use strict";

  const SLASH = 47, BACKSLASH = 92, DOT = 46, COLON = 58, QUESTION = 63;

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

  function validateString(value, name) {
    if (typeof value !== "string") {
      const e = new TypeError('The "' + name + '" argument must be of type string.' + received(value));
      e.code = "ERR_INVALID_ARG_TYPE";
      throw e;
    }
  }

  function validateObject(value, name) {
    if (value === null || typeof value !== "object") {
      const e = new TypeError('The "' + name + '" argument must be of type object.' + received(value));
      e.code = "ERR_INVALID_ARG_TYPE";
      throw e;
    }
  }

  function isPathSeparator(code) {
    return code === SLASH || code === BACKSLASH;
  }

  function isPosixPathSeparator(code) {
    return code === SLASH;
  }

  function isWindowsDeviceRoot(code) {
    return (code >= 65 && code <= 90) || (code >= 97 && code <= 122);
  }

  function cwd() {
    const p = globalThis.process;
    if (p && typeof p.cwd === "function") return p.cwd();
    return windows ? "\\" : "/";
  }

  // On Windows, node's posix functions take the working directory with its
  // separators turned and its drive left off.
  function posixCwd() {
    if (!windows) return cwd();
    const c = cwd().replace(/\\/g, "/");
    return c.slice(c.indexOf("/"));
  }

  function env(name) {
    const p = globalThis.process;
    return p && p.env ? p.env[name] : undefined;
  }

  // normalizeString resolves . and .. elements in a path with directory
  // names.
  function normalizeString(path, allowAboveRoot, separator, isSeparator) {
    let res = "";
    let lastSegmentLength = 0;
    let lastSlash = -1;
    let dots = 0;
    let code = 0;
    for (let i = 0; i <= path.length; ++i) {
      if (i < path.length) code = path.charCodeAt(i);
      else if (isSeparator(code)) break;
      else code = SLASH;

      if (isSeparator(code)) {
        if (lastSlash === i - 1 || dots === 1) {
          // NOOP
        } else if (dots === 2) {
          if (res.length < 2 || lastSegmentLength !== 2 ||
              res.charCodeAt(res.length - 1) !== DOT ||
              res.charCodeAt(res.length - 2) !== DOT) {
            if (res.length > 2) {
              const lastSlashIndex = res.lastIndexOf(separator);
              if (lastSlashIndex === -1) {
                res = "";
                lastSegmentLength = 0;
              } else {
                res = res.slice(0, lastSlashIndex);
                lastSegmentLength = res.length - 1 - res.lastIndexOf(separator);
              }
              lastSlash = i;
              dots = 0;
              continue;
            } else if (res.length !== 0) {
              res = "";
              lastSegmentLength = 0;
              lastSlash = i;
              dots = 0;
              continue;
            }
          }
          if (allowAboveRoot) {
            res += res.length > 0 ? separator + ".." : "..";
            lastSegmentLength = 2;
          }
        } else {
          if (res.length > 0) res += separator + path.slice(lastSlash + 1, i);
          else res = path.slice(lastSlash + 1, i);
          lastSegmentLength = i - lastSlash - 1;
        }
        lastSlash = i;
        dots = 0;
      } else if (code === DOT && dots !== -1) {
        ++dots;
      } else {
        dots = -1;
      }
    }
    return res;
  }

  function formatExt(ext) {
    return ext ? (ext[0] === "." ? "" : ".") + ext : "";
  }

  function format(sep, pathObject) {
    validateObject(pathObject, "pathObject");
    const dir = pathObject.dir || pathObject.root;
    const base = pathObject.base || ((pathObject.name || "") + formatExt(pathObject.ext));
    if (!dir) return base;
    return dir === pathObject.root ? dir + base : dir + sep + base;
  }

  // The parts of an extension search, from the end of a path to start:
  // where the last dot is, where the last part begins -- startPart, unless
  // a separator says -- and where it ends.
  function lastPart(path, start, startPart, isSeparator) {
    let startDot = -1, end = -1, matchedSlash = true, preDotState = 0;
    for (let i = path.length - 1; i >= start; --i) {
      const code = path.charCodeAt(i);
      if (isSeparator(code)) {
        if (!matchedSlash) {
          startPart = i + 1;
          break;
        }
        continue;
      }
      if (end === -1) {
        matchedSlash = false;
        end = i + 1;
      }
      if (code === DOT) {
        if (startDot === -1) startDot = i;
        else if (preDotState !== 1) preDotState = 1;
      } else if (startDot !== -1) {
        preDotState = -1;
      }
    }
    const noExt = startDot === -1 || end === -1 || preDotState === 0 ||
      (preDotState === 1 && startDot === end - 1 && startDot === startPart + 1);
    return { startDot, startPart, end, noExt };
  }

  function basename(path, suffix, start, isSeparator) {
    let end = -1, matchedSlash = true;
    if (suffix !== undefined && suffix.length > 0 && suffix.length <= path.length) {
      if (suffix === path) return "";
      let extIdx = suffix.length - 1;
      let firstNonSlashEnd = -1;
      for (let i = path.length - 1; i >= start; --i) {
        const code = path.charCodeAt(i);
        if (isSeparator(code)) {
          if (!matchedSlash) {
            start = i + 1;
            break;
          }
        } else {
          if (firstNonSlashEnd === -1) {
            matchedSlash = false;
            firstNonSlashEnd = i + 1;
          }
          if (extIdx >= 0) {
            if (code === suffix.charCodeAt(extIdx)) {
              if (--extIdx === -1) end = i;
            } else {
              extIdx = -1;
              end = firstNonSlashEnd;
            }
          }
        }
      }
      if (start === end) end = firstNonSlashEnd;
      else if (end === -1) end = path.length;
      return path.slice(start, end);
    }
    for (let i = path.length - 1; i >= start; --i) {
      if (isSeparator(path.charCodeAt(i))) {
        if (!matchedSlash) {
          start = i + 1;
          break;
        }
      } else if (end === -1) {
        matchedSlash = false;
        end = i + 1;
      }
    }
    if (end === -1) return "";
    return path.slice(start, end);
  }

  // uncRoot reads a path that begins with two separators as a UNC root,
  // \\server\share: it returns where the server's name and the share's
  // begin and end, or null for a path that is not one.
  function uncRoot(path) {
    const len = path.length;
    let j = 2, last = j;
    while (j < len && !isPathSeparator(path.charCodeAt(j))) j++;
    if (j >= len || j === last) return null;
    const server = path.slice(last, j);
    last = j;
    while (j < len && isPathSeparator(path.charCodeAt(j))) j++;
    if (j >= len || j === last) return null;
    last = j;
    while (j < len && !isPathSeparator(path.charCodeAt(j))) j++;
    return { server, shareStart: last, shareEnd: j };
  }

  const win32 = {
    resolve(...args) {
      let resolvedDevice = "";
      let resolvedTail = "";
      let resolvedAbsolute = false;
      for (let i = args.length - 1; i >= -1; i--) {
        let path;
        if (i >= 0) {
          path = args[i];
          validateString(path, "paths[" + i + "]");
          if (path.length === 0) continue;
        } else if (resolvedDevice.length === 0) {
          path = cwd();
        } else {
          // Windows keeps a working directory for each drive, in the
          // environment, which a path on another drive than the current
          // one is resolved against.
          path = env("=" + resolvedDevice) || cwd();
          if (path === undefined ||
              (path.slice(0, 2).toLowerCase() !== resolvedDevice.toLowerCase() &&
               path.charCodeAt(2) === BACKSLASH)) {
            path = resolvedDevice + "\\";
          }
        }
        const len = path.length;
        let rootEnd = 0;
        let device = "";
        let isAbsolute = false;
        const code = path.charCodeAt(0);
        if (len === 1) {
          if (isPathSeparator(code)) {
            rootEnd = 1;
            isAbsolute = true;
          }
        } else if (isPathSeparator(code)) {
          isAbsolute = true;
          if (isPathSeparator(path.charCodeAt(1))) {
            const unc = uncRoot(path);
            if (unc !== null && (unc.shareEnd === len || unc.shareEnd !== unc.shareStart)) {
              if (unc.server !== "." && unc.server !== "?") {
                device = "\\\\" + unc.server + "\\" + path.slice(unc.shareStart, unc.shareEnd);
                rootEnd = unc.shareEnd;
              } else {
                // A device root, \\.\PHYSICALDRIVE0.
                device = "\\\\" + unc.server;
                rootEnd = 4;
              }
            }
          } else {
            rootEnd = 1;
          }
        } else if (isWindowsDeviceRoot(code) && path.charCodeAt(1) === COLON) {
          device = path.slice(0, 2);
          rootEnd = 2;
          if (len > 2 && isPathSeparator(path.charCodeAt(2))) {
            isAbsolute = true;
            rootEnd = 3;
          }
        }
        if (device.length > 0) {
          if (resolvedDevice.length > 0) {
            if (device.toLowerCase() !== resolvedDevice.toLowerCase()) continue;
          } else {
            resolvedDevice = device;
          }
        }
        if (resolvedAbsolute) {
          if (resolvedDevice.length > 0) break;
        } else {
          resolvedTail = path.slice(rootEnd) + "\\" + resolvedTail;
          resolvedAbsolute = isAbsolute;
          if (isAbsolute && resolvedDevice.length > 0) break;
        }
      }
      resolvedTail = normalizeString(resolvedTail, !resolvedAbsolute, "\\", isPathSeparator);
      return resolvedAbsolute ?
        resolvedDevice + "\\" + resolvedTail :
        (resolvedDevice + resolvedTail) || ".";
    },

    normalize(path) {
      validateString(path, "path");
      const len = path.length;
      if (len === 0) return ".";
      let rootEnd = 0;
      let device;
      let isAbsolute = false;
      const code = path.charCodeAt(0);
      if (len === 1) return isPosixPathSeparator(code) ? "\\" : path;
      if (isPathSeparator(code)) {
        isAbsolute = true;
        if (isPathSeparator(path.charCodeAt(1))) {
          const unc = uncRoot(path);
          if (unc !== null && (unc.shareEnd === len || unc.shareEnd !== unc.shareStart)) {
            if (unc.server === "." || unc.server === "?") {
              // A device root, \\.\PHYSICALDRIVE0.
              device = "\\\\" + unc.server;
              rootEnd = 4;
            } else if (unc.shareEnd === len) {
              // A UNC root alone.
              return "\\\\" + unc.server + "\\" + path.slice(unc.shareStart) + "\\";
            } else {
              device = "\\\\" + unc.server + "\\" + path.slice(unc.shareStart, unc.shareEnd);
              rootEnd = unc.shareEnd;
            }
          }
        } else {
          rootEnd = 1;
        }
      } else if (isWindowsDeviceRoot(code) && path.charCodeAt(1) === COLON) {
        device = path.slice(0, 2);
        rootEnd = 2;
        if (len > 2 && isPathSeparator(path.charCodeAt(2))) {
          isAbsolute = true;
          rootEnd = 3;
        }
      }
      let tail = rootEnd < len ?
        normalizeString(path.slice(rootEnd), !isAbsolute, "\\", isPathSeparator) : "";
      if (tail.length === 0 && !isAbsolute) tail = ".";
      if (tail.length > 0 && isPathSeparator(path.charCodeAt(len - 1))) tail += "\\";
      if (!isAbsolute && device === undefined && path.includes(":")) {
        // A relative path must not come out as one Windows would read as
        // absolute, or as on a drive: a tail that begins "C:", or a first
        // part that ends in a colon.
        if (tail.length >= 2 && isWindowsDeviceRoot(tail.charCodeAt(0)) && tail.charCodeAt(1) === COLON) {
          return ".\\" + tail;
        }
        let index = path.indexOf(":");
        do {
          if (index === len - 1 || isPathSeparator(path.charCodeAt(index + 1))) {
            return ".\\" + tail;
          }
        } while ((index = path.indexOf(":", index + 1)) !== -1);
      }
      if (device === undefined) return isAbsolute ? "\\" + tail : tail;
      return isAbsolute ? device + "\\" + tail : device + tail;
    },

    isAbsolute(path) {
      validateString(path, "path");
      const len = path.length;
      if (len === 0) return false;
      const code = path.charCodeAt(0);
      return isPathSeparator(code) ||
        (len > 2 && isWindowsDeviceRoot(code) && path.charCodeAt(1) === COLON &&
         isPathSeparator(path.charCodeAt(2)));
    },

    join(...args) {
      if (args.length === 0) return ".";
      let joined;
      let firstPart;
      for (let i = 0; i < args.length; ++i) {
        const arg = args[i];
        validateString(arg, "path");
        if (arg.length > 0) {
          if (joined === undefined) joined = firstPart = arg;
          else joined += "\\" + arg;
        }
      }
      if (joined === undefined) return ".";
      // The joined path must not begin with two separators unless the first
      // part did, which normalize would take for a UNC root.
      let needsReplace = true;
      let slashCount = 0;
      if (isPathSeparator(firstPart.charCodeAt(0))) {
        ++slashCount;
        const firstLen = firstPart.length;
        if (firstLen > 1 && isPathSeparator(firstPart.charCodeAt(1))) {
          ++slashCount;
          if (firstLen > 2) {
            if (isPathSeparator(firstPart.charCodeAt(2))) ++slashCount;
            else needsReplace = false;
          }
        }
      }
      if (needsReplace) {
        while (slashCount < joined.length && isPathSeparator(joined.charCodeAt(slashCount))) slashCount++;
        if (slashCount >= 2) joined = "\\" + joined.slice(slashCount);
      }
      return win32.normalize(joined);
    },

    relative(from, to) {
      validateString(from, "from");
      validateString(to, "to");
      if (from === to) return "";
      const fromOrig = win32.resolve(from);
      const toOrig = win32.resolve(to);
      if (fromOrig === toOrig) return "";
      from = fromOrig.toLowerCase();
      to = toOrig.toLowerCase();
      if (from === to) return "";
      let fromStart = 0;
      while (fromStart < from.length && from.charCodeAt(fromStart) === BACKSLASH) fromStart++;
      let fromEnd = from.length;
      while (fromEnd - 1 > fromStart && from.charCodeAt(fromEnd - 1) === BACKSLASH) fromEnd--;
      const fromLen = fromEnd - fromStart;
      let toStart = 0;
      while (toStart < to.length && to.charCodeAt(toStart) === BACKSLASH) toStart++;
      let toEnd = to.length;
      while (toEnd - 1 > toStart && to.charCodeAt(toEnd - 1) === BACKSLASH) toEnd--;
      const toLen = toEnd - toStart;
      const length = fromLen < toLen ? fromLen : toLen;
      let lastCommonSep = -1;
      let i = 0;
      for (; i < length; i++) {
        const fromCode = from.charCodeAt(fromStart + i);
        if (fromCode !== to.charCodeAt(toStart + i)) break;
        else if (fromCode === BACKSLASH) lastCommonSep = i;
      }
      if (i !== length) {
        // They differ before a separator they share: there is no path from
        // one to the other but the other.
        if (lastCommonSep === -1) return toOrig;
      } else {
        if (toLen > length) {
          if (to.charCodeAt(toStart + i) === BACKSLASH) return toOrig.slice(toStart + i + 1);
          if (i === 2) return toOrig.slice(toStart + i);
        }
        if (fromLen > length) {
          if (from.charCodeAt(fromStart + i) === BACKSLASH) lastCommonSep = i;
          else if (i === 2) lastCommonSep = 3;
        }
        if (lastCommonSep === -1) lastCommonSep = 0;
      }
      let out = "";
      for (i = fromStart + lastCommonSep + 1; i <= fromEnd; ++i) {
        if (i === fromEnd || from.charCodeAt(i) === BACKSLASH) out += out.length === 0 ? ".." : "\\..";
      }
      toStart += lastCommonSep;
      if (out.length > 0) return out + toOrig.slice(toStart, toEnd);
      if (toOrig.charCodeAt(toStart) === BACKSLASH) ++toStart;
      return toOrig.slice(toStart, toEnd);
    },

    toNamespacedPath(path) {
      if (typeof path !== "string" || path.length === 0) return path;
      const resolvedPath = win32.resolve(path);
      if (resolvedPath.length <= 2) return path;
      if (resolvedPath.charCodeAt(0) === BACKSLASH) {
        if (resolvedPath.charCodeAt(1) === BACKSLASH) {
          const code = resolvedPath.charCodeAt(2);
          if (code !== QUESTION && code !== DOT) return "\\\\?\\UNC\\" + resolvedPath.slice(2);
        }
      } else if (isWindowsDeviceRoot(resolvedPath.charCodeAt(0)) &&
                 resolvedPath.charCodeAt(1) === COLON &&
                 resolvedPath.charCodeAt(2) === BACKSLASH) {
        return "\\\\?\\" + resolvedPath;
      }
      return resolvedPath;
    },

    dirname(path) {
      validateString(path, "path");
      const len = path.length;
      if (len === 0) return ".";
      let rootEnd = -1;
      let offset = 0;
      const code = path.charCodeAt(0);
      if (len === 1) return isPathSeparator(code) ? path : ".";
      if (isPathSeparator(code)) {
        rootEnd = offset = 1;
        if (isPathSeparator(path.charCodeAt(1))) {
          const unc = uncRoot(path);
          if (unc !== null) {
            if (unc.shareEnd === len) return path;
            if (unc.shareEnd !== unc.shareStart) rootEnd = offset = unc.shareEnd + 1;
          }
        }
      } else if (isWindowsDeviceRoot(code) && path.charCodeAt(1) === COLON) {
        rootEnd = len > 2 && isPathSeparator(path.charCodeAt(2)) ? 3 : 2;
        offset = rootEnd;
      }
      let end = -1;
      let matchedSlash = true;
      for (let i = len - 1; i >= offset; --i) {
        if (isPathSeparator(path.charCodeAt(i))) {
          if (!matchedSlash) {
            end = i;
            break;
          }
        } else {
          matchedSlash = false;
        }
      }
      if (end === -1) {
        if (rootEnd === -1) return ".";
        end = rootEnd;
      }
      return path.slice(0, end);
    },

    basename(path, suffix) {
      if (suffix !== undefined) validateString(suffix, "suffix");
      validateString(path, "path");
      // A drive letter is not a part, and the separator after it not one at
      // the end.
      const start = path.length >= 2 && isWindowsDeviceRoot(path.charCodeAt(0)) &&
        path.charCodeAt(1) === COLON ? 2 : 0;
      return basename(path, suffix, start, isPathSeparator);
    },

    extname(path) {
      validateString(path, "path");
      const start = path.length >= 2 && path.charCodeAt(1) === COLON &&
        isWindowsDeviceRoot(path.charCodeAt(0)) ? 2 : 0;
      const p = lastPart(path, start, start, isPathSeparator);
      return p.noExt ? "" : path.slice(p.startDot, p.end);
    },

    format: (pathObject) => format("\\", pathObject),

    parse(path) {
      validateString(path, "path");
      const ret = { root: "", dir: "", base: "", ext: "", name: "" };
      if (path.length === 0) return ret;
      const len = path.length;
      let rootEnd = 0;
      const code = path.charCodeAt(0);
      if (len === 1) {
        if (isPathSeparator(code)) {
          ret.root = ret.dir = path;
          return ret;
        }
        ret.base = ret.name = path;
        return ret;
      }
      if (isPathSeparator(code)) {
        rootEnd = 1;
        if (isPathSeparator(path.charCodeAt(1))) {
          const unc = uncRoot(path);
          if (unc !== null) {
            if (unc.shareEnd === len) rootEnd = unc.shareEnd;
            else if (unc.shareEnd !== unc.shareStart) rootEnd = unc.shareEnd + 1;
          }
        }
      } else if (isWindowsDeviceRoot(code) && path.charCodeAt(1) === COLON) {
        if (len <= 2) {
          ret.root = ret.dir = path;
          return ret;
        }
        rootEnd = 2;
        if (isPathSeparator(path.charCodeAt(2))) {
          if (len === 3) {
            ret.root = ret.dir = path;
            return ret;
          }
          rootEnd = 3;
        }
      }
      if (rootEnd > 0) ret.root = path.slice(0, rootEnd);
      const p = lastPart(path, rootEnd, rootEnd, isPathSeparator);
      if (p.end !== -1) {
        if (p.noExt) {
          ret.base = ret.name = path.slice(p.startPart, p.end);
        } else {
          ret.name = path.slice(p.startPart, p.startDot);
          ret.base = path.slice(p.startPart, p.end);
          ret.ext = path.slice(p.startDot, p.end);
        }
      }
      if (p.startPart > 0 && p.startPart !== rootEnd) ret.dir = path.slice(0, p.startPart - 1);
      else ret.dir = ret.root;
      return ret;
    },

    sep: "\\",
    delimiter: ";",
    win32: null,
    posix: null,
  };

  const posix = {
    resolve(...args) {
      let resolvedPath = "";
      let resolvedAbsolute = false;
      for (let i = args.length - 1; i >= -1 && !resolvedAbsolute; i--) {
        const path = i >= 0 ? args[i] : posixCwd();
        validateString(path, "paths[" + i + "]");
        if (path.length === 0) continue;
        resolvedPath = path + "/" + resolvedPath;
        resolvedAbsolute = path.charCodeAt(0) === SLASH;
      }
      resolvedPath = normalizeString(resolvedPath, !resolvedAbsolute, "/", isPosixPathSeparator);
      if (resolvedAbsolute) return "/" + resolvedPath;
      return resolvedPath.length > 0 ? resolvedPath : ".";
    },

    normalize(path) {
      validateString(path, "path");
      if (path.length === 0) return ".";
      const isAbsolute = path.charCodeAt(0) === SLASH;
      const trailingSeparator = path.charCodeAt(path.length - 1) === SLASH;
      path = normalizeString(path, !isAbsolute, "/", isPosixPathSeparator);
      if (path.length === 0) {
        if (isAbsolute) return "/";
        return trailingSeparator ? "./" : ".";
      }
      if (trailingSeparator) path += "/";
      return isAbsolute ? "/" + path : path;
    },

    isAbsolute(path) {
      validateString(path, "path");
      return path.length > 0 && path.charCodeAt(0) === SLASH;
    },

    join(...args) {
      if (args.length === 0) return ".";
      let joined;
      for (let i = 0; i < args.length; ++i) {
        const arg = args[i];
        validateString(arg, "path");
        if (arg.length > 0) {
          if (joined === undefined) joined = arg;
          else joined += "/" + arg;
        }
      }
      if (joined === undefined) return ".";
      return posix.normalize(joined);
    },

    relative(from, to) {
      validateString(from, "from");
      validateString(to, "to");
      if (from === to) return "";
      from = posix.resolve(from);
      to = posix.resolve(to);
      if (from === to) return "";
      const fromStart = 1;
      const fromEnd = from.length;
      const fromLen = fromEnd - fromStart;
      const toStart = 1;
      const toLen = to.length - toStart;
      const length = fromLen < toLen ? fromLen : toLen;
      let lastCommonSep = -1;
      let i = 0;
      for (; i < length; i++) {
        const fromCode = from.charCodeAt(fromStart + i);
        if (fromCode !== to.charCodeAt(toStart + i)) break;
        else if (fromCode === SLASH) lastCommonSep = i;
      }
      if (i === length) {
        if (toLen > length) {
          if (to.charCodeAt(toStart + i) === SLASH) return to.slice(toStart + i + 1);
          if (i === 0) return to.slice(toStart + i);
        } else if (fromLen > length) {
          if (from.charCodeAt(fromStart + i) === SLASH) lastCommonSep = i;
          else if (i === 0) lastCommonSep = 0;
        }
      }
      let out = "";
      for (i = fromStart + lastCommonSep + 1; i <= fromEnd; ++i) {
        if (i === fromEnd || from.charCodeAt(i) === SLASH) out += out.length === 0 ? ".." : "/..";
      }
      return out + to.slice(toStart + lastCommonSep);
    },

    toNamespacedPath(path) {
      return path;
    },

    dirname(path) {
      validateString(path, "path");
      if (path.length === 0) return ".";
      const hasRoot = path.charCodeAt(0) === SLASH;
      let end = -1;
      let matchedSlash = true;
      for (let i = path.length - 1; i >= 1; --i) {
        if (path.charCodeAt(i) === SLASH) {
          if (!matchedSlash) {
            end = i;
            break;
          }
        } else {
          matchedSlash = false;
        }
      }
      if (end === -1) return hasRoot ? "/" : ".";
      if (hasRoot && end === 1) return "//";
      return path.slice(0, end);
    },

    basename(path, suffix) {
      if (suffix !== undefined) validateString(suffix, "suffix");
      validateString(path, "path");
      return basename(path, suffix, 0, isPosixPathSeparator);
    },

    extname(path) {
      validateString(path, "path");
      const p = lastPart(path, 0, 0, isPosixPathSeparator);
      return p.noExt ? "" : path.slice(p.startDot, p.end);
    },

    format: (pathObject) => format("/", pathObject),

    parse(path) {
      validateString(path, "path");
      const ret = { root: "", dir: "", base: "", ext: "", name: "" };
      if (path.length === 0) return ret;
      const isAbsolute = path.charCodeAt(0) === SLASH;
      let start;
      if (isAbsolute) {
        ret.root = "/";
        start = 1;
      } else {
        start = 0;
      }
      const p = lastPart(path, start, 0, isPosixPathSeparator);
      if (p.end !== -1) {
        const s = p.startPart === 0 && isAbsolute ? 1 : p.startPart;
        if (p.noExt) {
          ret.base = ret.name = path.slice(s, p.end);
        } else {
          ret.name = path.slice(s, p.startDot);
          ret.base = path.slice(s, p.end);
          ret.ext = path.slice(p.startDot, p.end);
        }
      }
      if (p.startPart > 0) ret.dir = path.slice(0, p.startPart - 1);
      else if (isAbsolute) ret.dir = "/";
      return ret;
    },

    sep: "/",
    delimiter: ":",
    win32: null,
    posix: null,
  };

  posix.win32 = win32.win32 = win32;
  posix.posix = win32.posix = posix;
  // Node's legacy name for toNamespacedPath.
  win32._makeLong = win32.toNamespacedPath;
  posix._makeLong = posix.toNamespacedPath;
  return { posix, win32 };
})`
