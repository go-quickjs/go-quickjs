//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The calls' family of a snapshot's derived methods (jit_snapderive.go):
// the methods that answer from the call caches the interpreter fills
// (regCalls) and the realm's built-ins a call there is compared with. The
// caches are copied whole once a compile, each closure's in its copy
// (jitClosureCopy's calls), as regCallsRestored would have restored them;
// the realm's built-ins, a few words, once a snapshot. The built-ins a
// cache names are read as they are: what a native function's data says of
// it is fixed when it is made. The feedback answers from the same record
// of the live caches and the realm, made at its first question (callsData):
// one implementation.

// jitCallsData is what the calls' methods answer from: the function's
// register code lowered (p, rc), its closure's call caches -- live, through
// regCallsRestored, or copied -- the realm's built-ins (jitRealmCallees),
// and whether the feedback is a callee's (root).
type jitCallsData struct {
	p        *ir.Program
	rc       *bytecode.RegCode
	gathered bool
	cl       *closure
	// r is the runtime whose live caches the feedback's own record reads,
	// nil in a snapshot's copy, whose calls are copied.
	r      *Runtime
	copied []regCallCache
	realm  jitRealmCallees
	// tg is what NativeCalls' derivation is made of (jit_snaptargets.go).
	tg jitTargetsData
}

// jitRealmCallees are the realm's built-ins the calls' methods compare a
// call's callee with (jitRealmFields, the Runtime's), and the cache of
// Math.random's numbers native code reads (mathRandom), made where a call
// may read it.
type jitRealmCallees struct {
	// Nothing compares it: comparable, the type had an equality function
	// the compiler lays out among package vm's code.
	_                                 [0]func()
	callFn, parseInt, bigToString     *Object
	randomFn, powFn, atan2Fn, hypotFn *Object
	charCodeAt, fromCharCode          Value
	substring, slice, substr, charAt  Value
	searches                          [jitSearchKinds]*Object
	random                            *mathRandom
}

// jitCallsMethods are the methods answered from a jitCallsData.
const jitCallsMethods uint64 = 1<<snapCharCode | 1<<snapSubstring | 1<<snapFromCharCode | 1<<snapIntrinsic |
	1<<snapIndexOf | 1<<snapParseInt | 1<<snapBigString

func init() {
	jitSnapDerived |= jitCallsMethods
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, jitCallsGather)
	jitSnapDerivers[jitDeriveCopies] = append(jitSnapDerivers[jitDeriveCopies], jitCallsDerive)
}

// realmCallees is r's built-ins the calls' methods compare with, now.
func (r *Runtime) realmCallees() jitRealmCallees {
	return jitRealmCallees{
		callFn: r.callFn, parseInt: r.jitParseInt, bigToString: r.jitBigToString,
		randomFn: r.randomFn, powFn: r.powFn, atan2Fn: r.atan2Fn, hypotFn: r.hypotFn,
		charCodeAt: r.jitCharCodeAt, fromCharCode: r.jitFromCharCode,
		substring: r.jitSubstring, slice: r.jitSlice, substr: r.jitSubstr, charAt: r.jitCharAt,
		searches: r.jitSearches, random: r.rnd,
	}
}

// callsData is f's record of its closure's call caches and the realm's
// built-ins, the live state's: made at its first question; its caches read
// live at each (regCallsRestored), its realm's as they were then, which no
// compile changes.
func (f *jitRegFeedback) callsData() *jitCallsData {
	if f.calls == nil {
		r := f.fb.r
		f.calls = &jitCallsData{p: f.p, rc: f.rc, gathered: true, cl: f.fb.cl, r: r, realm: r.realmCallees()}
	}
	return f.calls
}

// jitCallsGather is the family's gather of s, fb's snapshot: the closure's
// call caches, by its copy, and the realm's built-ins; and Math.random's
// cache made where a call's cache holds Math.random, as the question of
// its call makes it.
func jitCallsGather(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.calls
	r := fb.fb.r
	*d = jitCallsData{p: fb.p, rc: fb.rc, gathered: true, cl: fb.fb.cl, realm: r.realmCallees()}
	if d.cl == nil {
		return
	}
	d.copied = s.root.d.closureCopy(d.cl).calls
	if d.realm.random == nil && d.realm.randomFn != nil &&
		slices.ContainsFunc(d.copied, func(c regCallCache) bool { return c.callee == d.realm.randomFn }) {
		d.realm.random = r.mathRandom()
	}
}

