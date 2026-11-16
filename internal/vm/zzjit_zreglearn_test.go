//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regLearnSrc reads and writes properties of objects of two shapes: a
// function compiled for the first deoptimizes at the second, and what its
// code learned at its exits (jitRegDeoptSeen) has its next compile take
// both, as the stack form's code does. Go calls the functions, so that
// no caller's code inlines them.
const regLearnSrc = `
function A(x) { this.x = x; this.y = 1 }
function B(x) { this.w = 0; this.x = x; this.y = 2 }
function get(o, n) { var s = 0; for (var j = 0; j < n; j++) s += o.x + o.y; return s }
function put(o, v, n) { for (var j = 0; j < n; j++) o.x = v + j; return o.x }
var a = new A(1), b = new B(2)
`

// regLearnRun calls get and put of regLearnSrc in r: k times with a, then
// k times with a and b in turn, twice.
func regLearnRun(r *Runtime, k int) string {
	global := func(name string) Value { v, _ := r.GetProp(Obj(r.Global()), r.Intern(name)); return v }
	get, put, a, b := global("get"), global("put"), global("a"), global("b")
	s := 0.0
	for round := range 3 {
		for i := range k {
			o := a
			if round > 0 && i%2 != 0 {
				o = b
			}
			v, err := r.Call(get, Undefined, []Value{o, Float(3)})
			if err != nil {
				return regNativeShow(r, v, err)
			}
			w, err := r.Call(put, Undefined, []Value{o, Float(float64(i)), Float(3)})
			if err != nil {
				return regNativeShow(r, w, err)
			}
			s += v.Number() + w.Number()
		}
	}
	return regNativeShow(r, Float(s), nil)
}

// jitRegLearnPlain has r tier up as it does with no QJS_JIT_STRESS, which
// the learning TestJITRegPolymorphicLearned compares is written for: each
// form's code compiled once its sites' caches know the first shape, then
// left at the second's. Under the stress each form's first code is
// compiled at its first call, a scan before any cache knew a shape, and
// what each then learns depends on when its compiles happen -- in the
// background or at once, at which check installed -- not on what its
// exits teach it.
func jitRegLearnPlain(r *Runtime) {
	r.jitStress = jitStressConfig{}
	r.jitCallThreshold, r.jitICThreshold = jitInvocationsForMaglev, jitInvocationsAfterICUpdate
	r.interruptCounter = interruptCheckInterval
}

// regLearnSite is the Property answer for the first instruction of op in
// the program the code of the global function name of r's was compiled
// from: of its register code where reg says, of its stack code otherwise.
func regLearnSite(t *testing.T, r *Runtime, name string, op ir.Op, reg bool) ssa.PropertySite {
	t.Helper()
	v, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
	cl := v.Object().fn().closure
	var p *ir.Program
	var fb interface {
		Property(int) (ssa.PropertySite, bool)
	}
	if reg {
		e := r.jitRegFor(cl, cl.reg)
		if e == nil || e.p == nil {
			t.Fatalf("%s: not compiled: %+v", name, e)
		}
		p, fb = e.p, r.jitRegFeedbackFor(cl, e.p)
	} else {
		se := r.jit.cache[r.jitKeyOf(cl.fn)]
		if se == nil || se.ssaProg == nil {
			t.Fatalf("%s: not compiled", name)
		}
		p, fb = se.ssaProg, &jitFeedback{r: r, fn: cl.fn, cl: cl, e: se, prog: se.ssaProg}
	}
	for pc, in := range p.Code {
		if in.Op == op {
			site, _ := fb.Property(pc)
			return site
		}
	}
	t.Fatalf("%s: no instruction of op %d", name, op)
	return ssa.PropertySite{}
}

// TestJITRegPolymorphicLearned: a read's and a write's sites of register
// code compiled for one shape, deoptimized at a second, compiled again as
// the stack form's code is, from what each learned at its exits.
func TestJITRegPolymorphicLearned(t *testing.T) {
	rts := regNativeRuntimes(t, regLearnSrc)
	jitRegLearnPlain(rts[0])
	var got [2]string
	for i, r := range rts {
		got[i] = regLearnRun(r, 3000)
	}
	if got[0] != got[1] {
		t.Fatalf("with the JIT %q, without %q", got[0], got[1])
	}
	r := rts[0]
	if st := r.RegNativeStats(); st.Entries == 0 {
		t.Fatalf("no native code entered")
	}
	// The stack form's code, run alike.
	SetRegTier(false)
	SetJITRegTier(false)
	sr := New(Config{JIT: true})
	t.Cleanup(sr.Close)
	jitRegLearnPlain(sr)
	if _, err := sr.Run(compileForTest(t, regLearnSrc)); err != nil {
		t.Fatal(err)
	}
	regLearnRun(sr, 3000)
	for _, c := range []struct {
		name string
		op   ir.Op
	}{{"get", ir.PropertyRead}, {"put", ir.PropertyWrite}} {
		reg, stack := regLearnSite(t, r, c.name, c.op, true), regLearnSite(t, sr, c.name, c.op, false)
		t.Logf("%s: register %+v, stack %+v", c.name, reg, stack)
		if reg.Shape == 0 || len(reg.Cases) != len(stack.Cases) || reg.Megamorphic != stack.Megamorphic {
			t.Errorf("%s: register %+v, stack %+v", c.name, reg, stack)
		}
	}
}

