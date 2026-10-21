package vm

import "testing"

// privateMemberScript reads and writes private members -- fields, methods,
// accessors, static ones -- in loops, three rounds of them, so that the
// sites' caches are filled before they are tested: through this and
// through other objects; with brand checks of objects without the member
// and of primitives; across two evaluations of one class, whose names
// differ though their code and sites are the same; written where a method
// or a getter alone is; a base's members on its subclasses' objects, of
// shapes of their own; fields a constructor's returned object is given --
// an array, a function, a proxy, a typed array, a map, one in dictionary
// mode, one with an index of its properties, one frozen or sealed after --
// and properties added and deleted around them; compound, update,
// destructuring, logical and optional-chaining forms; #x in o.
const privateMemberScript = `(function () {
	var out = [];
	function line(name, v) { out.push(name + ": " + v); }
	function attempt(name, f) {
		try { line(name, String(f())); } catch (e) { line(name, "threw " + (e && e.constructor && e.constructor.name)); }
	}
	// Each check runs several rounds, so that its functions run as trees
	// and their sites' caches are filled before they are tested.
	for (var round = 0; round < 3; round++) {
		// Fields read and written, in a loop and once, through this and
		// through another object of the class.
		class Counter {
			#n = 0;
			#m = 10;
			add(k) { for (var j = 0; j < k; j++) this.#n = (this.#n + j) | 0; return this.#n; }
			addTo(o, k) { for (var j = 0; j < k; j++) o.#m += j; return o.#m; }
			get n() { return this.#n; }
			static read(o) { return o.#n; }
			static write(o, v) { o.#n = v; return o.#n; }
		}
		var a = new Counter(), b = new Counter();
		var r1 = a.add(100), r2 = b.addTo(a, 10), r3 = Counter.read(b), r4 = Counter.write(b, 7);
		line("fields " + round, [r1, r2, r3, r4, a.n, b.n].join(" "));
		// The brand check: an object without the member, or a primitive, is
		// a TypeError, at a site that has met objects with it.
		var brand = [];
		[{}, Object.create(a), new Proxy(a, {}), 1, "s", null, undefined, function () {}].forEach(function (x) {
			try { brand.push(Counter.read(x)); } catch (e) { brand.push(e.constructor.name); }
			try { brand.push(Counter.write(x, 1)); } catch (e) { brand.push(e.constructor.name); }
		});
		line("brand " + round, brand.join(" "));
		// A class evaluated again has names of its own: an object of one
		// is not an object of the other, though their code, and their
		// sites, are the same.
		function make(start) {
			return class {
				#v = start;
				get() { return this.#v; }
				set(v) { this.#v = v; return this; }
				static get(o) { return o.#v; }
				static set(o, v) { o.#v = v; }
			};
		}
		var K1 = make(1), K2 = make(2), k1 = new K1(), k2 = new K2(), cross = [];
		for (var j = 0; j < 4; j++) {
			K1.set(k1, 1); K2.set(k2, 2);
			cross.push(K1.get(k1), K2.get(k2), k1.get(), k2.get());
			try { cross.push(K1.get(k2)); } catch (e) { cross.push(e.constructor.name); }
			try { cross.push(K2.get(k1)); } catch (e) { cross.push(e.constructor.name); }
			try { K1.set(k2, 8); cross.push("wrote"); } catch (e) { cross.push(e.constructor.name); }
			try { K2.set(k1, 9); cross.push("wrote"); } catch (e) { cross.push(e.constructor.name); }
			try { cross.push(K1.prototype.get.call(k2)); } catch (e) { cross.push(e.constructor.name); }
		}
		k1.set(5); k2.set(6);
		try { K1.prototype.set.call(k2, 7); cross.push("wrote"); } catch (e) { cross.push(e.constructor.name); }
		cross.push(K1.get(k1), K2.get(k2));
		line("evaluations " + round, cross.join(" "));
		// Methods: read, called, compared, not written; accessors: a getter
		// and a setter called at every access, and a member without one.
		var log = [];
		class Members {
			#x = 1;
			#m() { return "m" + this.#x; }
			get #acc() { log.push("get"); return this.#x * 100; }
			set #acc(v) { log.push("set" + v); this.#x = v; }
			get #only() { return "only"; }
			set #wo(v) { log.push("wo" + v); }
			static #sm() { return "static method"; }
			static #sf = "static field";
			callM() { var s = ""; for (var j = 0; j < 3; j++) s += this.#m(); return s; }
			same(o) { return this.#m === o.#m; }
			writeM() { this.#m = 1; }
			readAcc() { var s = 0; for (var j = 0; j < 3; j++) s += this.#acc; return s; }
			writeAcc(v) { for (var j = 0; j < 2; j++) this.#acc = v + j; return this.#x; }
			writeOnly() { this.#only = 1; }
			readWo() { return this.#wo; }
			writeWo() { this.#wo = 3; }
			static statics() { return Members.#sm() + " " + Members.#sf + " " + (#sf in Members) + " " + (#sm in Members); }
			static setStatic(v) { Members.#sf = v; return Members.#sf; }
			static readStatic(o) { return o.#sf; }
			static has(o) { return [#x in o, #m in o, #acc in o].join(); }
		}
		var m1 = new Members(), m2 = new Members(), mem = [];
		mem.push(m1.callM(), m1.same(m2), typeof m1.readAcc());
		mem.push(m1.readAcc(), m1.writeAcc(4), m1.readAcc());
		try { m1.writeM(); mem.push("wrote method"); } catch (e) { mem.push(e.constructor.name); }
		try { m1.writeOnly(); mem.push("wrote getter only"); } catch (e) { mem.push(e.constructor.name); }
		try { mem.push(m1.readWo()); } catch (e) { mem.push(e.constructor.name); }
		m1.writeWo();
		mem.push(Members.statics(), Members.setStatic("changed"), Members.readStatic(Members));
		try { mem.push(Members.readStatic(m1)); } catch (e) { mem.push(e.constructor.name); }
		try { mem.push(Members.readStatic(class extends Members {})); } catch (e) { mem.push(e.constructor.name); }
		mem.push(Members.has(m1), Members.has({}), Members.has(Object.create(m1)));
		try { Members.has(1); } catch (e) { mem.push(e.constructor.name); }
		line("members " + round, mem.join(" | ") + " | " + log.join(","));
		// Inheritance: a base's members on a subclass's objects, of shapes
		// of their own, at one site; a subclass's own of the same name.
		class Base {
			#b = "base";
			static read(o) { return o.#b; }
			static write(o, v) { o.#b = v; }
			readB() { return this.#b; }
		}
		class Sub1 extends Base { x = 1; }
		class Sub2 extends Base { y = 2; z = 3; }
		class Sub3 extends Sub2 { #b = "sub3"; readSub() { return this.#b; } }
		var objs = [new Base(), new Sub1(), new Sub2(), new Sub3(), new Sub1(), new Base()];
		var inh = [];
		for (var j = 0; j < 3; j++) {
			objs.forEach(function (o, i) { Base.write(o, "w" + i + j); inh.push(Base.read(o), o.readB()); });
		}
		inh.push(objs[3].readSub());
		try { inh.push(Sub3.prototype.readSub.call(objs[2])); } catch (e) { inh.push(e.constructor.name); }
		line("inheritance " + round, inh.join(" "));
		// The constructor of a base returning another object: the
		// subclass's fields go on it -- a plain object, an array, a
		// function, a proxy, one with many properties, one that has had
		// properties deleted, a typed array, a map.
		class Wrap { constructor(o) { return o; } }
		class Stamp extends Wrap {
			#s = 0;
			static bump(o) { for (var j = 0; j < 3; j++) o.#s = o.#s + 1; return o.#s; }
			static has(o) { return #s in o; }
		}
		var big = {};
		for (var j = 0; j < 40; j++) big["p" + j] = j;
		var dict = { a: 1, b: 2, c: 3 };
		delete dict.b;
		var frozen = { f: 1 }, sealed = { s: 1 };
		var targets = [{}, [1, 2], function () {}, new Proxy({}, {}), big, dict, new Int8Array(2), new Map()];
		var wrap = [];
		targets.forEach(function (o) {
			try { new Stamp(o); wrap.push(Stamp.bump(o) + ":" + Stamp.has(o)); } catch (e) { wrap.push(e.constructor.name); }
		});
		// A field added before the object is frozen or sealed is written
		// still.
		new Stamp(frozen);
		Object.freeze(frozen);
		new Stamp(sealed);
		Object.seal(sealed);
		wrap.push("frozen " + Stamp.bump(frozen) + " sealed " + Stamp.bump(sealed) + " " + Object.isFrozen(frozen));
		try { new Stamp(targets[0]); wrap.push("twice"); } catch (e) { wrap.push("twice " + e.constructor.name); }
		// Properties added and deleted around the member after it is
		// cached: the member is found where it now is.
		var o = {};
		new Stamp(o);
		Stamp.bump(o);
		o.after = 1;
		wrap.push(Stamp.bump(o));
		delete o.after;
		wrap.push(Stamp.bump(o));
		for (var j = 0; j < 30; j++) o["q" + j] = j;
		wrap.push(Stamp.bump(o));
		for (var j = 0; j < 30; j += 2) delete o["q" + j];
		wrap.push(Stamp.bump(o), Object.keys(o).length, JSON.stringify(Object.getOwnPropertyNames(o).slice(0, 3)));
		line("return override " + round, wrap.join(" "));
		// A compound assignment, an update, a destructuring target, a
		// logical assignment and optional chaining of a private member.
		class Ops {
			#v = 1;
			#o = null;
			run() {
				var s = [];
				this.#v += 2; s.push(this.#v);
				this.#v++; s.push(this.#v);
				--this.#v; s.push(this.#v);
				[this.#v] = [40]; s.push(this.#v);
				({ x: this.#v } = { x: 41 }); s.push(this.#v);
				this.#o ??= { k: 1 }; s.push(this.#o.k);
				this.#v ||= 99; s.push(this.#v);
				this.#v &&= 0; s.push(this.#v);
				var n = null;
				s.push(n?.#v, this?.#v);
				return s.join(",");
			}
		}
		var ops = new Ops().run();
		line("operators " + round, ops);
		// Stack trace position of a failed read inside a loop.
		class Thrower {
			#t = 1;
			static read(o) { var s = 0; for (var j = 0; j < 3; j++) s += o.#t; return s; }
		}
		var msgs = [];
		msgs.push(Thrower.read(new Thrower()));
		try { Thrower.read({}); } catch (e) { msgs.push(e instanceof TypeError); }
		line("throw " + round, msgs.join(" "));
	}
	return out.join("\n");
})()`

