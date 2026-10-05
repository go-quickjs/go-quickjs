// The path corpus: what each of path.posix's and path.win32's functions
// answers for the paths that are awkward -- roots, drives, UNC shares,
// devices, dots, separators of both kinds, extensions -- and the errors they
// throw. It runs unchanged in node, where pathcorpus_node.js records the
// answers in path_node.json, and in the engine, where TestPathMatchesNode
// compares its answers with node's.
(function (path, setCwd) {
  "use strict";
  const posixInputs = [
    "", "/", "//", "///", ".", "..", "./", "../", "a", "a/", "/a", "/a/", "a/b",
    "a//b", "a/./b", "a/../b", "a/b/..", "a/b/../..", "a/b/../../..", "../a",
    "../../a/b", "/..", "/../a", "//a", "///a//b", "./a/./b/.", "foo.bar",
    "/foo.bar", ".bashrc", "/.bashrc", "..ext", "file.", "file..", "file...ext",
    "a.b.c", "dir/.hidden", "dir/file.txt", "dir/file.txt/", "dir/file.txt//",
    "/dir/file.tar.gz", "a/.", "a/..", ".../x", "..a/b", "a..", "...", "....",
    "a/b.c/d", "/a/b/c/", " ", "a b/c d.e", "日本/語.txt", "😀/x.js",
  ];
  const winInputs = [
    "C:", "C:\\", "C:/", "c:\\", "C:\\a", "C:a", "C:a\\b", "C:..", "C:..\\b",
    "C:\\..", "C:\\a\\..\\..\\b", "C:/a/b", "D:\\x\\y.txt", "\\", "\\a", "\\a\\",
    "\\\\", "\\\\\\", "\\\\server", "\\\\server\\", "\\\\server\\share",
    "\\\\server\\share\\", "\\\\server\\share\\a\\b", "\\\\server\\share\\a\\..\\..",
    "//server/share/a", "\\\\?\\C:\\a", "\\\\?\\UNC\\server\\share",
    "\\\\.\\pipe\\x", "\\\\.\\PHYSICALDRIVE0", "a\\b/c", "a/b\\c", "a:b", "a:",
    "file:stream", "foo\\bar.baz", "C:foo.bar", "C:\\foo.bar\\", "CON", "C:\\CON",
    "a\\..\\..\\b", "\\..\\a", "c:/ignore", "\\\\a\\b\\c\\..\\d", "a\\b\\",
    "C:\\a\\b\\c.d\\", "1:\\x", "C::x",
  ];
  const pairInputs = [
    "", ".", "..", "a", "a/b", "/a", "/a/b", "../c", "a/../b", "//x", "/a/b/c/d",
    "/a/b/x", "C:\\a", "C:", "C:b", "\\a\\b", "\\\\s\\sh\\x", "D:\\y", "a\\b",
    "c:\\A\\b", "\\\\s\\sh",
  ];
  const errorCases = [
    ["join", [1]], ["join", ["a", null]], ["basename", [null]], ["basename", ["a", 1]],
    ["dirname", [{}]], ["extname", [[]]], ["parse", [1n]], ["normalize", [function f() {}]],
    ["resolve", [undefined]], ["resolve", ["a", 2]], ["relative", [1, "a"]], ["relative", ["a", true]],
    ["format", [null]], ["format", ["x"]], ["isAbsolute", [Symbol("s")]],
    ["join", ["a long string that is longer than twenty-eight characters", 3]],
    ["basename", ["a", "a long string that is longer than twenty-eight characters".length]],
  ];
  const formatCases = [
    { root: "/", dir: "/x", base: "y" }, { name: "n", ext: "e" }, { name: "n", ext: ".e" },
    { dir: "d" }, { root: "C:\\", name: "x" }, {}, { root: "/", base: "b" },
    { dir: "C:\\a", name: "n", ext: "txt" }, { base: "b", name: "ignored", ext: ".x" },
  ];
  const out = {};
  const show = (v) => typeof v === "string" ? v : JSON.stringify(v);
  const record = (key, f) => {
    try {
      out[key] = show(f());
    } catch (e) {
      out[key] = "throws " + e.name + " " + e.code + ": " + e.message;
    }
  };
  for (const [flavor, inputs, cwd] of [
    ["posix", [...posixInputs, ...winInputs], "/home/user/proj"],
    ["win32", [...posixInputs, ...winInputs], "C:\\Users\\user\\proj"],
  ]) {
    setCwd(cwd);
    const p = path[flavor];
    record(flavor + " sep", () => p.sep + " " + p.delimiter);
    for (const a of inputs) {
      const k = flavor + " " + JSON.stringify(a) + " ";
      record(k + "normalize", () => p.normalize(a));
      record(k + "isAbsolute", () => p.isAbsolute(a));
      record(k + "dirname", () => p.dirname(a));
      record(k + "basename", () => p.basename(a));
      record(k + "basename .txt", () => p.basename(a, ".txt"));
      record(k + "basename ext", () => p.basename(a, p.extname(a)));
      record(k + "basename itself", () => p.basename(a, a));
      record(k + "basename b", () => p.basename(a, "b"));
      record(k + "extname", () => p.extname(a));
      record(k + "parse", () => p.parse(a));
      record(k + "format parse", () => p.format(p.parse(a)));
      record(k + "toNamespacedPath", () => p.toNamespacedPath(a));
      record(k + "resolve", () => p.resolve(a));
    }
    for (const a of pairInputs) {
      for (const b of pairInputs) {
        const k = flavor + " " + JSON.stringify(a) + " " + JSON.stringify(b) + " ";
        record(k + "join", () => p.join(a, b));
        record(k + "join 3", () => p.join(a, b, "c"));
        record(k + "resolve", () => p.resolve(a, b));
        record(k + "relative", () => p.relative(a, b));
      }
    }
    record(flavor + " join none", () => p.join());
    record(flavor + " resolve none", () => p.resolve());
    for (const [i, o] of formatCases.entries()) {
      record(flavor + " format " + i, () => p.format(o));
    }
    for (const [name, args] of errorCases) {
      record(flavor + " error " + name + " " + args.length + " " + typeof args[0] + " " + typeof args[1], () => p[name](...args));
    }
  }
  return out;
})