// regLeafSrc calls leaves -- built-ins native code calls without a frame
// (jitLeafOp) -- from register code: those native code makes itself
// (ssa.LeafKind), a string's codePointAt and at, Number.isNaN, and one Go
// calls (GoLeaf), a string's startsWith; and Math.max, which is none.
const regLeafSrc = `
function leaves(s, n) {
	var t = 0
	for (var i = 0; i < n; i++) {
		t += s.codePointAt(i % 5) + (Number.isNaN(t) ? 1 : 0) + Math.max(i, 3) + (s.startsWith("h") ? 2 : 0)
		if (s.at(i % 7) === undefined) t++
	}
	return t
}
var re = /l+(o)?/g, word = /^h/
function rx(s, n) {
	var t = 0
	for (var i = 0; i < n; i++) {
		var m = re.exec(s)
		t += m ? m.index + m[0].length + re.lastIndex : -1
		if (word.test(s)) t += 2
	}
	return t
}
`

// TestJITRegLeaf: register code's calls of leaves, answered from the
// call sites' caches (jitRegFeedback's Leaf), made natively or as calls of
// Go of their own, each answer as the interpreter's; a receiver of another
// kind, and a leaf replaced, as Go's call.
func TestJITRegLeaf(t *testing.T) {
	rts := regNativeRuntimes(t, regLeafSrc)
	for _, s := range []string{"hello", "h\U0001F600llo", ""} {
		var got [2]string
		for i, r := range rts {
			for _, name := range []string{"leaves", "rx"} {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
				var v Value
				var err error
				for range 3 {
					v, err = r.Call(fn, Undefined, []Value{Str(NewString(s)), Float(2000)})
				}
				got[i] += regNativeShow(r, v, err) + ";"
			}
		}
		if got[0] != got[1] {
			t.Errorf("%q: with the JIT %q, without %q", s, got[0], got[1])
		}
	}
	r := rts[0]
	for _, c := range []struct {
		name string
		want int
	}{{"leaves", 4}, {"rx", 2}} {
		// rx's exec, its array kept in a local the interpreter may make
		// it again in (RMarkReuse), and its test, marked dead -- only its
		// truth is read -- both leaves, as the stack form's code calls
		// them: the leaf makes the array anew, and the whole answer.
		v, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
		cl := v.Object().fn().closure
		e := r.jitRegFor(cl, cl.reg)
		if e == nil || e.p == nil {
			t.Fatalf("%s: not compiled: %+v", c.name, e)
		}
		fb := r.jitRegFeedbackFor(cl, e.p)
		leaves := 0
		for pc := range e.p.Code {
			if _, ok := fb.Leaf(pc); ok {
				leaves++
			}
		}
		if leaves != c.want {
			t.Errorf("%s: %d calls of leaves, want %d", c.name, leaves, c.want)
		}
	}
	if st := r.RegNativeStats(); st.Entries == 0 {
		t.Errorf("no native code entered")
	}
}

// regCallsSrc calls the built-ins native code makes itself from register
// code: Math's functions, a string's charCodeAt and substring and charAt,
// String.fromCharCode.
const regCallsSrc = `
var sqrt = Math.sqrt
function calls(s, n) {
	var t = 0, u = ""
	for (var i = 0; i < n; i++) {
		t += Math.sqrt(i) + Math.floor(i / 3) + Math.max(i, 7, t % 5) + Math.imul(i, 3) + sqrt(i + 1)
		t += s.charCodeAt(i % s.length)
		u = s.substring(i % 3, i % 3 + 1) + s.charAt(i % 4) + String.fromCharCode(65 + i % 26)
	}
	return t + u
}
`