// jitCallsCopy is cl's call caches copied whole (jitClosureCopy's calls),
// restored first from what a turn's end kept of them (regCallsRestored)
// where a call site the snapshot asks is empty, as the asking would have
// restored them.
func jitCallsCopy(r *Runtime, cl *closure) []regCallCache {
	calls := cl.regCalls
	if rc := cl.fn.RegCode(); cl.regSeen != nil && rc != nil {
		for _, w := range rc.Code {
			switch w.Op() {
			case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW, bytecode.RCallWT, bytecode.RCallMWT, bytecode.RApplyArgs:
				if i := int(w.C()); i < len(calls) && calls[i].callee == nil {
					calls = r.regCallsRestored(cl, i)
				}
			}
			if cl.regSeen == nil {
				break
			}
		}
	}
	return slices.Clone(calls)
}

// calls is the closure's call caches, as an ask of site finds them.
func (d *jitCallsData) calls(site int) []regCallCache {
	if d.r != nil {
		return d.r.regCallsRestored(d.cl, site)
	}
	return d.copied
}

// jitCallsDerive puts s's answers of the calls' methods, at every PC whose
// op asks each, from what the gather took; their effects, the built-ins
// the code keeps, appended to the snapshot's sink.
func jitCallsDerive(s *jitSnapshot) {
	d := &s.d.calls
	if !d.gathered {
		return
	}
	nd := &s.d.native
	sink := s.sinkOf()
	a := &s.a
	for pc, op := range s.ops {
		asks := jitSnapAsks[op] & jitCallsMethods
		if asks == 0 {
			continue
		}
		at := int64(pc)
		if asks&(1<<snapCharCode) != 0 {
			r0 := d.charCode(pc)
			s.heard(snapCharCode, at, r0, r0)
			if r0 {
				a.CharCode.reserve(int(s.room[snapCharCode]))
				a.CharCode.put(at, 0, true, jitSnapAnsCharCode{r0: r0})
			}
		}
		if asks&(1<<snapSubstring) != 0 {
			r0, r1, r2 := d.substring(pc)
			s.heard(snapSubstring, at, r2, r0, r1, r2)
			if r2 {
				a.Substring.reserve(int(s.room[snapSubstring]))
				a.Substring.put(at, 0, true, jitSnapAnsSubstring{r0: r0, r1: r1, r2: r2})
			}
		}
		if asks&(1<<snapFromCharCode) != 0 {
			r0, r1 := d.fromCharCode(pc)
			s.heard(snapFromCharCode, at, r1, r0, r1)
			if r1 {
				a.FromCharCode.reserve(int(s.room[snapFromCharCode]))
				a.FromCharCode.put(at, 0, true, jitSnapAnsFromCharCode{r0: r0, r1: r1})
			}
		}
		if asks&(1<<snapIntrinsic) != 0 {
			before := s.begin(sink)
			r0, r1 := d.intrinsic(pc, sink.keep())
			eff := s.endDerived(sink, before)
			s.heard(snapIntrinsic, at, r1, r0, r1)
			if r1 || eff != 0 {
				a.Intrinsic.reserve(int(s.room[snapIntrinsic]))
				a.Intrinsic.put(at, eff, r1, jitSnapAnsIntrinsic{r0: r0, r1: r1})
			}
		}
		if asks&(1<<snapIndexOf) != 0 {
			before := s.begin(sink)
			r0, r1, r2 := d.indexOf(pc, sink.keep())
			eff := s.endDerived(sink, before)
			s.heard(snapIndexOf, at, r2, r0, r1, r2)
			if r2 || eff != 0 {
				a.IndexOf.reserve(int(s.room[snapIndexOf]))
				a.IndexOf.put(at, eff, r2, jitSnapAnsIndexOf{r0: r0, r1: r1, r2: r2})
			}
		}
		if asks&(1<<snapParseInt) != 0 {
			before := s.begin(sink)
			r0, r1 := d.parseInt(pc, nd, sink.keep())
			eff := s.endDerived(sink, before)
			s.heard(snapParseInt, at, r1, r0, r1)
			if r1 || eff != 0 {
				a.ParseInt.reserve(int(s.room[snapParseInt]))
				a.ParseInt.put(at, eff, r1, jitSnapAnsParseInt{r0: r0, r1: r1})
			}
		}
		if asks&(1<<snapBigString) != 0 {
			before := s.begin(sink)
			r0, r1 := d.bigString(pc, sink.keep())
			eff := s.endDerived(sink, before)
			s.heard(snapBigString, at, r1, r0, r1)
			if r1 || eff != 0 {
				a.BigString.reserve(int(s.room[snapBigString]))
				a.BigString.put(at, eff, r1, jitSnapAnsBigString{r0: r0, r1: r1})
			}
		}
	}
}