// privateMemberWant is Node v26.10.0's answer, the script run by
// vm.runInThisContext.
const privateMemberWant = `fields 0: 4950 55 0 7 4950 7
brand 0: TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError
evaluations 0: 1 2 1 2 TypeError TypeError TypeError TypeError TypeError 1 2 1 2 TypeError TypeError TypeError TypeError TypeError 1 2 1 2 TypeError TypeError TypeError TypeError TypeError 1 2 1 2 TypeError TypeError TypeError TypeError TypeError TypeError 5 6
members 0: m1m1m1 | true | number | 300 | 5 | 1500 | TypeError | TypeError | TypeError | static method static field true true | changed | changed | TypeError | TypeError | true,true,true | false,false,false | false,false,false | TypeError | get,get,get,get,get,get,set4,set5,get,get,get,wo3
inheritance 0: w00 w00 w10 w10 w20 w20 w30 w30 w40 w40 w50 w50 w01 w01 w11 w11 w21 w21 w31 w31 w41 w41 w51 w51 w02 w02 w12 w12 w22 w22 w32 w32 w42 w42 w52 w52 sub3 TypeError
return override 0: 3:true 3:true 3:true 3:true 3:true 3:true 3:true 3:true frozen 3 sealed 3 true twice TypeError 6 9 12 15 15 ["q1","q3","q5"]
operators 0: 3,4,3,40,41,1,41,0,,0
throw 0: 3 true
fields 1: 4950 55 0 7 4950 7
brand 1: TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError
evaluations 1: 1 2 1 2 TypeError TypeError TypeError TypeError TypeError 1 2 1 2 TypeError TypeError TypeError TypeError TypeError 1 2 1 2 TypeError TypeError TypeError TypeError TypeError 1 2 1 2 TypeError TypeError TypeError TypeError TypeError TypeError 5 6
members 1: m1m1m1 | true | number | 300 | 5 | 1500 | TypeError | TypeError | TypeError | static method static field true true | changed | changed | TypeError | TypeError | true,true,true | false,false,false | false,false,false | TypeError | get,get,get,get,get,get,set4,set5,get,get,get,wo3
inheritance 1: w00 w00 w10 w10 w20 w20 w30 w30 w40 w40 w50 w50 w01 w01 w11 w11 w21 w21 w31 w31 w41 w41 w51 w51 w02 w02 w12 w12 w22 w22 w32 w32 w42 w42 w52 w52 sub3 TypeError
return override 1: 3:true 3:true 3:true 3:true 3:true 3:true 3:true 3:true frozen 3 sealed 3 true twice TypeError 6 9 12 15 15 ["q1","q3","q5"]
operators 1: 3,4,3,40,41,1,41,0,,0
throw 1: 3 true
fields 2: 4950 55 0 7 4950 7
brand 2: TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError TypeError
evaluations 2: 1 2 1 2 TypeError TypeError TypeError TypeError TypeError 1 2 1 2 TypeError TypeError TypeError TypeError TypeError 1 2 1 2 TypeError TypeError TypeError TypeError TypeError 1 2 1 2 TypeError TypeError TypeError TypeError TypeError TypeError 5 6
members 2: m1m1m1 | true | number | 300 | 5 | 1500 | TypeError | TypeError | TypeError | static method static field true true | changed | changed | TypeError | TypeError | true,true,true | false,false,false | false,false,false | TypeError | get,get,get,get,get,get,set4,set5,get,get,get,wo3
inheritance 2: w00 w00 w10 w10 w20 w20 w30 w30 w40 w40 w50 w50 w01 w01 w11 w11 w21 w21 w31 w31 w41 w41 w51 w51 w02 w02 w12 w12 w22 w22 w32 w32 w42 w42 w52 w52 sub3 TypeError
return override 2: 3:true 3:true 3:true 3:true 3:true 3:true 3:true 3:true frozen 3 sealed 3 true twice TypeError 6 9 12 15 15 ["q1","q3","q5"]
operators 2: 3,4,3,40,41,1,41,0,,0
throw 2: 3 true`