// TestJITRegCalls: register code's calls of the built-ins the builder
// makes itself (jitRegFeedback's Intrinsic, CharCode, Substring,
// FromCharCode), from the operands in the call's window, each answer as
// the interpreter's -- a receiver or a code they do not take, Go's.
func TestJITRegCalls(t *testing.T) {
	rts := regNativeRuntimes(t, regCallsSrc)
	for _, s := range []string{"hello", "h\U0001F600\u00e9llo\u4e00x", "ab"} {
		var got [2]string
		// Native code reads a string's code units where it has made them
		// (mir's stringUnit), Go making them at the first read that needs
		// them, at an exit: made here first, so that what the code's calls
		// do is what the test sees, whenever QJS_JIT_STRESS's budget has
		// the code entered first.
		str := NewString(s)
		str.units()
		for i, r := range rts {
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("calls"))
			var v Value
			var err error
			for range 3 {
				v, err = r.Call(fn, Undefined, []Value{Str(str), Float(2000)})
			}
			got[i] = regNativeShow(r, v, err)
		}
		if got[0] != got[1] {
			t.Errorf("%q: with the JIT %q, without %q", s, got[0], got[1])
		}
	}
	r := rts[0]
	v, _ := r.GetProp(Obj(r.Global()), r.Intern("calls"))
	cl := v.Object().fn().closure
	e := r.jitRegFor(cl, cl.reg)
	if e == nil || e.p == nil {
		t.Fatalf("not compiled: %+v", e)
	}
	fb := r.jitRegFeedbackFor(cl, e.p)
	var intrinsics, charCodes, substrings, fromChars int
	for pc := range e.p.Code {
		if _, ok := fb.Intrinsic(pc); ok {
			intrinsics++
		}
		if fb.CharCode(pc) {
			charCodes++
		}
		if _, _, ok := fb.Substring(pc); ok {
			substrings++
		}
		if _, ok := fb.FromCharCode(pc); ok {
			fromChars++
		}
	}
	if intrinsics != 5 || charCodes != 1 || substrings != 2 || fromChars != 1 {
		t.Errorf("intrinsics %d, charCodeAt %d, substrings %d, fromCharCode %d; want 5, 1, 2, 1", intrinsics, charCodes, substrings, fromChars)
	}
	st := r.RegNativeStats()
	if st.Entries == 0 {
		t.Errorf("no native code entered")
	}
	// Made natively: none of the calls is left to Go whole.
	if n := st.HostOps["call_method_w"] + st.HostOps["call_w"]; n != 0 {
		t.Errorf("%d calls left to Go at host exits: %v", n, st.HostOps)
	}
	// A string whose code units are not made yet: native code's first read
	// of one leaves for Go, at most once, which makes them, and reads them
	// natively from then on -- no call left to Go after the first.
	fresh := func() Value { return Str(NewString("h\U0001F600éllo一y")) }
	calls := func(r *Runtime, s Value) string {
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern("calls"))
		v, err := r.Call(fn, Undefined, []Value{s, Float(2000)})
		return regNativeShow(r, v, err)
	}
	left := func() uint64 {
		return r.RegNativeStats().HostOps["call_method_w"] + r.RegNativeStats().HostOps["call_w"]
	}
	s0, s1 := fresh(), fresh()
	before := left()
	if a, b := calls(r, s0), calls(rts[1], s1); a != b {
		t.Errorf("a fresh string: with the JIT %q, without %q", a, b)
	}
	first := left()
	for range 2 {
		calls(r, s0)
	}
	if first-before > 1 || left() != first {
		t.Errorf("a fresh string: %d calls left to Go at the first call, %d after it; want at most 1, 0", first-before, left()-first)
	}
}

// regLiteralSrc makes object, array and regular expression literals in
// register code: native code takes them from pools (jitRegFeedback's
// Literal), and defines a literal's fields along its shapes' transitions
// (Define).
const regLiteralSrc = `
function lits(n) {
	var t = 0, keep = []
	for (var i = 0; i < n; i++) {
		var o = {x: i, y: i + 1, z: "z"}, e = {}, a = [i, i * 2, o], z = [], re = /a(b)?/g
		e.k = i
		z.push(i)
		t += o.x + o.y + a[1] + a.length + e.k + z[0] + re.lastIndex + (re.test("ab") ? 1 : 0) + re.lastIndex
		if (i % 100 == 0) keep.push(o, a, re)
	}
	return t + ":" + keep.length + ":" + JSON.stringify(keep[3]) + (keep[2] === keep[5])
}
`