// callTarget is the function the call at the program's instruction pc --
// call_w's, call_method_w's or new_w's, or a call in tail position --
// last called, as its site's cache holds it, and its closure where it is
// one of register code's (jitRegFeedback's callTarget).
func (d *jitCallsData) callTarget(pc int) (*Object, *closure, bool) {
	w, _, ok := jitRegAt(d.p, d.rc, pc)
	if !ok || d.cl == nil {
		return nil, nil, false
	}
	switch jitRegCallOp(w.Op()) {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW:
	default:
		return nil, nil, false
	}
	calls := d.calls(int(w.C()))
	if int(w.C()) >= len(calls) || calls[w.C()].callee == nil {
		return nil, nil, false
	}
	k := &calls[w.C()]
	if k.kind&regCallThrough != 0 {
		// f.call(t, ...), whose cache the interpreter makes f's, entering f
		// one register on (regCallThrough): the call's function, in the
		// window, is the realm's Function.prototype.call -- taken for a
		// call of f, a leaf's check of f failed at every call
		// (hasOwnProperty.call(o, k)).
		return d.realm.callFn, nil, d.realm.callFn != nil
	}
	return k.callee, k.cl, true
}

// methodCall is the built-in the call at the program's instruction pc --
// call_w's, or call_method_w's where method says, including tail position
// -- last called, as its site's cache holds it, and its argument count
// (jitRegFeedback's methodCall).
func (d *jitCallsData) methodCall(pc int, method bool) (*Object, int, bool) {
	w, op, ok := jitRegAt(d.p, d.rc, pc)
	want, tail := bytecode.RCallW, bytecode.RCallWT
	if method {
		want, tail = bytecode.RCallMW, bytecode.RCallMWT
	}
	if !ok || op != ir.Host || !d.p.Code[pc].Window || w.Op() != want && w.Op() != tail {
		return nil, 0, false
	}
	if rpc := int(d.p.Origin[pc]); rpc < len(d.rc.Marks) && d.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
		return nil, 0, false
	}
	callee, cl, ok := d.callTarget(pc)
	if !ok || cl != nil {
		return nil, 0, false
	}
	return callee, int(w.X()), true
}

// charCode is CharCode's answer.
func (d *jitCallsData) charCode(pc int) bool {
	o, n, ok := d.methodCall(pc, true)
	return ok && n == 1 && d.realm.charCodeAt.IsObject() && o == d.realm.charCodeAt.Object()
}

// substringOf is which of the realm's String.prototype substring, slice,
// substr and charAt o is (jitSubstringOf).
func (c *jitRealmCallees) substringOf(o *Object) (kind int, ok bool) {
	for _, m := range [...]struct {
		v    Value
		kind int
	}{{c.substring, 0}, {c.slice, 0}, {c.substr, ssa.SubstrLength}, {c.charAt, ssa.SubstrAt}} {
		if m.v.IsObject() && m.v.Object() == o {
			return m.kind, true
		}
	}
	return 0, false
}

