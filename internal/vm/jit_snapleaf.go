//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// The leaves' family of a snapshot's derived methods (jit_snapderive.go):
// Leaf, the built-in a call calls that native code calls without a frame,
// derived on the worker from the call caches copied (jitCallsData's
// callTarget) and the realm's built-ins the gather read; and the questions
// of a leaf's call, CreatePool, of the pool Object.create's leaf fills,
// which the gather makes by register pc where the copies name
// Object.create, as the asking made it at a leaf's call, and ExecReuse,
// whether exec's array may be made again, of the register code and the
// program. The feedback on the spot answers so of the live state: one
// implementation.

// jitLeafSrc is what Leaf's answer is derived from besides the function's
// code: the call caches (callTarget), the realm and its built-ins the leaf
// is compared with, regexp_lit's methods as the realm has them
// (regexpLitMethod), and the sites whose speculation failed (regLitFed).
type jitLeafSrc struct {
	p          *ir.Program
	rc         *bytecode.RegCode
	fn         *bytecode.Function
	calls      *jitCallsData
	realm      *Realm
	strReplace *Object
	lit        [2]*Object
	learned    bool
	failed     []int32
}

// leafSrc is f's, live.
func (f *jitRegFeedback) leafSrc() jitLeafSrc {
	r := f.fb.r
	s := jitLeafSrc{p: f.p, rc: f.rc, fn: f.fb.fn, calls: f.callsData(), realm: r.Realm, strReplace: r.strReplaceFn,
		lit: [2]*Object{r.regexpLitMethod(false), r.regexpLitMethod(true)}}
	if e := f.fb.e; e != nil {
		s.learned, s.failed = true, e.failed
	}
	return s
}

// litFed is regLitFed's of register pc rpc.
func (s *jitLeafSrc) litFed(rpc int) bool {
	return !s.learned || !slices.Contains(s.failed, int32(rpc))
}

// jitLeafOf is Leaf's answer at the program's instruction pc of src's
// function, the callee kept in k's holders: see Leaf.
func jitLeafOf(src *jitLeafSrc, pc int, k *jitFeedback) (uintptr, bool) {
	w, op, ok := jitRegAt(src.p, src.rc, pc)
	if ok && op == ir.Host && src.p.Code[pc].Window && w.Op() == bytecode.RLit {
		// regexp_lit's call: the realm's exec or test, read where it is the
		// built-in (regexpLitMethod), a leaf; the literal's RegExp, from
		// the pool, nothing keeps.
		m := src.lit[0]
		if w.X()&bytecode.RLitTest != 0 {
			m = src.lit[1]
		}
		if m == nil || !src.litFed(int(src.p.Origin[pc])) {
			return 0, false
		}
		k.holders = append(k.holders, m)
		return uintptr(unsafe.Pointer(m)), true
	}
	call := jitRegCallOp(w.Op())
	if !ok || op != ir.Host || !src.p.Code[pc].Window || call != bytecode.RCallW && call != bytecode.RCallMW {
		return 0, false
	}
	var marks uint8
	if rpc := int(src.p.Origin[pc]); rpc < len(src.rc.Marks) {
		marks = src.rc.Marks[rpc]
	}
	if marks&bytecode.RMarkTail != 0 {
		return 0, false
	}
	callee, cl, ok := src.calls.callTarget(pc)
	if !ok || cl != nil {
		return 0, false
	}
	fd := callee.fn()
	if fd == nil || fd.native == nil || fd.bound || fd.realm != nil && fd.realm != src.realm {
		return 0, false
	}
	// RegExp's exec and test too -- a literal's call among them
	// (RMarkLitRecv), its RegExp then from the pool (regexp_lit's lowering,
	// compile's regexpLit), one whose value nothing reads but its truth
	// (RMarkDead), which the leaf makes whole, and an exec whose array the
	// interpreter may make again in the memory of the last (RMarkReuse),
	// which the leaf makes anew: the old array, which only the local the
	// new one replaces could reach, the interpreter's next call may make
	// again, its slots' check finding the local holds it no more.
	regexp := fd.elemOp == elemRegExpExec || fd.elemOp == elemRegExpTest
	// String.prototype.replace of two arguments, s.replace(re, ""), whose
	// leaf checks at each call that they are a RegExp it replaces with
	// directly and a string (jitReplaceLeaf) -- a literal's too, which the
	// call makes its first argument (RMarkLitArg), natively from the
	// literal's pool (lowerreg's literal before the call), the leaf giving
	// it back (ssa.CallSite's Give, jitLeafForm's giveArg), where its
	// template is a string constant: a function's, which the leaf's check
	// refuses, would leave native code at every call the call of Go makes
	// now.
	replace := callee == src.strReplace && call == bytecode.RCallMW && w.X() == 2 &&
		(marks&bytecode.RMarkLitArg == 0 || jitRegStringBefore(src.fn, src.rc, int(src.p.Origin[pc]), int(w.A())+3))
	// Array.prototype.push of other than one value: one value's push native
	// code appends itself.
	push := fd.elemOp == elemPush && w.X() != 1
	if !jitLeafOp(fd.elemOp) && !regexp && !replace && !push {
		return 0, false
	}
	k.holders = append(k.holders, callee)
	return uintptr(unsafe.Pointer(callee)), true
}