// TestJITRegLiterals: register code's literals made from pools, each a new
// object as the interpreter makes it.
func TestJITRegLiterals(t *testing.T) {
	rts := regNativeRuntimes(t, regLiteralSrc)
	var got [2]string
	for i, r := range rts {
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern("lits"))
		var v Value
		var err error
		for range 3 {
			v, err = r.Call(fn, Undefined, []Value{Float(3000)})
		}
		got[i] = regNativeShow(r, v, err)
	}
	if got[0] != got[1] {
		t.Fatalf("with the JIT %q, without %q", got[0], got[1])
	}
	r := rts[0]
	v, _ := r.GetProp(Obj(r.Global()), r.Intern("lits"))
	cl := v.Object().fn().closure
	e := r.jitRegFor(cl, cl.reg)
	if e == nil || e.p == nil {
		t.Fatalf("not compiled: %+v", e)
	}
	fb := r.jitRegFeedbackFor(cl, e.p)
	lits, defines := 0, 0
	for pc := range e.p.Code {
		if _, ok := fb.Literal(pc); ok {
			lits++
		}
		if _, ok := fb.Define(pc); ok {
			defines++
		}
	}
	// Two arrays, two objects and the RegExp in the loop, and keep's array
	// before it.
	if lits != 6 || defines != 3 {
		t.Errorf("%d literals, %d fields defined natively; want 6, 3", lits, defines)
	}
	if st := r.RegNativeStats(); st.Entries == 0 {
		t.Errorf("no native code entered")
	}
}

// regKindsSrc runs a class's constructors, a derived one's before and
// after its super call, its accessors and methods, and an arrow reading
// its this, in register code: the setter called by Go too (Reflect.set),
// which native code that inlines it never calls.
const regKindsSrc = `
class P { constructor(x) { this.x = x; this.s = 0; for (let i = 0; i < 2; i++) this.s += i }
	get dbl() { let d = 0; for (let i = 0; i < 2; i++) d += this.x; return d }
	set v(n) { for (let i = 0; i < 2; i++) this.s += n }
	sum(n) { let t = 0; for (let i = 0; i < n; i++) t += this.x + i; return t } }
class C extends P { constructor(x) { let k = 0; for (let i = 0; i < 3; i++) k += i; super(x + k); this.y = 1 } sum(n) { return super.sum(n) + this.y } }
function kinds(n) {
	let t = 0
	const add = (a) => { let r = 0; for (let i = 0; i < 4; i++) r += a + this.k; return r }
	for (let i = 0; i < n; i++) { const c = new C(i); c.v = i; Reflect.set(c, "v", i); t += c.dbl + c.sum(3) + c.s + add(i) }
	return t
}
function drive(n) { return kinds.call({k: 2}, n) }
`

// TestJITRegKinds: an arrow's, a class's constructors' and its accessors'
// register code compiled and entered as a function's is (jitRegTakes),
// each answer as the interpreter's.
func TestJITRegKinds(t *testing.T) {
	rts := regNativeRuntimes(t, regKindsSrc)
	for _, r := range rts {
		r.SetJITWarmUp(1, 1)
	}
	var got [2]string
	for i, r := range rts {
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern("drive"))
		var v Value
		var err error
		for range 3 {
			v, err = r.Call(fn, Undefined, []Value{Float(300)})
		}
		got[i] = regNativeShow(r, v, err)
	}
	if got[0] != got[1] {
		t.Fatalf("with the JIT %q, without %q", got[0], got[1])
	}
	r := rts[0]
	kinds := map[bytecode.FuncKind]bool{}
	why := map[bytecode.FuncKind][]string{}
	for key, e := range r.jit.regCache {
		if fn := key.Value(); fn != nil {
			if e.code != nil {
				kinds[fn.Kind] = true
			} else {
				why[fn.Kind] = append(why[fn.Kind], fn.Name+": "+e.refusal)
			}
		}
	}
	for _, k := range []bytecode.FuncKind{bytecode.KindArrow, bytecode.KindConstructor, bytecode.KindDerivedConstructor, bytecode.KindGetter, bytecode.KindSetter, bytecode.KindMethod} {
		if !kinds[k] {
			t.Errorf("no code of kind %v compiled: %v, refused %q", k, kinds, why[k])
		}
	}
}

// regReplaceSrc replaces by RegExps held in bindings, as the V8 suite's
// RegExp does: a captured binding's global RegExp and a global's, a string
// template and an empty one, one call's result unused -- leaves, as the
// stack form's code calls them (jitReplaceLeafSeen) -- and a literal's,
// which the call makes and gives back, and a function's replacement, which
// the leaf's check refuses each call of, both left calls of Go.
const regReplaceSrc = `
var g = /[aeiou]/;
var replaces = (function () {
	var re = /\s+/g;
	return function (s, n) {
		var out = "", k = 0;
		for (var i = 0; i < n; i++) {
			out = s.replace(re, "-");
			s.replace(g, "");
			k += out.replace(/-/g, "+").length + s.replace(re, function (m) { return m.length }).length;
		}
		return out + k + g.lastIndex + re.lastIndex;
	};
})();
`

