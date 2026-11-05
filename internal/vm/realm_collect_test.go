package vm

import (
	"runtime"
	"strconv"
	"testing"
	"time"
	"weak"
)

// realmsAlive collects until no more of refs go, a turn of the runtime's
// between collections, as a host that runs on has, and says how many are
// left: what is queued for a finalization callback holds a realm until the
// turn after the cleanup has run, on a goroutine of its own.
func realmsAlive(r *Runtime, refs []weak.Pointer[Realm]) int {
	left := len(refs)
	for range 8 {
		runtime.GC()
		runtime.GC()
		time.Sleep(time.Millisecond)
		r.DrainJobs()
		left = 0
		for _, w := range refs {
			if w.Value() != nil {
				left++
			}
		}
		if left == 0 {
			break
		}
	}
	return left
}

// realmPrograms are what a realm a host has dropped may have run, each with
// what a cache or the runtime had kept of a realm.
var realmPrograms = []struct{ name, src string }{
	{"loop", `function f(n) { var o; for (var i = 0; i < n; i++) o = {a: i}; return o; } f(10);`},
	{"classes", `{ class A { #p = 1; static s = 2; m() { return this.#p; } get g() { return 1; } }
		class B extends A { constructor() { super(); this.x = 1; } m() { return super.m() + 1; } }
		var o; for (var i = 0; i < 50; i++) o = new B(); o.m() + o.g; }`},
	{"closures", `function mk() { var c = 0; return function () { return ++c; }; }
		var f = mk(); for (var i = 0; i < 100; i++) f(); var a = () => this; a();`},
	{"generators", `function* g() { yield 1; yield* [2, 3]; return 4; }
		var s = 0; for (var x of g()) s += x; var it = g(); it.next();
		async function* ag() { yield 1; } ag().next(); s;`},
	// The realm's RegExp's layouts (matchLayout) and the legacy statics.
	{"regexps", `var s = "abc abc".replace(/b/g, "x"); /a(?<n>b)/.exec("ab").groups.n; "a,b".split(/,/);
		RegExp.$1; "x".match(/x/); [..."aXbX".matchAll(/X/g)]; s;`},
	{"maps", `var m = new Map; var k = {}; m.set(k, 1); var st = new Set([1, 2]);
		for (var v of st) v; for (var e of m) e;`},
	// A value that reaches its key, and so does not keep it.
	{"weakmaps", `var k = {}; var w = new WeakMap; w.set(k, {k, w}); var ws = new WeakSet; ws.add(k);
		var r = new WeakRef(k); r.deref();`},
	// A cleanup held its registry, whose callback reaches the realm.
	{"finalization", `var k = {}, h = {}; var reg = new FinalizationRegistry(function () {}); reg.register(k, h, k);
		new FinalizationRegistry(function () {}).register({}, h);`},
	{"promises", `var out = 0; Promise.resolve(1).then(v => { out = v; return new Promise(r => r(2)); }).then(v => { out = v; });
		async function f() { await 1; out = 3; } f(); new Promise(function () {}); Promise.reject(new Error("x")); out;`},
	{"proxies", `var p = new Proxy({}, { get: function (t, k) { return k; } }); p.x;
		var b = (function () {}).bind(null); b(); Reflect.ownKeys(p);`},
	// The shared cache, the cache of additions and the in operator's.
	{"megamorphic", `function rd(o) { return o.x; } var os = [];
		for (var i = 0; i < 40; i++) { var o = {}; o["k" + i] = 1; o.x = i; os.push(o); }
		var s = 0; for (var j = 0; j < 400; j++) s += rd(os[j % 40]);
		function has(o) { return "x" in o; } for (var j = 0; j < 400; j++) has(os[j % 40]);
		function st(o) { o.y = 1; } for (var j = 0; j < 400; j++) st(Object.create(os[j % 40])); s;`},
	{"dictionaries", `var c = {}; for (var i = 0; i < 2000; i++) c["p" + i] = i; for (var k in c) k;
		JSON.stringify({a: [1]}); JSON.parse('{"a":[1,{"b":2}]}');`},
	{"prototypes", `function P() {} P.prototype.m = function () { return 1; };
		function Q() {} Q.prototype = Object.create(P.prototype);
		var q; for (var i = 0; i < 100; i++) { q = new Q(); q.m(); }
		Object.setPrototypeOf(q, null); var op = {__proto__: {y: 1}}; op.y;`},
	{"templates", "function tag(s) { return s; } function t() { return tag`a${1}b`; } t(); t();"},
	{"errors", `try { null.x; } catch (e) { e.stack; }
		function thrower() { throw new RangeError("x"); } try { thrower(); } catch (e) { String(e); }`},
	// The iterator results' layout, and the primitives' methods (primProps).
	{"primitives", `var s = Symbol.for("realm"); var o = {[s]: 1}; [][Symbol.iterator]().next(); ""[Symbol.iterator];
		"abc".toUpperCase().trim().at(1); (1).toFixed(2); true.toString(); 10n ** 20n;`},
	{"arguments", `function a() { return arguments; } a(1, 2); (function () { "use strict"; return arguments; })(1);
		function b(x) { arguments[0] = 2; return x; } b(1);`},
	{"typed", `var u = new Uint8Array(100); for (var i = 0; i < 100; i++) u[i] = i; var s = 0;
		u.forEach(function (x) { s += x; }); new DataView(new ArrayBuffer(8)).getInt8(0);`},
	// The math functions the engine knew by name, the runtime's last's.
	{"math", `Math.pow(2, 3); Math.atan2(1, 2); Math.hypot(1, 2); Math.random(); new Date(0).toISOString(); Date.now();`},
	{"lazy", `new Intl.NumberFormat("en").format(1); "a".localeCompare("b"); Temporal.PlainDate.from("2020-01-01").toString();`},
	// What a join, a call in tail position and a derived constructor last
	// left in the runtime's buffers.
	{"join", `[[1, {}], [new Array(2)]].join(); [{toLocaleString() { return "x"; }}].toLocaleString();`},
	{"tail", `(function () { "use strict"; function g(o, p) { return p; } function f(o) { return g(o, {o}); }
		for (var i = 0; i < 100; i++) f({}); function r(n, o) { return n ? r(n - 1, o) : 0; } r(20000, {}); })();`},
	{"derived", `{ class A { constructor() { this.a = 1; } } class B extends A { constructor() { super(); this.b = () => this; } }
		class C extends A { constructor() { super(); } } for (var i = 0; i < 100; i++) { new B(); new C(); } }`},
	{"recursion", `function d(n) { return n ? d(n - 1) + 1 : 0; } d(5000); try { (function f() { f(); })(); } catch (e) {}`},
}