// substring is Substring's answer.
func (d *jitCallsData) substring(pc int) (uintptr, int, bool) {
	o, n, ok := d.methodCall(pc, true)
	if !ok {
		return 0, 0, false
	}
	kind, ok := d.realm.substringOf(o)
	if !ok || (kind == ssa.SubstrAt) != (n == 1) || n < 1 || n > 2 {
		return 0, 0, false
	}
	return uintptr(unsafe.Pointer(o)), kind, true
}

// fromCharCode is FromCharCode's answer.
func (d *jitCallsData) fromCharCode(pc int) (uintptr, bool) {
	o, n, ok := d.methodCall(pc, true)
	if !ok || n != 1 || !d.realm.fromCharCode.IsObject() || o != d.realm.fromCharCode.Object() {
		return 0, false
	}
	return uintptr(unsafe.Pointer(o)), true
}

// indexOf is IndexOf's answer, the search kept by k.
func (d *jitCallsData) indexOf(pc int, k *jitFeedback) (uintptr, int, bool) {
	o, n, ok := d.methodCall(pc, true)
	if !ok || n != 1 && n != 2 || o == nil {
		return 0, 0, false
	}
	kind := slices.Index(d.realm.searches[:], o)
	if kind < 0 {
		return 0, 0, false
	}
	k.holders = append(k.holders, o)
	return uintptr(unsafe.Pointer(o)), kind, true
}

// parseInt is ParseInt's answer, the function kept by k; nd says where the
// code built a site generic.
func (d *jitCallsData) parseInt(pc int, nd *jitNativeData, k *jitFeedback) (uintptr, bool) {
	o, n, ok := d.methodCall(pc, false)
	if !ok || n != 1 || o == nil || o != d.realm.parseInt || nd.generic(pc) {
		// Not where its argument was met not a number (jitRegCallMissed): a
		// call of Go there, which had been a host exit at every call --
		// parseInt('7').
		return 0, false
	}
	k.holders = append(k.holders, o)
	return uintptr(unsafe.Pointer(o)), true
}

// bigString is BigString's answer, the function kept by k.
func (d *jitCallsData) bigString(pc int, k *jitFeedback) (uintptr, bool) {
	o, n, ok := d.methodCall(pc, true)
	if !ok || n > 1 || o == nil || o != d.realm.bigToString {
		return 0, false
	}
	k.holders = append(k.holders, o)
	return uintptr(unsafe.Pointer(o)), true
}

// intrinsic is Intrinsic's answer, the function kept by k.
func (d *jitCallsData) intrinsic(pc int, k *jitFeedback) (ssa.Intrinsic, bool) {
	o, n, ok := d.methodCall(pc, true)
	plain := false
	if !ok {
		if o, n, ok = d.methodCall(pc, false); !ok {
			return ssa.Intrinsic{}, false
		}
		plain = true
	}
	if n > jitMathArgs {
		return ssa.Intrinsic{}, false
	}
	fd := o.fn()
	if fd == nil || fd.native == nil {
		return ssa.Intrinsic{}, false
	}
	if fd.mathOp == mathBigInt || fd.mathOp == mathAsIntN || fd.mathOp == mathAsUintN {
		// BigInt's that native code makes (bigint.go in ssa): BigInt(x) of
		// one argument, plain; BigInt.asUintN(n, x) or asIntN's whose width
		// n the window's register is given as a constant from 0 to 64
		// before the call, on the way to it alone (regConstOperand).
		in := ssa.Intrinsic{Callee: uintptr(unsafe.Pointer(o))}
		switch {
		case plain:
			if fd.mathOp != mathBigInt || n != 1 {
				return ssa.Intrinsic{}, false
			}
			in.Op, in.Plain = ssa.OpBigOfF64, true
		case n == 2 && fd.mathOp != mathBigInt:
			width, ok := regConstOperand(d.p, pc, d.p.Code[pc].Left.Slot+2)
			if !ok || width != math.Trunc(width) || width < 0 || width > 64 {
				return ssa.Intrinsic{}, false
			}
			in.Op, in.Fn = ssa.OpZextW, int(width)
			if fd.mathOp == mathAsIntN {
				in.Op = ssa.OpSextW
			}
		default:
			return ssa.Intrinsic{}, false
		}
		k.holders = append(k.holders, o)
		return in, true
	}
	if d.r != nil && n == 0 && o == d.realm.randomFn && d.realm.random == nil {
		// Made now, as the live question makes it.
		d.realm.random = d.r.mathRandom()
	}
	return d.realm.mathIntrinsic(o, n, plain, k)
}

