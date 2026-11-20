package vm

import (
	"os"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The link of typed code (docs/register-interpreter.md, "The typed
// table"): a closure of a function that has typed code -- a function of a
// validated asm.js module, or the call path's prototype's twin -- given it
// where what the code reads of its upvalues is what the validator took it
// for. A module's body runs as the ordinary code it also is, every getter,
// valueOf and constructor of its imports as in plain JavaScript; its
// functions' bindings are never assigned after it, which validation
// forbids, so what a closure's upvalues hold at its first call is what
// they hold for good, but for its variables, numbers, and its heap's
// buffer, whose bytes may be detached (asmHeap). Only the first closure of
// a template is linked: the typed code is the template's, its heap its
// closure's.

// regTypedDefault is whether a runtime links typed code (Runtime's
// regTyped): QJS_NOTYPED set in the environment, read once, leaves every
// function to its ordinary code, to compare the two and as an escape
// hatch, as QJS_NONATIVE does the native handlers; a test chooses for
// each runtime (testConfig).
var regTypedDefault = os.Getenv("QJS_NOTYPED") == ""

// regTypedOf is closure cl's typed code, linked at its template's first
// call where it has some (asmLink), or nil: where the runtime runs the JIT,
// whose code is compiled from the ordinary code, and for a closure of the
// template other than the one linked.
func (r *Runtime) regTypedOf(cl *closure) *regCode {
	t := cl.regTyped
	if t == nil {
		if cl.asmTried {
			return nil
		}
		cl.asmTried = true
		if t = r.asmLink(cl); t == nil {
			return nil
		}
	}
	if c := t.typedExt().asmCl; c != nil && c != cl {
		return nil
	}
	return t
}

// asmViewKinds are the element types of asm.js's views, by
// bytecode.AsmUp.View (asmjs.View).
var asmViewKinds = [...]elemType{elemInt8, elemUint8, elemInt16, elemUint16, elemInt32, elemUint32, elemFloat32, elemFloat64}

// asmMathOps are funcData.mathOp of the Math functions typed code inlines,
// by name.
var asmMathOps = map[string]uint8{"imul": mathImul, "pow": mathPow, "atan2": mathAtan2, "min": mathMin, "max": mathMax}

// The functions of one number's, by their names' order (unaryMathNames),
// and clz32's: in an init, not a closure, which would move the closures
// after it.
func init() {
	asmMathOps["clz32"] = mathClz32
	for i, name := range unaryMathNames {
		asmMathOps[name] = uint8(i + 1)
	}
}

// asmLink links cl, of a template whose function has typed code and
// whose ordinary code cl.reg is, and gives its template the typed code, or
// gives nil where the runtime runs the JIT or cl's upvalues are not what
// the code takes them for.
func (r *Runtime) asmLink(cl *closure) *regCode {
	g := cl.reg
	if !r.regTyped || g == nil || !g.wide || !cl.fn.HasRegTyped() {
		return nil
	}
	rc := cl.fn.RegTypedCode()
	if rc == nil || rc.CallSites < g.rc.CallSites {
		return nil
	}
	t := newRegTypedCode(cl.fn, rc, g)
	if rc.Asm != nil {
		t.typedExt().asmCl = cl
		var heap *asmHeap
		var buf *Object
		for k, up := range rc.Asm {
			if up.Kind == bytecode.AsmUpNone {
				continue
			}
			if k >= len(cl.upvalues) {
				return nil
			}
			v := cl.upvalues[k].get()
			switch up.Kind {
			case bytecode.AsmUpView:
				view, ok := asmView(v, up.View)
				if !ok || buf != nil && view.buffer != buf {
					return nil
				}
				if heap == nil {
					buf = view.buffer
					heap = &asmHeap{view: view, size: uintptr(elemInfos[view.kind].size)}
				}
			case bytecode.AsmUpMath:
				fd := (*funcData)(nil)
				if v.IsObject() {
					fd = v.Object().fn()
				}
				if fd == nil || fd.native == nil || fd.bound || fd.mathOp != asmMathOps[up.Name] || fd.mathOp == 0 ||
					fd.realm != nil && fd.realm != cl.realm {
					return nil
				}
			case bytecode.AsmUpConst, bytecode.AsmUpVarI, bytecode.AsmUpVarF:
				if !v.IsNumber() {
					return nil
				}
			case bytecode.AsmUpTable:
				if !v.IsObject() || v.Object().class != ClassArray || len(v.Object().elemsOf()) != up.Len {
					return nil
				}
			}
		}
		if heap != nil {
			heap.at(r)
		}
		t.typedExt().heap = heap
	}
	cl.regTyped = t
	// The call caches of both codes, the typed code's sites numbered past
	// the ordinary code's.
	if cl.regCalls == nil && rc.CallSites > 0 {
		cl.regCalls = r.newRegCalls(cl.closureCode, rc.CallSites)
	} else if len(cl.regCalls) < rc.CallSites {
		calls := make([]regCallCache, rc.CallSites)
		copy(calls, cl.regCalls)
		cl.regCalls = calls
	}
	if jitBuilt && r.jitOn() {
		// Compiled natively, from the typed code (jitTypedLinked).
		r.jitTypedLinked(cl, t)
	}
	return t
}

// asmView is v's view data where v is a view of the kind asm.js's view
// kind is, over the whole of an ArrayBuffer that is neither shared, nor
// resizable, nor immutable, nor detached.
func asmView(v Value, kind uint8) (*typedArrayData, bool) {
	if !v.IsObject() || int(kind) >= len(asmViewKinds) {
		return nil, false
	}
	o := v.Object()
	if o.class != ClassTypedArray {
		return nil, false
	}
	t, ok := o.dataOf().(*typedArrayData)
	if !ok || t.kind != asmViewKinds[kind] || t.byteOffset != 0 || t.tracking || t.buffer == nil {
		return nil, false
	}
	b, ok := t.buffer.dataOf().(*arrayBufferData)
	if !ok || b.shared || b.resizable || b.immutable || b.detached || b.block != nil ||
		t.fixedLength*elemInfos[t.kind].size != len(b.bytes) {
		return nil, false
	}
	return t, true
}
