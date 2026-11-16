package vm

import (
	"fmt"
	"strings"
)

// RegCacheDigest describes the inline caches the JIT reads of every
// function reachable from r's global object -- each property site's entry
// and the shapes it met before, each global site's epoch against the
// runtime's, each call site's kind and callee -- in an order and a form
// that two runtimes running one program alike give alike: what a test
// compares between the native handlers and the Go table's
// (TestRegNativeCaches in internal/v8bench). It is a test's, and slow.
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
