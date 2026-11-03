package vm

import (
	"runtime/debug"
	"testing"
)

// regexpPristineSrc warms the RegExp methods' fast paths -- exec, test,
// replace, split, match, matchAll and search, on RegExps global, sticky and
// neither, held by locals and made by literals -- in work(), each call's
// answer what it found and which of the script's functions it ran (log);
// and in prep() a megamorphic read and write of a property named exec and a
// keyed write, which native code makes. A test then changes RegExp.prototype
// or RegExp through one of the ways a script, or a host, can (change), and
// work() must find the change. regexpPristineCases' answers are Node's
// (v26.10.0, each case in a context of its own).
const regexpPristineSrc = `var log = []
var origExec = RegExp.prototype.exec
function myExec(s) { log.push('exec'); return origExec.call(this, s) }
function myTest(s) { log.push('test'); return false }
function myRep(s, r) { log.push('rep'); return 'R' }
function mySplit(s, l) { log.push('split'); return ['S'] }
function myMatch(s) { log.push('match'); return ['M'] }
function mySearch(s) { log.push('search'); return 42 }
function getter(name, v) { return { get() { log.push(name); return v }, configurable: true } }
function Ctor() {}
Object.defineProperty(Ctor, Symbol.species, getter('species', RegExp))
function put(o, v) { o.exec = v }
var bigs = []
for (var i = 0; i < 24; i++) { var o = { exec: 0, a1: 1, a2: 2, a3: 3, a4: 4, a5: 5, a6: 6, a7: 7, a8: 8, a9: 9 }; o['k' + i] = i; bigs.push(o) }
function mget(o) { return o.exec }
function mput(o, v) { o.exec = v }
function kput(o, k, v) { o[k] = v }
function mputs(o, v) { for (var i = 0; i < 3; i++) mput(o, v) }
function kputs(o, k, v) { for (var i = 0; i < 3; i++) kput(o, k, v) }
function prep() {
  var n = 0
  for (var j = 0; j < 50; j++) {
    for (var i = 0; i < bigs.length; i++) { mput(bigs[i], i); n += mget(bigs[i]) }
    n += typeof mget(RegExp.prototype) === 'function'
    kput({ a: j }, 'b', j); kput(bigs[j % 24], 'a1', j)
    mputs(bigs[j % 24], j); kputs({ c: j }, 'd', j)
  }
  return n
}
function put2(o, v) { o.constructor = v }
function t(f) { try { return String(f()) } catch (e) { return e.constructor.name } }
function work() {
  log = []
  var out = []
  var g = /b(c)?/g, n = /b(c)?/, y = /b/y
  for (var i = 0; i < 2; i++) {
    g.lastIndex = 0; y.lastIndex = 0
    out.push(t(() => { var m = n.exec('abcb'); return m && m.index + m[0] }))
    out.push(t(() => n.test('xb')), t(() => /b/.test('ab')), t(() => /c/.exec('abc')))
    out.push(t(() => 'abcbd'.replace(g, '-')), t(() => 'abcb'.replace(n, '+')), t(() => 'ab'.replace(/b/, '*')))
    out.push(t(() => 'abcbd'.split(/b/)), t(() => 'abcbd'.split(n)))
    out.push(t(() => 'abcb'.match(g)), t(() => 'abcb'.match(/c/)))
    out.push(t(() => [...'abcb'.matchAll(g)].length), t(() => 'xxb'.search(n)), t(() => y.test('b')), t(() => g.exec('ab')))
  }
  var c = {}
  log.forEach(k => { c[k] = (c[k] || 0) + 1 })
  return out.join(' ') + ' log=' + Object.keys(c).map(k => k + c[k]).join(',')
}`

// regexpPristineBefore is work()'s answer before any change, Node's, and
// regexpPristinePrep prep()'s.
const regexpPristineBefore = "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log="

const regexpPristinePrep = "13850"

// regexpPristineCases changes RegExp.prototype or RegExp each a way of its
// own: an assignment at a site of its own, by a key, at a site that
// remembers dictionary objects and at one that has given up, through
// Reflect.set, Object.assign, a with statement and a super property;
// definitions of a value, a getter -- of a flag, of flags, of
// Symbol.species --, through Object.defineProperties, __defineGetter__,
// Reflect.defineProperty and the host's DefineGetter (host); deletes;
// Object.freeze; and natively, by a keyed write and a megamorphic one.
// proto and ctor say whether RegExp.prototype and RegExp are pristine after
// it: a change of RegExp.prototype's prototype leaves its own properties
// as they were.
type regexpPristineCase struct {
	name, change, want string
	proto, ctor, host  bool
}