// jitRegStringBefore is regStringBefore's, of fn's register code rc.
func jitRegStringBefore(fn *bytecode.Function, rc *bytecode.RegCode, rpc, reg int) bool {
	if rpc < 1 || rpc > len(rc.Code) {
		return false
	}
	w := rc.Code[rpc-1]
	switch {
	case int(w.A()) != reg:
		return false
	case w.Op() == bytecode.RLoadEmpty:
		return true
	case w.Op() == bytecode.RLoadConst:
		consts := regConsts(fn, rc)
		return int(w.BC()) < len(consts) && consts[w.BC()].Kind == bytecode.ConstString
	}
	return false
}

// jitLeafData is a snapshot's copy of what the family's answers are made
// of (jitLeafSrc's, the calls' copy the calls' family took), the pools
// CreatePool's asking made, by register pc, with their effects, and what
// ExecReuse reads of the closure's register code.
type jitLeafData struct {
	gathered   bool
	realm      *Realm
	strReplace *Object
	execFn     *Object
	lit        [2]*Object
	learned    bool
	failed     []int32
	creates    []jitLeafCreate
	// reg is the closure's register code, own whether it is its
	// function's (jitReuseOwn), as the gather found them.
	reg *regCode
	own bool
}

// jitLeafCreate is CreatePool's answer at the calls of register pc rpc of
// Object.create, fn, its pool, and its effect's index past 0.
type jitLeafCreate struct {
	// Nothing compares it: comparable, the type had an equality function
	// the compiler lays out among package vm's code.
	_    [0]func()
	rpc  int32
	fn   *Object
	pool uintptr
	eff  int32
	// bound marks one a Leaf answered at a PC of (deriveLeaf).
	bound bool
}

func init() {
	jitSnapDerived |= 1<<snapLeaf | 1<<snapCreatePool | 1<<snapExecReuse
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherLeaf)
	jitSnapDerivers[jitDeriveCopies] = append(jitSnapDerivers[jitDeriveCopies], deriveLeaf)
}

