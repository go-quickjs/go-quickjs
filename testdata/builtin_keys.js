// The own keys of every built-in constructor, prototype and namespace, in
// Reflect.ownKeys's order, by a path that names each object: what
// TestBuiltinKeyOrder compares with Node's, testdata/builtin_keys.golden,
// which Node 26 makes of it, run as a script:
//
//	node -e 'console.log(require("vm").runInThisContext(require("fs").readFileSync("testdata/builtin_keys.js", "utf8")))' > testdata/builtin_keys.golden
(function () {
	var out = {}, seen = new Map();
	var names = ["Object", "Function", "Array", "Number", "Boolean", "String", "Symbol", "Date", "Promise", "RegExp",
		"Error", "AggregateError", "EvalError", "RangeError", "ReferenceError", "SyntaxError", "TypeError", "URIError",
		"SuppressedError", "JSON", "Math", "Intl", "ArrayBuffer", "SharedArrayBuffer", "Atomics", "Uint8Array", "Int8Array",
		"Uint16Array", "Int16Array", "Uint32Array", "Int32Array", "BigUint64Array", "BigInt64Array", "Uint8ClampedArray",
		"Float32Array", "Float64Array", "Float16Array", "DataView", "Map", "BigInt", "Set", "Iterator", "WeakMap", "WeakSet",
		"Proxy", "Reflect", "FinalizationRegistry", "WeakRef", "DisposableStack", "AsyncDisposableStack", "Temporal"];
	function key(k) { return typeof k === "symbol" ? k.toString() : k; }
	function visit(path, o, depth) {
		if (o === null || typeof o !== "object" && typeof o !== "function" || seen.has(o)) return;
		seen.set(o, path);
		var keys = Reflect.ownKeys(o);
		out[path] = keys.map(key);
		for (var k of keys) {
			var d = Object.getOwnPropertyDescriptor(o, k);
			if (!d || !("value" in d)) continue;
			var v = d.value;
			if (v === null || typeof v !== "object" && typeof v !== "function") continue;
			// Constructors and namespaces under a namespace (Intl, Temporal,
			// Math has none), and every constructor's prototype.
			if (k === "prototype" || k === "constructor") continue;
			var isNs = typeof v === "object" && depth === 0 && path.indexOf(".") < 0;
			var isCtor = typeof v === "function" && /^[A-Z]/.test(key(k)) && depth === 0;
			if (isNs || isCtor) walk(path + "." + key(k), v, depth + 1);
		}
	}
	function walk(path, o, depth) {
		visit(path, o, depth);
		if (typeof o === "function" && o.prototype && (typeof o.prototype === "object" || typeof o.prototype === "function")) {
			visit(path + ".prototype", o.prototype, depth + 1);
		}
	}
	for (var n of names) {
		if (n in globalThis) walk(n, globalThis[n], 0);
	}
	var g = Object.getPrototypeOf;
	var extra = {
		"%TypedArray%": g(Int8Array),
		"%IteratorPrototype%": g(g([][Symbol.iterator]())),
		"%ArrayIteratorPrototype%": g([][Symbol.iterator]()),
		"%MapIteratorPrototype%": g(new Map()[Symbol.iterator]()),
		"%SetIteratorPrototype%": g(new Set()[Symbol.iterator]()),
		"%StringIteratorPrototype%": g(""[Symbol.iterator]()),
		"%RegExpStringIteratorPrototype%": g(/a/[Symbol.matchAll]("")),
		"%GeneratorFunction%": g(function* () {}).constructor,
		"%GeneratorPrototype%": g(function* () {}).prototype,
		"%AsyncFunction%": g(async function () {}).constructor,
		"%AsyncGeneratorFunction%": g(async function* () {}).constructor,
		"%AsyncGeneratorPrototype%": g(async function* () {}).prototype,
		"%AsyncIteratorPrototype%": g(g(async function* () {}).prototype),
		"%IteratorHelperPrototype%": g(Iterator.from([]).map(function (x) { return x; })),
		"%WrapForValidIteratorPrototype%": g(Iterator.from({ next: function () {} })),
		"%SegmentsPrototype%": typeof Intl === "object" ? g(new Intl.Segmenter().segment("")) : undefined,
		"%SegmentIteratorPrototype%": typeof Intl === "object" ? g(new Intl.Segmenter().segment("")[Symbol.iterator]()) : undefined,
	};
	for (var e in extra) {
		if (extra[e] !== undefined) walk(e, extra[e], 1);
	}
	return JSON.stringify(out, null, 1);
})()