// A realm a host has dropped is collected, whatever it ran, the bytecode it
// ran living on. Twenty of twenty had stayed for as long as the runtime did:
// the runtime had held each realm's built-ins (leafFns), the last's Math
// functions, what its caches last found, and a cleanup the realm of a
// registration's callback. Each program runs twice in each realm, for
// what a second run finds the caches left it.
func TestDroppedRealmsAreCollected(t *testing.T) {
	for _, p := range realmPrograms {
		t.Run(p.name, func(t *testing.T) {
			r := New(Config{})
			defer r.Close()
			fn := compileForTest(t, p.src)
			var refs []weak.Pointer[Realm]
			for range 20 {
				re := r.NewRealm()
				refs = append(refs, weak.Make(re))
				for range 2 {
					if _, err := r.RunIn(re, fn); err != nil {
						t.Fatal(err)
					}
					if err := r.DrainJobs(); err != nil {
						t.Fatal(err)
					}
				}
			}
			if n := realmsAlive(r, refs); n != 0 {
				t.Errorf("%d of %d realms alive", n, len(refs))
			}
			runtime.KeepAlive(fn)
		})
	}
}

// A realm that has run no script goes as one that has: the first thing that
// was found to hold one was the runtime's list of every realm's leaves.
func TestDroppedRealmNeverRun(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	var refs []weak.Pointer[Realm]
	for range 20 {
		refs = append(refs, weak.Make(r.NewRealm()))
	}
	if n := realmsAlive(r, refs); n != 0 {
		t.Errorf("%d of %d realms alive", n, len(refs))
	}
}