var regexpPristineCases = []regexpPristineCase{
	{name: "assign", change: "RegExp.prototype.exec = myExec", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=exec56", proto: false, ctor: true, host: false},
	{name: "keyed", change: "RegExp.prototype['te' + 'st'] = myTest", want: "1bc false false c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 false b, 1bc false false c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 false b, log=test6", proto: false, ctor: true, host: false},
	{name: "keyedSymbol", change: "RegExp.prototype[Symbol.replace] = myRep", want: "1bc true true c R R R a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c R R R a,c,d a,c,,,d bc,b c 2 2 true b, log=rep6", proto: false, ctor: true, host: false},
	{name: "dictSite", change: "var d1 = { x: 1, exec: 0 }, d2 = { y: 1, exec: 0 }; delete d1.x; delete d2.y; put(d1, 1); put(d2, 2); put(d1, 3); put(RegExp.prototype, myExec)", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=exec56", proto: false, ctor: true, host: false},
	{name: "gaveUpSite", change: "for (var i = 0; i < 20; i++) { var o = { constructor: 0 }; o['k' + i] = i; put2(o, i) } put2(RegExp.prototype, Ctor)", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=species6", proto: false, ctor: true, host: false},
	{name: "reflectSet", change: "Reflect.set(RegExp.prototype, Symbol.match, myMatch)", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d M M 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d M M 2 2 true b, log=match4", proto: false, ctor: true, host: false},
	{name: "assignAll", change: "Object.assign(RegExp.prototype, { exec: myExec, [Symbol.search]: mySearch })", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 42 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 42 true b, log=exec54,search2", proto: false, ctor: true, host: false},
	{name: "defineValue", change: "Object.defineProperty(RegExp.prototype, Symbol.split, { value: mySplit })", want: "1bc true true c a--d a+b a* S S bc,b c 2 2 true b, 1bc true true c a--d a+b a* S S bc,b c 2 2 true b, log=split4", proto: false, ctor: true, host: false},
	{name: "defineGetter", change: "Object.defineProperty(RegExp.prototype, 'global', getter('global', false))", want: "1bc true true c a-bd a+b a* a,c,d a,c,,,d b, c TypeError 2 true null 1bc true true c a-bd a+b a* a,c,d a,c,,,d b, c TypeError 2 true null log=global16", proto: false, ctor: true, host: false},
	{name: "defineProperties", change: "Object.defineProperties(RegExp.prototype, { flags: getter('flags', '') })", want: "1bc true true c a-bd a+b a* a,c,d a,c,,,d b, c TypeError 2 true null 1bc true true c a-bd a+b a* a,c,d a,c,,,d b, c TypeError 2 true null log=flags16", proto: false, ctor: true, host: false},
	{name: "legacyGetter", change: "RegExp.prototype.__defineGetter__('sticky', function () { log.push('sticky'); return false })", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=sticky18", proto: false, ctor: true, host: false},
	{name: "reflectDefine", change: "Reflect.defineProperty(RegExp.prototype, 'unicode', getter('unicode', false))", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=unicode18", proto: false, ctor: true, host: false},
	{name: "species", change: "Object.defineProperty(RegExp, Symbol.species, getter('species', RegExp))", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=species6", proto: true, ctor: false, host: false},
	{name: "remove", change: "delete RegExp.prototype[Symbol.replace]", want: "1bc true true c abcbd abcb ab a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c abcbd abcb ab a,c,d a,c,,,d bc,b c 2 2 true b, log=", proto: false, ctor: true, host: false},
	{name: "reflectDelete", change: "Reflect.deleteProperty(RegExp.prototype, Symbol.split)", want: "1bc true true c a--d a+b a* abcbd abcbd bc,b c 2 2 true b, 1bc true true c a--d a+b a* abcbd abcbd bc,b c 2 2 true b, log=", proto: false, ctor: true, host: false},
	{name: "with", change: "with (RegExp.prototype) { exec = myExec }", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=exec56", proto: false, ctor: true, host: false},
	{name: "super", change: "({ __proto__: {}, m() { super.exec = myExec } }).m.call(RegExp.prototype)", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=exec56", proto: false, ctor: true, host: false},
	{name: "freeze", change: "Object.freeze(RegExp.prototype); RegExp.prototype.exec = myExec", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=", proto: false, ctor: true, host: false},
	{name: "hostGetter", change: "", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=hasIndices18", proto: false, ctor: true, host: true},
	{name: "keyedNative", change: "kputs(RegExp.prototype, 'exec', myExec)", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=exec56", proto: false, ctor: true, host: false},
	{name: "megaNative", change: "mputs(RegExp.prototype, myExec)", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=exec56", proto: false, ctor: true, host: false},
	{name: "setProto", change: "Object.setPrototypeOf(RegExp.prototype, { exec: myExec, [Symbol.replace]: myRep })", want: "1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, 1bc true true c a--d a+b a* a,c,d a,c,,,d bc,b c 2 2 true b, log=", proto: true, ctor: true, host: false},
}

// regexpPristineCheck runs regexpPristineSrc in r with c's change: work()
// and prep() three times, which leave RegExp.prototype and RegExp pristine,
// then the change, then work() twice, each answer Node's, and the objects
// pristine as c says.
func regexpPristineCheck(t *testing.T, r *Runtime, c regexpPristineCase, where string) {
	t.Helper()
	run := func(src string) string {
		t.Helper()
		v, err := r.Run(compileForTest(t, "''+("+src+")"))
		if err != nil {
			t.Fatalf("%s: %s: %s: %v", where, c.name, src, err)
		}
		return v.String().Go()
	}
	if _, err := r.Run(compileForTest(t, regexpPristineSrc+"\nfunction change() { "+c.change+" }")); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got := run("work()"); got != regexpPristineBefore {
			t.Fatalf("%s: %s: before the change\ngot  %s\nwant %s", where, c.name, got, regexpPristineBefore)
		}
		if got := run("prep()"); got != regexpPristinePrep {
			t.Fatalf("%s: %s: prep() = %s, want %s", where, c.name, got, regexpPristinePrep)
		}
	}
	if !r.proto.regexp.shape.pristine || !r.proto.regexpCtor.shape.pristine {
		t.Errorf("%s: %s: before the change, RegExp.prototype pristine %v, RegExp %v", where, c.name,
			r.proto.regexp.shape.pristine, r.proto.regexpCtor.shape.pristine)
	}
	if c.host {
		// The host's getter, as Object.defineProperty's in Node's run:
		// it logs its name and answers false.
		r.DefineGetter(r.proto.regexp, "hasIndices", func(rt *Runtime, this Value, args []Value) (Value, error) {
			log, err := rt.getValueProp(Obj(rt.global), rt.atoms.intern("log"))
			if err != nil {
				return Undefined, err
			}
			push, err := rt.getValueProp(log, rt.atoms.intern("push"))
			if err != nil {
				return Undefined, err
			}
			_, err = rt.call(push, log, []Value{Str(NewString("hasIndices"))})
			return Bool(false), err
		})
	} else {
		run("change()")
	}
	for range 2 {
		if got := run("work()"); got != c.want {
			t.Errorf("%s: %s: after the change\ngot  %s\nwant %s", where, c.name, got, c.want)
		}
	}
	if p := r.proto.regexp.shape.pristine; p != c.proto {
		t.Errorf("%s: %s: RegExp.prototype pristine %v after the change, want %v", where, c.name, p, c.proto)
	}
	if p := r.proto.regexpCtor.shape.pristine; p != c.ctor {
		t.Errorf("%s: %s: RegExp pristine %v after the change, want %v", where, c.name, p, c.ctor)
	}
}

// Every change to RegExp.prototype or RegExp a script or a host makes is one
// the RegExp methods find after their fast paths have run, in the tree tier
// and in the interpreter, with the collector idle: each answer is Node's,
// and the object changed is no longer pristine.
func TestRegExpPristine(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		for _, c := range regexpPristineCases {
			gc := debug.SetGCPercent(-1)
			r := New(Config{})
			regexpPristineCheck(t, r, c, map[bool]string{true: "tree tier", false: "interpreter"}[tree])
			r.Close()
			debug.SetGCPercent(gc)
		}
	}
	SetTreeTier(true)
}

// A realm's RegExp.prototype and RegExp are pristine once built, with
// layouts of their own that a change gives each another of; a realm made
// later has its own, which a change to the first one's leaves pristine.
func TestRegExpPristineRealms(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	p, ctor := r.proto.regexp, r.proto.regexpCtor
	if !p.shape.pristine || !ctor.shape.pristine || !p.shape.seen || !p.shape.unique {
		t.Fatalf("RegExp.prototype pristine %v (seen %v, unique %v), RegExp pristine %v",
			p.shape.pristine, p.shape.seen, p.shape.unique, ctor.shape.pristine)
	}
	if int(p.valn) <= pristineMinProps || int(ctor.valn) <= pristineMinProps {
		t.Fatalf("RegExp.prototype has %d properties, RegExp %d: no more than native code searches", int(p.valn), int(ctor.valn))
	}
	re := r.NewRealm()
	var other *Object
	r.InRealm(re, func() { other = r.proto.regexp })
	if other == p || !other.shape.pristine {
		t.Fatalf("the second realm's RegExp.prototype is the first's (%v) or not pristine", other == p)
	}
	if _, err := r.Run(compileForTest(t, "delete RegExp.prototype.compile")); err != nil {
		t.Fatal(err)
	}
	if p.shape.pristine || !other.shape.pristine {
		t.Errorf("after a delete, RegExp.prototype pristine %v, the second realm's %v", p.shape.pristine, other.shape.pristine)
	}
}