func TestJITRegReplaceLeaf(t *testing.T) {
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regReplaceSrc)
			for _, s := range []string{"a b  c\td", "x\U0001F600 y", ""} {
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("replaces"))
					var v Value
					var err error
					for range 3 {
						v, err = r.Call(fn, Undefined, []Value{Str(NewString(s)), Float(2000)})
					}
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Errorf("%q: with the JIT %q, without %q", s, got[0], got[1])
				}
			}
			r := rts[0]
			v, _ := r.GetProp(Obj(r.Global()), r.Intern("replaces"))
			cl := v.Object().fn().closure
			e := r.jitRegFor(cl, cl.reg)
			if e == nil || e.p == nil {
				t.Fatalf("not compiled: %+v", e)
			}
			fb := r.jitRegFeedbackFor(cl, e.p)
			leaves := 0
			for pc := range e.p.Code {
				if o, ok := fb.Leaf(pc); ok && o == uintptr(unsafe.Pointer(r.strReplaceFn)) {
					leaves++
				}
			}
			// The two replaces by bindings' RegExps with string templates
			// and the one by a function's -- whose leaf's check refuses it,
			// Go then making the call -- but not the literal's.
			if leaves != 3 {
				t.Errorf("%d calls of replace's leaf, want 3", leaves)
			}
		})
	}
}

// regSharedSrc makes array literals of constants, whose arrays share them
// until one is written (new_array_k): read, written, grown, and kept, in a
// loop, as the stack form's code makes them natively from a pool of the
// arrays Go makes (ssa's LiteralSite's Shared). Its calls of Go fill every
// register native code may keep with junk (jitClobberCallsForTest): a
// literal whose pool Go fills is made again from its start, and its
// elements -- a string's words in registers -- had been read after the
// call as Go's code left them, a crash where Go 1.24's had used them.
const regSharedSrc = `
var kept = [];
function shared(n) {
	var s = 0;
	for (var i = 0; i < n; i++) {
		var a = [1, 2.5, "x", true, null], b = [3, 4, 5];
		s += a[0] + a[1] + b[i % 3] + a.length;
		if (i % 7 == 0) { b[1] = i; s += b[1] }
		if (i % 11 == 0) { a.push(i); s += a.length }
		if (i % 97 == 0) kept.push(a, b);
	}
	return s + ":" + kept.length + ":" + kept.slice(-4).join("|") + ":" + [3, 4, 5].join();
}
`

func TestJITRegSharedLiteral(t *testing.T) {
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			jitClobberCallsForTest(t)
			rts := regNativeRuntimes(t, regSharedSrc)
			var got [2]string
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("shared"))
				for range 4 {
					v, err := r.Call(fn, Undefined, []Value{Float(3000)})
					got[i] = regNativeShow(r, v, err)
				}
			}
			if got[0] != got[1] {
				t.Errorf("with the JIT %q, without %q", got[0], got[1])
			}
			r := rts[0]
			v, _ := r.GetProp(Obj(r.Global()), r.Intern("shared"))
			cl := v.Object().fn().closure
			e := r.jitRegFor(cl, cl.reg)
			if e == nil || e.p == nil {
				t.Fatalf("not compiled: %+v", e)
			}
			fb := r.jitRegFeedbackFor(cl, e.p)
			pooled, shared := 0, 0
			for pc := range e.p.Code {
				if w, _, ok := fb.at(pc); !ok || w.Op() != bytecode.RNewArrayK {
					continue
				}
				if site, ok := fb.Literal(pc); ok {
					pooled++
					if site.Shared {
						shared++
					}
				}
			}
			// a's, b's and the return's: the return's, never written, its
			// arrays sharing its constants; a's and b's sharing them where
			// the code was compiled before their sites gave sharing up for
			// the writes to their arrays (at the first call, under
			// QJS_JIT_STRESS), their elements written natively after.
			if pooled != 3 || shared < 1 {
				t.Errorf("%d literals of constants made from a pool, %d sharing them; want 3, 1 or more sharing", pooled, shared)
			}
			if st := r.RegNativeStats(); st.HostOps["new_array_k"] > 8 {
				t.Errorf("new_array_k left to Go %d times", st.HostOps["new_array_k"])
			}
		})
	}
}