// realmKept is a realm whose program has run, and what the program left: a
// function, an object made on a prototype of its own, an array.
func realmKept(t *testing.T, r *Runtime) (weak.Pointer[Realm], Value, Value, Value) {
	t.Helper()
	re := r.NewRealm()
	v, err := r.RunIn(re, compileForTest(t, `
		var n = 40;
		function inc() { return ++n + [1, 2].map(function (x) { return x * 2; }).length; }
		var p = {hi() { return "hi " + n + " " + /(l+)/.exec("hello")[1] + " " + new Map([[1, 2]]).get(1); }};
		[inc, Object.create(p), [1, 2, 3]];`))
	if err != nil {
		t.Fatal(err)
	}
	vals := v.Object().elemsOf()
	return weak.Make(re), vals[0], vals[1], vals[2]
}

// A realm stays for as long as something reachable needs it, a function or
// an object of it the host keeps, and works through collections, the realms
// around it going, as it did: the function finds its globals and its
// built-ins, the object its prototype's methods, and either makes what its
// realm makes.
func TestRealmKeptByWhatItMade(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	w, inc, obj, arr := realmKept(t, r)
	drop := compileForTest(t, `var o = {a: 1}; [1].map(x => x);`)
	var refs []weak.Pointer[Realm]
	for i := range 10 {
		re := r.NewRealm()
		refs = append(refs, weak.Make(re))
		if _, err := r.RunIn(re, drop); err != nil {
			t.Fatal(err)
		}
		if n := realmsAlive(r, refs); n != 0 {
			t.Fatalf("round %d: %d of %d others alive", i, n, len(refs))
		}
		if w.Value() == nil {
			t.Fatalf("round %d: the realm of what the host holds was collected", i)
		}
		v, err := r.call(inc, Undefined, nil)
		if err != nil || v.Number() != float64(43+i) {
			t.Fatalf("round %d: inc() = %v, %v", i, v, err)
		}
		hi, err := r.getValueProp(obj, r.atoms.intern("hi"))
		if err != nil {
			t.Fatal(err)
		}
		v, err = r.call(hi, obj, nil)
		if want := "hi " + strconv.Itoa(41+i) + " ll 2"; err != nil || v.String().Go() != want {
			t.Fatalf("round %d: hi() = %v, %v, want %q", i, v, err, want)
		}
		m, err := r.getValueProp(arr, r.atoms.intern("map"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.call(m, arr, []Value{inc}); err != nil {
			t.Fatal(err)
		}
		if _, err = r.call(inc, Undefined, nil); err != nil {
			t.Fatal(err)
		}
		// Three calls of map's callback and this one.
		if _, err := r.RunIn(w.Value(), compileForTest(t, `n -= 4`)); err != nil {
			t.Fatal(err)
		}
	}
	// Let go of all of it, and it goes.
	inc, obj, arr = Undefined, Undefined, Undefined
	if n := realmsAlive(r, []weak.Pointer[Realm]{w}); n != 0 {
		t.Error("the realm stayed once nothing held what it made")
	}
}

// A value one realm's global holds keeps the realm that made it alive for
// as long as that realm lives, and no longer: a function of a dropped realm
// goes on working for the realm given it, and goes with it.
func TestRealmKeptByAnotherRealm(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	w, inc, _, _ := realmKept(t, r)
	holder := r.NewRealm()
	r.InRealm(holder, func() { r.global.setOwnRaw(r.atoms.intern("kept"), inc, propDefault) })
	inc = Undefined
	if n := realmsAlive(r, []weak.Pointer[Realm]{w}); n != 1 {
		t.Fatal("the realm went with another's global holding its function")
	}
	v, err := r.RunIn(holder, compileForTest(t, `kept()`))
	if err != nil || v.Number() != 43 {
		t.Fatalf("kept() = %v, %v", v, err)
	}
	if _, err := r.RunIn(holder, compileForTest(t, `delete globalThis.kept`)); err != nil {
		t.Fatal(err)
	}
	if n := realmsAlive(r, []weak.Pointer[Realm]{w}); n != 0 {
		t.Error("the realm stayed once the other realm let go of its function")
	}
	runtime.KeepAlive(holder)
}

// What the engine knows of a realm's built-ins and of the layouts of its
// objects is the realm's own: a realm made later takes over none of the
// first's. Math's functions had been the last realm's, for the JIT to know,
// and the runtime's list of leaves had had every realm's.
func TestRealmIntrinsicsAreItsOwn(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	math := func(name string) *Object {
		m := r.global.getOwn(r.atoms.intern("Math")).value().Object()
		return m.getOwn(r.atoms.intern(name)).value().Object()
	}
	pow, leaves := r.powFn, len(r.leafFns)
	if pow == nil || pow != math("pow") || r.atan2Fn != math("atan2") || r.hypotFn != math("hypot") || r.randomFn != math("random") {
		t.Fatal("the realm's Math functions are not its own")
	}
	re := r.NewRealm()
	if r.powFn != pow || len(r.leafFns) != leaves {
		t.Fatal("a realm's making changed what the runtime's first knows of its built-ins")
	}
	r.InRealm(re, func() {
		if r.powFn == pow || r.powFn != math("pow") || len(r.leafFns) != leaves || r.leafFns[0] == nil {
			t.Error("the new realm does not know its own built-ins")
		}
	})
}

// Each realm has the caches of what its own code found: the reads that gave
// up, the additions, the in operator's answers, its objects' layouts, the
// primitives' methods. Code of one had replaced another's entries, and a
// layout it could not use had kept another's: a regular expression's result
// in any realm but the first's was made the long way.
func TestRealmCachesAreItsOwn(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	fn := compileForTest(t, `
		function rd(o) { return o.x; } function has(o) { return "x" in o; } var os = [];
		for (var i = 0; i < 40; i++) { var o = {}; o["k" + i] = 1; o.x = i; os.push(o); }
		var s = 0;
		for (var j = 0; j < 400; j++) { s += rd(os[j % 40]); has(os[j % 40]); }
		function A() { this.a = 1; this.b = 2 } function B() { A.call(this); this.c = 3 }
		B.prototype = Object.create(A.prototype);
		for (var i = 0; i < 100; i++) { var q = new B; q["y" + (i % 40)] = 1; q.z = 2; }
		(function () { return arguments; })(1); /a/.exec("a"); [][Symbol.iterator]().next(); "x".at(0);
		s;`)
	if r.has != nil || r.deep != nil || r.matchLayout != nil || r.iterResultShape != nil {
		t.Fatal("a runtime that has run nothing has caches")
	}
	re := r.NewRealm()
	if _, err := r.RunIn(re, fn); err != nil {
		t.Fatal(err)
	}
	for _, o := range []struct {
		name string
		left bool
	}{
		{"has", r.has != nil}, {"deep", r.deep != nil}, {"matchLayout", r.matchLayout != nil},
		{"iterResultShape", r.iterResultShape != nil},
		{"argsLayouts", r.argsLayouts[1] != nil}, {"primProps", r.primProps != [64]primEntry{}},
	} {
		if o.left {
			t.Errorf("%s of a realm's code in the first realm", o.name)
		}
	}
	if re.has == nil || re.deep == nil || re.matchLayout == nil || re.iterResultShape == nil || re.argsLayouts[1] == nil {
		t.Errorf("the realm has no caches of its code: %v %v %v %v %v", re.has != nil, re.deep != nil, re.matchLayout != nil, re.iterResultShape != nil, re.argsLayouts[1] != nil)
	}
	if re.matchLayout.shape.proto == r.proto.array {
		t.Error("the realm's match layout is the first's")
	}
	// A read whose site has given up asks the realm's shared cache.
	r.InRealm(re, func() {
		o := newObject(r.proto.object, ClassObject)
		o.setOwnRaw(r.atoms.intern("x"), Int(1), propDefault)
		if v, ok := r.sharedProp(o, r.atoms.intern("x")); !ok || v.Number() != 1 {
			t.Errorf("sharedProp: %v, %v", v, ok)
		}
	})
	if r.shared != nil || re.shared == nil {
		t.Errorf("shared cache in the first realm: %v, in the realm: %v", r.shared != nil, re.shared != nil)
	}
}

// A registry a script has let go of goes on working, while its realm lives,
// for as long as it has cells: a registration's cleanup holds the cell only
// weakly, so that a target of a realm no longer keeps the realm (and what
// held the registry had been the cleanup). Once its cells are gone so is it.
func TestFinalizationRegistryKeptByItsRealm(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		var seen = [];
		(function () {
			new FinalizationRegistry(h => seen.push(h)).register({}, "held");
		})();`)); err != nil {
		t.Fatal(err)
	}
	read := compileForTest(t, `seen.length`)
	v := Undefined
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(time.Millisecond)
		var err error
		if v, err = r.Run(read); err != nil {
			t.Fatal(err)
		}
		if err := r.DrainJobs(); err != nil {
			t.Fatal(err)
		}
		if v.Number() != 0 {
			break
		}
	}
	if v.Number() != 1 {
		t.Fatalf("the callback ran %v times", v)
	}
	if len(r.finalizing) != 1 {
		t.Fatalf("%d registries kept", len(r.finalizing))
	}
	// The set is looked through as it doubles, not at each registration.
	if _, err := r.Run(compileForTest(t, `
		for (var i = 0; i < 100; i++) {
			var t = {};
			var fr = new FinalizationRegistry(h => h);
			fr.register(t, "held", t);
			fr.unregister(t);
		}`)); err != nil {
		t.Fatal(err)
	}
	if n := len(r.finalizing); n > 32 {
		t.Errorf("%d registries of no cells kept", n)
	}
}

// Realms made and dropped give the shape tree back what they took: the
// shapes on their prototypes, and the roots of their literals, which code
// counts as it runs, had filled it after some tens of thousands of runs of
// a program, a realm for each, and every layout after that was of its own.
func TestShapeBudgetRealms(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	fn := compileForTest(t, `
		var o = {a: 1, b: 2}, e = {};
		function f() { return {c: 1, d: 2}; } f();
		class K { constructor() { this.x = 1; } } new K();
		var n = new (function N() { this.q = 1; })();
		[1, 2].map(x => ({v: x}));
		Promise.resolve().then(() => ({w: 1}));`)
	const realms = 500
	var refs []weak.Pointer[Realm]
	for range realms {
		re := r.NewRealm()
		refs = append(refs, weak.Make(re))
		if _, err := r.RunIn(re, fn); err != nil {
			t.Fatal(err)
		}
		if err := r.DrainJobs(); err != nil {
			t.Fatal(err)
		}
	}
	if n := realmsAlive(r, refs); n != 0 {
		t.Fatalf("%d realms alive", n)
	}
	// What is left is the tree's own roots and a chunk of budgets' realms,
	// which the tree holds until the next (liveShapes).
	if n := liveShapes(r); n > 2*budgetSlots*8 {
		t.Errorf("%d shapes left of %d realms'", n, realms)
	}
}