// mathIntrinsic is the intrinsic of a call of o, the realm's Math
// function, of n arguments, plain or a method's (jitFeedback's
// mathIntrinsic), the function kept by k.
func (c *jitRealmCallees) mathIntrinsic(o *Object, n int, plain bool, k *jitFeedback) (ssa.Intrinsic, bool) {
	in := ssa.Intrinsic{Callee: uintptr(unsafe.Pointer(o)), Plain: plain}
	fd := o.fn()
	switch {
	case n == 0:
		// Only Math.random takes none: the next number of the runtime's
		// cache, which the code reads, made where a call's cache holds it
		// (jitCallsGather, mathRandom).
		if o != c.randomFn || c.random == nil {
			return ssa.Intrinsic{}, false
		}
		in.Op, in.Random = ssa.OpRandomF64, uintptr(unsafe.Pointer(c.random))
	case o == c.powFn && n == 2:
		in.Op = ssa.OpPowF64
	case o == c.atan2Fn && n == 2:
		in.Op, in.Go = ssa.OpMathCall, ssa.GoAtan2
	case o == c.hypotFn && n == 2:
		in.Op, in.Go = ssa.OpMathCall, ssa.GoHypot
	case fd == nil || fd.native == nil:
		return ssa.Intrinsic{}, false
	case fd.mathOp == mathMax:
		in.Op, in.Args = ssa.OpMaxF64, n
	case fd.mathOp == mathMin:
		in.Op, in.Args = ssa.OpMinF64, n
	case fd.mathOp == mathImul && n == 2:
		in.Op = ssa.OpMulI32
	case fd.mathOp == mathClz32 && n == 1:
		in.Op = ssa.OpClz32
	case fd.mathOp != 0 && int(fd.mathOp) < len(unaryMath) && n == 1:
		in.Op = jitUnaryMath(unaryMathName(fd.mathOp))
		if in.Op == ssa.OpMathCall {
			in.Go, in.Fn = ssa.GoMath, int(fd.mathOp)
		}
	default:
		return ssa.Intrinsic{}, false
	}
	k.holders = append(k.holders, o)
	return in, true
}

