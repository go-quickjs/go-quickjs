// The url corpus: what url.pathToFileURL and url.fileURLToPath answer, in
// both flavors, for the paths and URLs that are awkward -- relative paths,
// drives, UNC shares, separators of both kinds, trailing separators, every
// character a URL escapes, encoded separators, hosts -- and the errors they
// throw. It runs unchanged in node, where urlcorpus_node.js records the
// answers in url_node.json, and in the engine, where TestURLMatchesNode
// compares its answers with node's. Each flavor is asked for by
// options.windows and resolves against a working directory the corpus sets,
// so the answers do not depend on the host -- but for a root's trailing
// separator, which node checks against the host's, and which
// TestURLFlavorFollowsWindowsPaths checks instead.
(function (url, setCwd) {
  "use strict";
  const out = {};
  const show = (f) => {
    try {
      const v = f();
      return typeof v === "string" ? JSON.stringify(v) : "URL " + v.href;
    } catch (e) {
      // The host's platform is in one message.
      return e.name + " " + e.code + ": " + String(e.message).replace(/ on [a-z0-9]+$/, " on <platform>");
    }
  };
  const ascii = [];
  for (let c = 0; c < 128; c++) ascii.push(String.fromCharCode(c));
  const others = ["\u00e9", "\u20ac", "\ud83d\ude00", "\u0080", "\u00a0", "\ud800"];

  const posixPaths = ["/a/b", "/a/b/", "a/b", "./a", "../x", "../../..", "", ".", "/a b/c%d#e?f",
    "/a/b/../c", "/a/b/.", "/a//b", "//a", "/%2F", "/:x", "C:\\a\\b", "\\\\server\\share"];
  const winPaths = ["C:\\a\\b", "C:/a/b", "c:\\a b\\", "C:\\a\\b\\", "C:", "C:a", "\\a\\b", "/a/b",
    "a\\b", "..\\x", "", ".", "\\\\server\\share\\f", "\\\\server\\share\\", "\\\\?\\UNC\\server\\share\\f",
    "\\\\server", "\\\\\\x", "\\\\?\\UNC\\x", "D:\\a b\\c%d#e?f", "C:\\a\\..\\..\\b"];

  setCwd("/work/dir");
  for (const p of posixPaths) out["posix " + JSON.stringify(p)] = show(() => url.pathToFileURL(p, { windows: false }));
  for (const ch of ascii.concat(others)) {
    out["posix char " + ch.charCodeAt(0)] = show(() => url.pathToFileURL("/a" + ch + "b", { windows: false }));
  }
  setCwd("C:\\work\\dir");
  for (const p of winPaths) out["win32 " + JSON.stringify(p)] = show(() => url.pathToFileURL(p, { windows: true }));
  for (const ch of ascii.concat(others)) {
    if (ch === "/" || ch === "\\") continue;
    out["win32 char " + ch.charCodeAt(0)] = show(() => url.pathToFileURL("C:\\a" + ch + "b", { windows: true }));
  }

  const urls = ["file:///a/b", "file:///a/b/", "file:///a%20b/c%23", "file:///C:/a/b", "file:///c:/a%5Cb",
    "file:///C:/a%5cb", "file://server/share/f", "file://SERVER/share/f", "file:///a%2Fb", "file:///a%2fb",
    "file:///C:/a%2Fb", "file:///a%5cb", "http://x/y", "file:///", "file://localhost/a", "file:///%00",
    "file:///C:", "file:///C", "file:///1:/x", "file:///%E2%82%AC", "file:///a?q#f", "file:///C|/a"];
  for (const windows of [false, true]) {
    const flavor = windows ? "win32" : "posix";
    for (const u of urls) {
      out[flavor + " to path " + u] = show(() => url.fileURLToPath(u, { windows }));
      out[flavor + " to path of URL " + u] = show(() => url.fileURLToPath(new URL(u), { windows }));
    }
    for (const v of [5, null, undefined, {}, { href: "file:///a", protocol: "file:", pathname: "/a", hostname: "" }]) {
      out[flavor + " to path of " + String(v && JSON.stringify(v))] = show(() => url.fileURLToPath(v, { windows }));
    }
  }
  for (const v of [5, null, undefined, {}, ["a"]]) {
    out["to URL of " + String(v && JSON.stringify(v))] = show(() => url.pathToFileURL(v, { windows: false }));
  }
  out.lengths = url.pathToFileURL.length + " " + url.fileURLToPath.length;
  return out;
})
