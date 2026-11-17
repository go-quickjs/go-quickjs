package vm

import (
	"fmt"
	"strings"
	"weak"
)

// RegCacheDigest describes the inline caches the JIT reads of every
// function reachable from r's global object -- each property site's entry
// and the shapes it met before, each global site's epoch against the
// runtime's, each call site's kind and callee -- in an order and a form
// that two runtimes running one program alike give alike: what a test
// compares between the native handlers and the Go table's
// (TestRegNativeCaches). It is a test's, and slow.
func RegCacheDigest(r *Runtime) string {
	var b strings.Builder
	seenObj := map[*Object]bool{}
	seenCode := map[*closureCode]bool{}
	var stack []*Object
	push := func(v Value) {
		if v.IsObject() {
			if o := v.Object(); o != nil && !seenObj[o] {
				seenObj[o] = true
				stack = append(stack, o)
			}
		}
	}
	push(r.globalThis)
	for len(stack) > 0 {
		o := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if p := o.proto(); p != nil {
			push(Obj(p))
		}
		for _, v := range o.vals() {
			push(v)
		}
		if o.flags&objHasExt != 0 {
			for _, v := range o.extAt().elems {
				push(v)
			}
		}
		fd := o.fn()
		if fd == nil || fd.closure == nil {
			continue
		}
		cl := fd.closure
		for _, u := range cl.upvalues {
			if u != nil && u.slot != nil {
				push(u.get())
			}
		}
		cc := cl.closureCode
		if cc == nil || seenCode[cc] || cl.fn == nil {
			continue
		}
		seenCode[cc] = true
		fmt.Fprintf(&b, "%s:%d\n", cl.fn.Name, len(cc.ic))
		for i := range cc.ic {
			s := &cc.ic[i]
			n := 0
			if s.poly != nil {
				n = int(s.poly.n)
			}
			fmt.Fprintf(&b, " ic%d own=%v shape=%v next=%v p1=%v p2=%v cell=%v idx=%d get=%v fills=%d poly=%d\n", i,
				s.ownShape != nil, s.shape != nil, s.next != nil, s.p1 != nil, s.p2 != nil, s.cell != nil, s.idx, s.getter, s.fills, n)
		}
		for i, ep := range cc.regEpochs {
			fmt.Fprintf(&b, " g%d %v\n", i, ep == r.shapes.globalEpoch+1)
		}
		for i := range cc.regCalls {
			e := &cc.regCalls[i]
			callee := ""
			if e.callee != nil {
				if f := e.callee.fn(); f != nil && f.closure != nil && f.closure.fn != nil {
					callee = f.closure.fn.Name
				} else {
					callee = "?"
				}
			}
			fmt.Fprintf(&b, " c%d kind=%d callee=%q\n", i, e.kind, callee)
		}
	}
	return b.String()
}

// regCallKeep is what the register code's call caches (closureCode's
// regCalls) are let go of by at each turn's end (releaseRegCalls): the
// templates whose caches the running turn filled, each once
// (regCallsHeld). Only those: a turn's end that went through every
// template the runtime had made caches for had cost each turn as much as
// the program had templates, and as many again collected but not yet
// found gone -- a host running a script a turn, each its own template,
// spent most of its time there, its turns' ends quadratic in their
// number until the collector came.
type regCallKeep struct {
	held []*closureCode
}

// regCallSeen is a call site's cache as a turn's end let it go of: the
// function it called, held weakly, and how the site entered it (its kind,
// without regCallMarks), which is all a cache entry is made from again
// (regCallsRestored).
type regCallSeen struct {
	callee weak.Pointer[Object]
	kind   uint8
}

// newRegCalls makes cc's call caches for its register code's n call
// sites. Out of line, where the caches are made, once a template: the
// calls that read them pay nothing for it.
//
//go:noinline
func (r *Runtime) newRegCalls(cc *closureCode, n int) []regCallCache {
	return make([]regCallCache, n)
}