// jitCallSrc is what a call's native targets (jitCallCtx's callTargets,
// regCallSites) are derived from besides the function's code: what the
// code learned of its calls, the closure's call caches, what of the
// targets they name native callers' calls depend on, the targets' links,
// and the realm's built-ins -- the live state, on the runtime's goroutine
// (jitCallLive), or a snapshot's copy of it, which the worker derives from
// (jitCallsData). One implementation of the answer reads either.
type jitCallSrc interface {
	// learned is what the function's code learned at its exits, nil for
	// nothing: the live entry, or the snapshot's copy of its lists'
	// headers, which only grow (TestJITLearnedListsPersist), each cut to
	// its length so that an append makes a list of its own. An answer
	// appends to its nativeCalls and inlines as before: live, or, in the
	// copy, kept for the compile's finish to write (jitSnapFinishers). A
	// derivation reads of it only what no exit writes in place: the
	// lists, not callSites, settling, regLitReads or an element's exits.
	learned() *jitEntry
	// calls is the closure's call caches, as an ask of site finds them
	// (regCallsRestored).
	calls(site int) []regCallCache
	// target is what of o, cl's function object, whose register code is
	// rc, native callers' calls of it depend on that the heap holds
	// (jitCallFacts).
	target(o *Object, cl *closure, rc *regCode) jitCallFacts
	// reg is cl's register code, nil for none yet (closure's reg).
	reg(cl *closure) *regCode
	// link is the link native callers call cl's code rc through
	// (regLinkOf): made by the runtime's goroutine, a snapshot's from what
	// its gather made.
	link(cl *closure, rc *regCode) *jitLink
	// seenCallees is jitRegSeenCallees' answer for the method call w at
	// register pc rpc.
	seenCallees(rpc int, w bytecode.RInstr) []jitCallee
	// now is regNowTarget's answer for the call w at register pc rpc,
	// which the interpreter has not made yet: the function the binding
	// read before it holds now, a method's where method says.
	now(rpc int, w bytecode.RInstr, method bool) (*Object, *closure, *regCode)
	// construct is jitRegConstructTarget's answer for a construction with
	// o of argc arguments, with the index of o's own prototype
	// (jitCallConstruct).
	construct(o *Object, argc int) jitCallConstruct
	// poolRoom is the room of the arrays pool's are made with, which
	// a refill changes (abi.ObjectPool's Room).
	poolRoom(pool *abi.ObjectPool) uint64
	// allocHidden reports whether the learned construction x, the i-th
	// of the learning's native calls, is one the asking's NativeCalls
	// had not met where it looked for an array's pool (regAllocSite): one
	// a snapshot's gather learned at the construction's question, which
	// no Inline's there learned before it (jitTargetsData's hidden).
	allocHidden(i int, x *jitInline) bool
	// seed learns at the construction w at register pc rpc, of argc
	// arguments, which the code has learned no pool at, the function the
	// binding read before it holds now, its objects from a pool
	// (jitRegSeedConstruct): live, or nothing in a snapshot's copy, whose
	// gather learned it (jitTargetsData's seed).
	seed(rpc int, w bytecode.RInstr, argc int)
	// literal is the pool of the RegExp literal the call w at register
	// pc rpc gives its RegExp back to (regLiteralAt), made where it had
	// none; nil for none.
	literal(rpc int, w bytecode.RInstr) *abi.ObjectPool
	// genNext is what the method call at register pc rpc resumes natively
	// of the generators its code learned (regGenSteps): their closures and
	// the pool of its result objects, made where it had none.
	genNext(rpc int) ([]*closure, *abi.ObjectPool)
	// genMake is what a call at register pc rpc of o, a generator
	// function its code learned it calls, makes natively (jitGenMakeFact).
	genMake(rpc int, o *Object) jitGenMakeFact
	// superTarget is jitRegSuperTarget's answer for a super() of active's
	// of argc arguments.
	superTarget(active *Object, argc int) (p, skip *Object, cl *closure)
	// arrayProtos is what a push native code makes reads of the realm's
	// array prototypes (jitCallProtos), their shapes remembered.
	arrayProtos() jitCallProtos
	// copies is the runtime's cache of copied data properties' layouts
	// (jitState's copies), made where it has none.
	copies() uintptr
	// realm is the realm's built-ins a call is compared with.
	realm() jitCallRealm
	// keep is the feedback an answer's effects -- what the code keeps
	// alive, its links -- are appended to: the asked feedback's keep, or
	// a snapshot's sink's.
	keep() *jitFeedback
	// live is the asked feedback where the source is the live state, nil
	// in a snapshot's derivation: the answers that read the heap or make
	// what the runtime keeps -- a binding's value, a pool, register code
	// -- which a snapshot's gather asks on the runtime's goroutine.
	live() *jitRegFeedback
}

// jitCallFacts is what of a function object o, cl's, whose register code is
// rc, native callers' calls of it depend on that is not fixed when they
// are made: whether jitRegTarget's checks that no argument count decides
// pass (ok), whether o is an arrow, and whether it shares a derived
// constructor's this binding (sharesThis).
type jitCallFacts struct {
	ok, arrow, sharesThis bool
}

// callable is jitRegCallable's answer for a call of argc arguments of the
// function whose facts f are, cl's, of register code rc; quirks says
// whether the runtime runs with WithNodeQuirks.
func (f jitCallFacts) callable(cl *closure, rc *regCode, argc int, quirks bool) bool {
	return !isClassConstructorKind(cl.fn.Kind) && f.target(rc, argc, quirks)
}

// target is jitRegTarget's answer for argc arguments, of register code rc.
func (f jitCallFacts) target(rc *regCode, argc int, quirks bool) bool {
	return f.ok && !(rc.args && argc > abi.MaxGoArgs) && !(quirks && argc > int(rc.keep))
}