// gatherLeaf copies, for s, the snapshot of fb, what its leaves' answers
// are made of, of the register code: the realm's built-ins, the failed
// sites' list's header (it only grows), and the closure's register code;
// makes the pool of each call the copies name Object.create at, as
// CreatePool's asking makes it, and names the copies' built-ins for the
// snapshot to ask the kinds of (jitSnapData's leaves).
func gatherLeaf(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.leaf
	r, e := fb.fb.r, fb.fb.e
	*d = jitLeafData{gathered: true, realm: r.Realm, strReplace: r.strReplaceFn, execFn: r.regexpExecFn,
		lit: [2]*Object{r.regexpLitMethod(false), r.regexpLitMethod(true)}}
	if e != nil {
		d.learned, d.failed = true, slices.Clip(e.failed)
	}
	cl := fb.fb.cl
	if cl == nil {
		return
	}
	d.reg = cl.reg
	d.own = d.reg != nil && d.reg.rc == fb.rc && jitReuseOwn[struct{}](d.reg)
	for _, m := range d.lit {
		if m != nil {
			s.root.d.leaves = append(s.root.d.leaves, uintptr(unsafe.Pointer(m)))
		}
	}
	calls := s.d.calls.copied
	for rpc, w := range fb.rc.Code {
		switch jitRegCallOp(w.Op()) {
		case bytecode.RCallW, bytecode.RCallMW:
		default:
			continue
		}
		if rpc < len(fb.rc.Marks) && fb.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
			// In tail position: no leaf's (Leaf).
			continue
		}
		site := int(w.C())
		if site >= len(calls) {
			continue
		}
		k := &calls[site]
		callee := k.callee
		if k.kind&regCallThrough != 0 {
			callee = s.d.calls.realm.callFn
		} else if k.cl != nil {
			continue
		}
		if callee == nil {
			continue
		}
		fd := callee.fn()
		if fd == nil || fd.native == nil || fd.bound || fd.realm != nil && fd.realm != d.realm {
			continue
		}
		// A leaf's kind the build may ask (LeafKind).
		s.root.d.leaves = append(s.root.d.leaves, uintptr(unsafe.Pointer(callee)))
		if fd.elemOp != elemObjCreate || e == nil || k.kind&regCallThrough != 0 {
			continue
		}
		// Object.create's: its pool, made as the asking at the leaf's call
		// makes it (CreatePool), its effect taken back.
		before := s.begin(fb.fb)
		pool := jitCreatePool[struct{}](e, rpc)
		fb.fb.keep().pools = append(fb.fb.keep().pools, pool)
		eff := s.end(fb.fb, before)
		s.root.d.pools = append(s.root.d.pools, pool)
		d.creates = append(d.creates, jitLeafCreate{rpc: int32(rpc), fn: callee, pool: uintptr(unsafe.Pointer(pool)), eff: eff})
	}
}

// deriveLeaf puts s's answers of Leaf at every PC whose op asks it, and of
// CreatePool and ExecReuse at each PC Leaf answers, as the asking asked
// them (askLeafCalls), derived from what the gather took.
func deriveLeaf(s *jitSnapshot) {
	d := &s.d.leaf
	if !d.gathered {
		return
	}
	src := jitLeafSrc{p: s.p, rc: s.rc, fn: s.fb.fn, calls: &s.d.calls, realm: d.realm, strReplace: d.strReplace,
		lit: d.lit, learned: d.learned, failed: d.failed}
	sink := s.sinkOf()
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapLeaf) == 0 {
			continue
		}
		before := s.begin(sink)
		r0, r1 := jitLeafOf(&src, pc, sink.keep())
		eff := s.endDerived(sink, before)
		s.heard(snapLeaf, int64(pc), r1, r0, r1)
		if r1 || eff != 0 {
			s.a.Leaf.reserve(int(s.room[snapLeaf]))
			s.a.Leaf.put(int64(pc), eff, r1, jitSnapAnsLeaf{r0: r0, r1: r1})
		}
		if !r1 {
			continue
		}
		var pool uintptr
		var peff int32
		if d.learned && pc < len(s.p.Origin) {
			rpc := int32(s.p.Origin[pc])
			for i := range d.creates {
				if c := &d.creates[i]; c.rpc == rpc && uintptr(unsafe.Pointer(c.fn)) == r0 {
					pool, peff, c.bound = c.pool, c.eff, true
					break
				}
			}
		}
		s.heard(snapCreatePool, int64(pc), true, pool)
		s.a.CreatePool.put(int64(pc), peff, true, jitSnapAnsCreatePool{r0: pool})
		reuse := jitExecReuseOf(d.reg, d.own, s.p, d.execFn, pc, r0)
		s.heard(snapExecReuse, int64(pc), true, reuse)
		s.a.ExecReuse.put(int64(pc), 0, true, jitSnapAnsExecReuse{r0: reuse})
	}
}
