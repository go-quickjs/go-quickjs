//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

var (
	_ ssa.Feedback              = (*jitRegFeedback)(nil)
	_ ssa.ConstantFeedback      = (*jitRegFeedback)(nil)
	_ ssa.StringContentFeedback = (*jitRegFeedback)(nil)
	_ ssa.BigIntConstFeedback   = (*jitRegFeedback)(nil)
	_ ssa.CalledClosureFeedback = (*jitRegFeedback)(nil)
)

// TestJITRegFeedback asks register code's feedback (jitRegFeedback) about
// a function the register interpreter has run, by the program LowerReg
// makes of it: each property read, method read and store, the fused
// update's read and store among them, is the shape the receiver has, its
// index the property's; each global read and write the global object's
// slot of the name; each call its argument count and the function it
// called, from the call's cache.
func TestJITRegFeedback(t *testing.T) {
	src := `
var total = 0;
function Point(x, y) { this.x = x; this.y = y }
Point.prototype.norm = function () { return this.x * this.x + this.y * this.y };
function helper(v) { return v + 1 }
function work(p, n) {
	for (var i = 0; i < n; i++) {
		p.x += 1;
		total = total + p.norm() + helper(p.y);
	}
	var tag = n > 1000 ? "tag" : "";
	var big = n > 1000 ? 7n : 0;
	return p.x + p.y + tag.length + Number(big);
}
var pt = new Point(1, 2);
for (var k = 0; k < 20; k++) work(pt, 5);
`
	compiler.SetRegisterForm(true)
	prog := compileForTest(t, src)
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	defer SetRegTier(bytecode.RegDefault())
	defer SetJITRegTier(SetJITRegTier(true))
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(prog); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("work")).value().Object().fn().closure
	if cl.fn.RegCode() == nil {
		t.Fatal("work has no register code")
	}
	p, err := jitcompile.LowerReg(cl.fn.RegCode(), cl.fn)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	f := r.jitRegFeedbackFor(cl, p)
	// The SSA builder takes the program with its feedback: the sites
	// below are what it is compiled for.
	if _, err := ssa.BuildWith(p, f); err != nil {
		t.Fatalf("the SSA build of work's register program: %v", err)
	}
	pt := r.global.getOwn(r.atoms.intern("pt")).value().Object()
	shape := uintptr(unsafe.Pointer(pt.shape))
	globals := r.global
	var reads, writes, gets, sets, calls, natives, strs, bigs int
	for pc, in := range p.Code {
		w := cl.fn.RegCode().Code[p.Origin[pc]]
		switch in.Op {
		case ir.PropertyRead, ir.ReferenceRead, ir.PropertyWrite:
			site, ok := f.Property(pc)
			if !ok {
				t.Fatalf("pc %d (%s): no property site", pc, cl.fn.RegCode().Format(int(p.Origin[pc])))
			}
			name := r.atoms.name(Atom(site.Key))
			switch {
			case name == "norm":
				// Found on the prototype: a holder's.
				if site.Holders[0].Object == 0 {
					t.Errorf("pc %d: norm's read has no holder: %+v", pc, site)
				}
			case name == "x" || name == "y":
				if site.Shape != shape || int(site.Index) != pt.findOwnIndexForTest(r, name) {
					t.Errorf("pc %d: %s's site %+v, want the point's shape and the property's index", pc, name, site)
				}
			default:
				t.Errorf("pc %d: a site of %q", pc, name)
			}
			if in.Op == ir.PropertyWrite {
				writes++
			} else {
				reads++
			}
		case ir.BindingRead, ir.BindingWrite:
			site, ok := f.Global(pc)
			if !ok {
				t.Fatalf("pc %d (%s): no global site", pc, cl.fn.RegCode().Format(int(p.Origin[pc])))
			}
			name := r.atoms.name(Atom(site.Key))
			if i := globals.findOwn(r.atoms.intern(name)); i < 0 || site.Index != i {
				t.Errorf("pc %d: %s's global site %+v, want index %d", pc, name, site, i)
			}
			if in.Op == ir.BindingWrite {
				sets++
			} else {
				gets++
			}
		case ir.Host:
			argc, method, ok := f.CallShape(pc)
			if w.Op() != bytecode.RCallW && w.Op() != bytecode.RCallMW {
				if ok {
					t.Errorf("pc %d: a call's shape for %s", pc, cl.fn.RegCode().Format(int(p.Origin[pc])))
				}
				continue
			}
			callee, ccl, known := f.callTarget(pc)
			if known && ccl == nil && callee != nil && !method {
				// Number(big), a built-in's.
				natives++
				continue
			}
			if !ok || argc != int(w.X()) || method != (w.Op() == bytecode.RCallMW) || !known || ccl == nil || callee == nil {
				t.Errorf("pc %d: call %s: shape %d %v %v, target %v", pc, cl.fn.RegCode().Format(int(p.Origin[pc])), argc, method, ok, known)
				continue
			}
			want := "helper"
			if method {
				want = ""
			}
			if ccl.fn.Name != want {
				t.Errorf("pc %d: the call's target is %q, want %q", pc, ccl.fn.Name, want)
			}
			calls++
		}
		if in.Op == ir.StringConst {
			if s, ok := f.StringContent(pc); !ok || s != "tag" && s != "" {
				t.Errorf("pc %d: string constant %q, %v", pc, s, ok)
			}
			if _, ok := f.StringCell(pc); !ok {
				t.Errorf("pc %d: no string constant's cell", pc)
			}
			strs++
		}
		if in.Op == ir.BigIntConst {
			if _, bits, kind, ok := f.BigIntCell(pc); !ok || kind != ssa.BigConstInt64 || bits != 7 {
				t.Errorf("pc %d: BigInt constant %d, %d, %v", pc, bits, kind, ok)
			}
			bigs++
		}
		if f.Generic(pc) || f.EntryGeneric(pc) {
			t.Errorf("pc %d: feedback register code does not record yet", pc)
		}
		// A call of a register function is a native call of the one its
		// cache holds, its window the call's (regCallSite); of anything
		// else, a call of Go of its shape (jitRegGoCall); nothing else but
		// a typeof and an instanceof is either.
		sites := f.NativeCalls(pc)
		switch w.Op() {
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW:
			want, native := ssa.GoCall, ssa.GoOp(0)
			if calls := cl.regCalls; w.Op() != bytecode.RNewW && int(w.C()) < len(calls) && calls[w.C()].cl != nil {
				want = native
			}
			if len(sites) != 1 || sites[0].Go != want || sites[0].Argc != int(w.X()) || sites[0].Method != (w.Op() == bytecode.RCallMW) ||
				want == native && (sites[0].Window != p.Code[pc].Left.Slot+1 || sites[0].Callee != uintptr(unsafe.Pointer(cl.regCalls[w.C()].callee)) || sites[0].Entry == 0) {
				t.Errorf("pc %d: call %s: sites %+v", pc, cl.fn.RegCode().Format(int(p.Origin[pc])), sites)
			}
		case bytecode.RTypeof, bytecode.RInstanceOf:
		default:
			if sites != nil {
				t.Errorf("pc %d: %s: native calls %+v", pc, cl.fn.RegCode().Format(int(p.Origin[pc])), sites)
			}
		}
	}
	// p.x += 1 is a read and a store; p.norm a method's read; p.y and the
	// return's p.x and p.y reads.
	if reads < 4 || writes != 1 || gets < 2 || sets != 1 || calls != 2 || natives != 1 || strs != 2 || bigs != 1 {
		t.Errorf("%d reads, %d writes, %d global reads, %d global writes, %d calls, %d of built-ins, %d strings, %d BigInts asked about\n%s",
			reads, writes, gets, sets, calls, natives, strs, bigs, cl.fn.RegCode())
	}
}

// findOwnIndexForTest is the index of o's own property name.
func (o *Object) findOwnIndexForTest(r *Runtime, name string) int {
	return int(o.findOwn(r.atoms.intern(name)))
}