// jitCallRealm is the realm's built-ins a call's native targets are
// compared with: its Function.prototype.call and apply, its Array, its
// global object (nil where globalThis is none), and whether the runtime
// runs with WithNodeQuirks.
type jitCallRealm struct {
	// Nothing compares it: comparable, the type had an equality function
	// the compiler lays out among package vm's code.
	_                                  [0]func()
	callFn, applyFn, arrayCtor, global *Object
	quirks                             bool
	// realm is the runtime's realm, array its Array.prototype; genNextFn
	// and genIterFn its generators' next and [Symbol.iterator], and
	// genIteration and arrayIter the addresses of the runtime's caches
	// of how generators and arrays iterate.
	realm                   *Realm
	array                   *Object
	genNextFn, genIterFn    *Object
	genIteration, arrayIter uintptr
}

// jitGenMakeFact is what a call of a generator function makes natively of
// it (regGenMakeSite): the pool of its generators, the index of its own
// prototype, and its register code; ok where it may.
type jitGenMakeFact struct {
	pool       *abi.ObjectPool
	protoIndex int
	c          *regCode
	ok         bool
}

// jitCallProtos is the realm's Array.prototype and Object.prototype and
// their shapes, remembered, where a push native code makes may keep to
// them (regBuiltinSite): ok.
type jitCallProtos struct {
	ap, op           *Object
	apShape, opShape *shape
	ok               bool
}

// jitCallConstruct is what a construction with a function object, of a
// number of arguments, reads of it (jitRegConstructTarget): the closure
// whose code constructs, and the class it extends where that is its
// parent's (parent), ok where native code constructs with it; and the
// index of its own prototype property (protoIndex), which the
// construction reads.
type jitCallConstruct struct {
	cl         *closure
	parent     *Object
	protoIndex int
	ok         bool
}

// jitRegConstructOf is o's jitCallConstruct for argc arguments, now.
func (r *Runtime) jitRegConstructOf(o *Object, argc int) jitCallConstruct {
	cl, parent, ok := r.jitRegConstructTarget(o, argc)
	return jitCallConstruct{cl: cl, parent: parent, protoIndex: int(o.findOwn(atomPrototype)), ok: ok}
}

// jitCallCtx is a question of a call's native targets: the function's
// register code lowered (p, rc), its function and closure, and what the
// answer is derived from (src).
type jitCallCtx struct {
	// Nothing compares it: comparable, the type had an equality function
	// the compiler lays out among package vm's code.
	_   [0]func()
	p   *ir.Program
	rc  *bytecode.RegCode
	fn  *bytecode.Function
	cl  *closure
	src jitCallSrc
}

// callCtx is f's question of its calls' targets, of the live state.
func (f *jitRegFeedback) callCtx() jitCallCtx {
	return jitCallCtx{p: f.p, rc: f.rc, fn: f.fb.fn, cl: f.fb.cl, src: jitCallLive{f}}
}

// mustLive is the live feedback of q's source, for an answer that reads
// the heap, which a snapshot's gather asks on the runtime's goroutine: a
// derivation that comes to one is a gather's mistake.
func (q *jitCallCtx) mustLive() *jitRegFeedback {
	f := q.src.live()
	if f == nil {
		panic("jit: a snapshot's derivation of a call came to what its gather asks")
	}
	return f
}

// jitCallLive is the live state of f's function, as the runtime's
// goroutine reads it (jitCallSrc).
type jitCallLive struct{ f *jitRegFeedback }

func (l jitCallLive) learned() *jitEntry { return l.f.fb.e }

func (l jitCallLive) calls(site int) []regCallCache {
	return l.f.fb.r.regCallsRestored(l.f.fb.cl, site)
}

func (l jitCallLive) target(o *Object, cl *closure, rc *regCode) jitCallFacts {
	return l.f.fb.r.jitRegFactsOf(o, cl, rc)
}

func (l jitCallLive) reg(cl *closure) *regCode { return cl.reg }

func (l jitCallLive) link(cl *closure, rc *regCode) *jitLink {
	return l.f.fb.r.jitState().regLinkOf(cl, rc)
}

func (l jitCallLive) seenCallees(rpc int, w bytecode.RInstr) []jitCallee {
	return l.f.fb.r.jitRegSeenCallees(l.f.fb.cl, l.f.rc, rpc, w)
}