// Reads and writes of private members answer what Node's do, in a runtime
// made twice, the tree tier on and off.
func TestPrivateMembersAgreeWithNode(t *testing.T) {
	defer SetTreeTier(true)
	for round := range 4 {
		SetTreeTier(round < 2)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, privateMemberScript))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != privateMemberWant {
			diffLinesForTest(t, round, got, privateMemberWant)
		}
		r.Close()
	}
}

// The reads and writes of private fields, in a function the tree tier
// runs, are answered by their sites' caches: the caches past the
// function's PropSites, one for each in the order of the code, remember
// the object's shape and where in it each member is; a getter's member,
// which is called, is remembered by none.
func TestPrivateSitesCached(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	_, err := r.Run(compileForTest(t, `
		class C {
			#n = 0;
			#m = 5;
			get #g() { return 1; }
			add(k) { for (var j = 0; j < k; j++) this.#n = (this.#n + j) | 0; return this.#m + this.#g; }
		}
		var c = new C();
		for (var i = 0; i < 3; i++) c.add(10);
		var add = C.prototype.add;`))
	if err != nil {
		t.Fatal(err)
	}
	c := r.global.getOwn(r.atoms.intern("c")).value().Object()
	cl := r.global.getOwn(r.atoms.intern("add")).value().Object().fn().closure
	fn := cl.fn
	if fn.PrivateSites != 4 {
		t.Fatalf("PrivateSites = %d, want 4", fn.PrivateSites)
	}
	if len(cl.ic) != int(fn.PropSites+fn.PrivateSites)+1 {
		t.Fatalf("%d caches for %d sites and %d private ones", len(cl.ic), fn.PropSites, fn.PrivateSites)
	}
	// The getter is installed first, then the fields in their order.
	keys := c.keys()
	if len(keys) != 3 || keys[0].flags&propAccessor == 0 || keys[1].flags&propAccessor != 0 || keys[2].flags&propAccessor != 0 {
		t.Fatalf("keys %v", keys)
	}
	// The code reads #n, writes it, reads #m and reads #g, in that order.
	for i, want := range []int32{1, 1, 2, -1} {
		e := &cl.ic[fn.PropSites+1+uint32(i)]
		if want < 0 {
			if e.ownShape != noShape {
				t.Errorf("private site %d: the getter's member is cached", i)
			}
		} else if e.ownShape != c.shape || e.idx != want {
			t.Errorf("private site %d: cached %v at %d, want %d", i, e.ownShape == c.shape, e.idx, want)
		}
	}
}