// regCallsHold puts cc, whose call caches a call is about to fill, on the
// list of templates whose caches the turn's end lets go (releaseRegCalls),
// unless it is on it (regCallsHeld). The paths that fill a cache, misses
// all, ask it; the calls that read one pay nothing for it.
//
//go:noinline
func (r *Runtime) regCallsHold(cc *closureCode) {
	cc.regCallsHeld = true
	k := &r.weakMapRegistry().calls
	k.held = append(k.held, cc)
}

// releaseRegCalls empties, as a turn ends with no frame running, the call
// caches of the templates the turn filled (regCallsHold), keeping what each
// site called as a weak pointer (regCallSeen) on its template. An entry
// holds the function its site called last, its closure and its this: one
// of a template the program's code shares -- a library's forEach -- had
// kept the closure a finished program passed it, and with it everything
// the closure captured, the program's whole graph, which every collection
// after marked again: AWFY's DeltaBlue's, 17 MB, all through Havlak. V8's
// call feedback holds its target weakly; here the interpreter's handlers
// read the entries as they are, and so the turn's end lets them go, as it
// does the stack's slots past the frames (endTurn), and keeps their
// targets weakly: the next call at a site fills its entry again, and the
// JIT, which reads the entries as its call feedback, finds a callee still
// alive where it was (regCallsRestored). What a turn's end keeps is the
// template's, and goes with it.
func (r *Runtime) releaseRegCalls() {
	if r.weakMaps == nil || len(r.weakMaps.calls.held) == 0 {
		return
	}
	k := &r.weakMaps.calls
	for _, cc := range k.held {
		cc.regCallsHeld = false
		for i := range cc.regCalls {
			e := &cc.regCalls[i]
			if e.callee == nil {
				continue
			}
			if len(cc.regSeen) != len(cc.regCalls) {
				cc.regSeen = make([]regCallSeen, len(cc.regCalls))
			}
			cc.regSeen[i] = regCallSeen{callee: weak.Make(e.callee), kind: e.kind &^ regCallMarks}
		}
		clear(cc.regCalls)
	}
	clear(k.held)
	k.held = k.held[:0]
}

// regCallsRestored is cl's call caches for the JIT to read as its call
// feedback, the entry of site i -- or, for i < 0, every entry -- made
// again, where empty, from what a turn's end kept of it (regSeen) if the
// function it called is alive still: a function warmed in one turn and
// compiled in the next is compiled for its calls' targets, as V8's weakly
// held call feedback has them while they live. Only where the entry is
// empty, and a turn's end kept some: an entry read as filled costs nothing
// more. The entries made again are the turn's, which its end lets go.
func (r *Runtime) regCallsRestored(cl *closure, i int) []regCallCache {
	calls := cl.regCalls
	if cl.regSeen == nil || i >= len(calls) || i >= 0 && calls[i].callee != nil {
		return calls
	}
	seen := cl.regSeen
	cl.regSeen = nil
	if !cl.regCallsHeld {
		r.regCallsHold(cl.closureCode)
	}
	for j, x := range seen {
		if j >= len(calls) || calls[j].callee != nil {
			continue
		}
		o := x.callee.Value()
		if o == nil {
			continue
		}
		fd := o.fn()
		switch x.kind {
		case regCallNative, regCallMarked:
			calls[j] = regCallCache{callee: o, kind: x.kind}
		case regCallGen:
			if fd != nil && fd.closure != nil {
				calls[j] = regCallCache{callee: o, cl: fd.closure, this: Undefined, kind: regCallGen}
			}
		case regCallThis, regCallObjThis, regCallEnter:
			if fd == nil || fd.closure == nil || fd.bound {
				continue
			}
			if ecl, rc, kind, this, enter := r.regCallKind(fd, x.kind != regCallThis, nil); kind != 0 && enter {
				regCacheCallee(&calls[j], o, ecl, rc, this, kind, nil)
			}
		}
	}
	return calls
}