func (l jitCallLive) now(rpc int, w bytecode.RInstr, method bool) (*Object, *closure, *regCode) {
	return l.f.regNowTarget(rpc, w, method)
}

func (l jitCallLive) construct(o *Object, argc int) jitCallConstruct {
	return l.f.fb.r.jitRegConstructOf(o, argc)
}

func (l jitCallLive) poolRoom(pool *abi.ObjectPool) uint64 { return pool.Room }

func (l jitCallLive) allocHidden(int, *jitInline) bool { return false }

func (l jitCallLive) seed(rpc int, w bytecode.RInstr, argc int) {
	f := l.f
	if o := f.regConstructorNow(rpc, int(w.A())); o != nil {
		f.fb.r.jitRegSeedConstruct(f.fb.e, int32(rpc), o, argc)
	}
}

func (l jitCallLive) literal(rpc int, w bytecode.RInstr) *abi.ObjectPool {
	return l.f.regLiteralAt(rpc, w)
}

func (l jitCallLive) genNext(rpc int) ([]*closure, *abi.ObjectPool) {
	return l.f.regGenNextOf(rpc)
}

func (l jitCallLive) genMake(rpc int, o *Object) jitGenMakeFact {
	return l.f.regGenMakeOf(rpc, o)
}

func (l jitCallLive) superTarget(active *Object, argc int) (*Object, *Object, *closure) {
	return l.f.fb.r.jitRegSuperTarget(active, argc)
}

func (l jitCallLive) arrayProtos() jitCallProtos { return l.f.fb.r.jitArrayProtos() }

func (l jitCallLive) copies() uintptr { return l.f.fb.r.jitCopies() }

func (l jitCallLive) realm() jitCallRealm { return l.f.fb.r.jitCallRealmNow() }

// jitArrayProtos is r's jitCallProtos, now, their shapes remembered.
func (r *Runtime) jitArrayProtos() jitCallProtos {
	ap, op := r.proto.array, r.proto.object
	if ap.shape == nil || op.shape == nil || ap.shape.loose || op.shape.loose || ap.proto() != op || op.proto() != nil ||
		!ap.noIndexKeys() || !op.noIndexKeys() {
		return jitCallProtos{ap: ap, op: op}
	}
	return jitCallProtos{ap: ap, op: op, apShape: remember(ap.shape), opShape: remember(op.shape), ok: true}
}

// jitCopies is the runtime's cache of copied data properties' layouts,
// made at its first ask (regWholeSite's copy_data_props).
func (r *Runtime) jitCopies() uintptr {
	if r.jit.copies == nil {
		r.jit.copies = new([jitCopyEntries]jitCopyEntry)
	}
	return uintptr(unsafe.Pointer(r.jit.copies))
}

func (l jitCallLive) keep() *jitFeedback { return l.f.fb.keep() }

func (l jitCallLive) live() *jitRegFeedback { return l.f }

// jitCallRealmNow is r's built-ins a call is compared with, now.
func (r *Runtime) jitCallRealmNow() jitCallRealm {
	c := jitCallRealm{callFn: r.callFn, applyFn: r.applyFn, arrayCtor: r.proto.arrayCtor, quirks: r.nodeQuirks,
		realm: r.Realm, array: r.proto.array, genNextFn: r.genNextFn, genIterFn: r.genIterFn,
		genIteration: uintptr(unsafe.Pointer(&r.genIteration)), arrayIter: uintptr(unsafe.Pointer(&r.jit.arrayIter))}
	if r.globalThis.IsObject() {
		c.global = r.globalThis.Object()
	}
	return c
}

// jitRegFactsOf is o's facts (jitCallFacts), cl's function object, whose
// register code is rc, as they are now.
func (r *Runtime) jitRegFactsOf(o *Object, cl *closure, rc *regCode) jitCallFacts {
	fd := o.fn()
	if fd == nil {
		return jitCallFacts{}
	}
	return jitCallFacts{ok: r.jitRegTargetFree(fd, cl, rc), arrow: fd.arrow, sharesThis: fd.extra != nil && fd.extra.lexThisRef != nil}
}
