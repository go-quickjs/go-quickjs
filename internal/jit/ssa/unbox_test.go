package ssa

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// compiledFunctionForTest is the first function src declares, compiled.
func compiledFunctionForTest(t *testing.T, src string) *bytecode.Function {
	t.Helper()
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	top, err := compiler.Compile(prog, compiler.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range top.Constants {
		if c.Kind == bytecode.ConstFunction {
			return c.Fn
		}
	}
	t.Fatal("no function")
	return nil
}

func lowerJS(t *testing.T, src string) *Func {
	t.Helper()
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	top, err := compiler.Compile(prog, compiler.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range top.Constants {
		if c.Kind == bytecode.ConstFunction {
			p, err := jitcompile.Lower(c.Fn)
			if err != nil {
				t.Fatal(err)
			}
			f, err := Build(p)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	t.Fatal("no function")
	return nil
}

// A numeric loop's variables stay unboxed: its phis are Float64, and the only
// tag checks left are at entries, where a slot first comes in.
func TestOptimizeKeepsLoopsUnboxed(t *testing.T) {
	f := lowerJS(t, `function sum(n){let s=0;for(let i=0;i<n;i++)s+=i;return s}`)
	Optimize(f)
	if err := Check(f); err != nil {
		t.Fatal(err)
	}
	tagged, float := 0, 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpPhi && v.Type == Tagged {
				tagged++
			}
			if v.Op == OpPhi && v.Type == Float64 {
				float++
			}
			if v.Op == OpUnboxF64 && b.PC >= 0 && b.LoopHeader {
				t.Errorf("a tag check in the loop header b%d: %v", b.ID, v)
			}
		}
	}
	if tagged != 0 || float < 2 {
		t.Fatalf("%d tagged and %d float phis, want 0 and at least 2:\n%s", tagged, float, f)
	}
}

// An entry loads only the locals the interpreter may read from there before
// it writes them, as frame states name only those: the loop's entry, at its
// header, loads n, s and i, not b, which is written after the loop first.
// What runs from each entry is still the slot IR's (TestBuildMatchesSlotIR).
func TestEntriesLoadLiveLocals(t *testing.T) {
	fn := compiledFunctionForTest(t, `function f(n){let s=0;for(let i=0;i<n;i++)s=(s+i)|0;let b=s+1;return b}`)
	p, err := jitcompile.LowerSSA(fn)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(f); err != nil {
		t.Fatal(err)
	}
	b := -1
	for i, l := range fn.Locals {
		if l.Name == "b" {
			b = i
		}
	}
	loops := 0
	for _, e := range f.Entries {
		if e.PC != 0 {
			loops++
		}
		for _, v := range e.Block.Values {
			if v.Op == OpLoadSlot && v.Aux < p.LiveLocals && !p.LiveAt(e.PC, v.Aux) {
				t.Errorf("the entry at pc %d loads local %d, dead there:\n%s", e.PC, v.Aux, f)
			}
			if v.Op == OpLoadSlot && e.PC != 0 && v.Aux == b {
				t.Errorf("the loop's entry at pc %d loads b:\n%s", e.PC, f)
			}
		}
	}
	if loops == 0 {
		t.Fatalf("no entry but the function's:\n%s", f)
	}
}

// sitesFeedback knows every property site, by key alone.
type sitesFeedback struct{}

func (sitesFeedback) Property(int) (PropertySite, bool) { return PropertySite{Key: 1}, true }
func (sitesFeedback) Global(int) (GlobalSite, bool)     { return GlobalSite{}, false }
func (sitesFeedback) Generic(int) bool                  { return false }
func (sitesFeedback) EntryGeneric(int) bool             { return false }
func (sitesFeedback) Inline(int) (InlineSite, bool)     { return InlineSite{}, false }
func (sitesFeedback) NativeCalls(int) []CallSite        { return nil }

// Code has an entry at its start, at each loop's header and after what
// Go is sure to run -- a call, an instruction left to it whole -- but not
// after a property read it makes natively, whose guard's exit the
// interpreter goes on from to the loop's header, as from V8's
// deoptimizations. A read left to Go whole has one.
func TestEntriesAfterGuards(t *testing.T) {
	fn := compiledFunctionForTest(t, `function f(o,n){let s=0;for(let i=0;i<n;i++){s=(s+o.x)|0;g()}return s}`)
	p, err := jitcompile.LowerSSA(fn)
	if err != nil {
		t.Fatal(err)
	}
	entries := func(fb Feedback) map[int]bool {
		f, err := BuildWith(p, fb)
		if err != nil {
			t.Fatal(err)
		}
		if err := Check(f); err != nil {
			t.Fatal(err)
		}
		at := map[int]bool{}
		for _, e := range f.Entries {
			at[e.PC] = true
		}
		return at
	}
	var read, call, header int = -1, -1, -1
	for pc, in := range p.Code {
		switch {
		case in.Op == ir.PropertyRead:
			read = pc
		case in.Op == ir.Host:
			call = pc
		case (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target <= pc:
			header = in.Target
		}
	}
	if read < 0 || call < 0 || header < 0 {
		t.Fatalf("read %d, call %d, header %d", read, call, header)
	}
	for _, c := range []struct {
		fb   Feedback
		read bool
	}{{sitesFeedback{}, false}, {nil, true}} {
		at := entries(c.fb)
		if !at[0] || !at[header] || !at[call+1] || at[read+1] != c.read {
			t.Errorf("feedback %#v: entries %v; want 0, %d, %d and, %v, %d", c.fb, at, header, call+1, c.read, read+1)
		}
	}
}

// siteFeedback knows every property site as site.
type siteFeedback struct{ site PropertySite }

func (s siteFeedback) Property(int) (PropertySite, bool) { return s.site, true }
func (siteFeedback) Global(int) (GlobalSite, bool)       { return GlobalSite{}, false }
func (siteFeedback) Generic(int) bool                    { return false }
func (siteFeedback) EntryGeneric(int) bool               { return false }
func (siteFeedback) Inline(int) (InlineSite, bool)       { return InlineSite{}, false }
func (siteFeedback) NativeCalls(int) []CallSite          { return nil }

// A property read that meets a receiver its site was not compiled for
// deoptimizes (ir.GuardExit), as V8's map checks do, the interpreter making
// it and its cache learning the shape; but at a megamorphic site, which
// leaves for Go (ir.HostExit) what it does not find further. A site that
// knows no shape at all deoptimizes at every read, as V8's code does for
// insufficient feedback: its block ends there, with no read made and no
// entry after it. None has an entry after it.
func TestBuildPropertyMissesDeoptimize(t *testing.T) {
	fn := compiledFunctionForTest(t, `function f(o,n){let s=0;for(let i=0;i<n;i++){s=(s+o.x)|0}return s}`)
	p, err := jitcompile.LowerSSA(fn)
	if err != nil {
		t.Fatal(err)
	}
	read := -1
	for pc, in := range p.Code {
		if in.Op == ir.PropertyRead {
			read = pc
		}
	}
	if read < 0 {
		t.Fatal("no read")
	}
	for _, c := range []struct {
		site PropertySite
		kind ir.ExitKind
		made bool
	}{
		{PropertySite{Key: 1}, ir.GuardExit, false},
		{PropertySite{Key: 1, Shape: 0x1000}, ir.GuardExit, true},
		{PropertySite{Key: 1, Shape: 0x1000, Megamorphic: true}, ir.HostExit, true},
		{PropertySite{Key: 1, Megamorphic: true}, ir.HostExit, true},
		// An empty cache's read, which may have run where nothing filled
		// it: made natively, by its receiver's own table (Scan).
		{PropertySite{Key: 1, Scan: true}, ir.GuardExit, true},
	} {
		f, err := BuildWith(p, siteFeedback{c.site})
		if err != nil {
			t.Fatal(err)
		}
		if err := Check(f); err != nil {
			t.Fatal(err)
		}
		made, ended := false, false
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op == OpPropRead {
					made = true
					if ir.ExitKind(v.Aux) != c.kind {
						t.Errorf("%+v: the read exits as %v, want %v", c.site, ir.ExitKind(v.Aux), c.kind)
					}
				}
			}
			if b.Kind == BlockExit && b.State != nil && int(b.State.PC) == read {
				ended = true
				if b.ExitKind != c.kind {
					t.Errorf("%+v: the read's block exits as %v, want %v", c.site, b.ExitKind, c.kind)
				}
			}
		}
		if made != c.made || !c.made && !ended {
			t.Errorf("%+v: read made natively %v, its block ended %v at %d:\n%s", c.site, made, ended, read, f)
		}
		for _, e := range f.Entries {
			if e.PC == read+1 {
				t.Errorf("%+v: an entry after the read", c.site)
			}
		}
	}
}

// Build leaves the phis' shadows and the stores' checks to Optimize, which
// would make them again for what it keeps; Finish makes them, once, for
// whatever runs or compiles a Func Optimize has not.
func TestShadowsMadeOnce(t *testing.T) {
	const src = `function f(a,b,c){let t=c?a:b;return t}`
	shadows := func(f *Func) (n int) {
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op == OpPhi && v.Type == Source {
					n++
				}
			}
		}
		return n
	}
	f := lowerJS(t, src)
	if n := shadows(f); n != 0 {
		t.Fatalf("Build made %d shadows:\n%s", n, f)
	}
	f.Finish()
	n := shadows(f)
	if err := Check(f); n == 0 || err != nil {
		t.Fatalf("Finish made %d shadows (%v):\n%s", n, err, f)
	}
	f.Finish()
	if again := shadows(f); again != n {
		t.Fatalf("a second Finish made %d shadows, not %d:\n%s", again, n, f)
	}
	g := lowerJS(t, src)
	Optimize(g)
	Optimize(f)
	if err := Check(g); shadows(g) == 0 || shadows(f) != shadows(g) || err != nil {
		t.Fatalf("Optimize made %d shadows alone, %d after Finish (%v):\n%s", shadows(g), shadows(f), err, g)
	}
}

// Optimize unboxes phis at the start of a round, so that the boxes it
// makes cancel in the same round's walk: a function is mostly done in two
// rounds, the second finding nothing, not three. The corpus takes 25
// rounds, 34 the other way.
func TestOptimizeRounds(t *testing.T) {
	rounds := 0
	for _, src := range jsCorpus {
		f := lowerJS(t, src)
		Optimize(f)
		rounds += f.rounds
	}
	if rounds > 2*len(jsCorpus)+2 {
		t.Errorf("%d rounds for %d functions", rounds, len(jsCorpus))
	}
}

// goCallsFeedback has every call made by a call of Go (GoCall), which
// native code goes on after.
type goCallsFeedback struct{}

func (goCallsFeedback) Property(int) (PropertySite, bool) { return PropertySite{}, false }
func (goCallsFeedback) Global(int) (GlobalSite, bool)     { return GlobalSite{}, false }
func (goCallsFeedback) Generic(int) bool                  { return false }
func (goCallsFeedback) EntryGeneric(int) bool             { return false }
func (goCallsFeedback) Inline(int) (InlineSite, bool)     { return InlineSite{}, false }
func (goCallsFeedback) NativeCalls(int) []CallSite {
	return []CallSite{{Go: GoCall, ThisSlot: -1}}
}
func (goCallsFeedback) GoCalls() bool { return true }

// A captured binding is loaded from its cell where it is read, not after
// every call that may have assigned it: a function reading each of a
// hundred bindings between a hundred calls loads each once, where loading
// them all after each call made ten thousand loads -- RegExp's run blocks,
// of hundreds of bindings and a thousand calls each, took a hundred
// megabytes to compile.
func TestBuildLoadsCapturedBindingsWhereRead(t *testing.T) {
	var src strings.Builder
	src.WriteString("function outer(){var ")
	for i := range 100 {
		if i > 0 {
			src.WriteString(",")
		}
		fmt.Fprintf(&src, "a%d=%d", i, i)
	}
	src.WriteString(";return function f(g){var s=0;")
	for i := range 100 {
		fmt.Fprintf(&src, "g();s+=a%d;", i)
	}
	src.WriteString("return s}}")
	outer := compiledFunctionForTest(t, src.String())
	var fn *bytecode.Function
	for _, c := range outer.Constants {
		if c.Kind == bytecode.ConstFunction {
			fn = c.Fn
		}
	}
	p, err := jitcompile.LowerSSAInline(fn)
	if err != nil {
		t.Fatal(err)
	}
	f, err := BuildWith(p, goCallsFeedback{})
	if err != nil {
		t.Fatal(err)
	}
	loads, calls := 0, 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			switch {
			case v.Op == OpLoadCell && v.Args[0].Op == OpUpvalueCell:
				loads++
			case v.Op == OpCall && len(v.Calls()) != 0:
				calls++
			case v.Op == OpUpvalueStale:
				t.Fatalf("a stale marker in block b%d", b.ID)
			}
			for _, a := range v.Args {
				if a.Op == OpUpvalueStale {
					t.Fatalf("%v reads a stale marker", v)
				}
			}
		}
	}
	if calls != 100 || loads > 200 {
		t.Fatalf("%d calls of Go, %d loads of captured bindings", calls, loads)
	}
	Optimize(f)
	if err := Check(f); err != nil {
		t.Fatal(err)
	}
}

// An entry -- one after each call left to Go whole here -- loads the frame's live
// slots, not the captured bindings' slots: a binding is in its cell, read
// there where it is read (stale at the entry), and the VM's frame has no
// slot for it. Loaded at every entry, each binding's slot made a value no
// one used, which the entry's state kept: a hundred bindings over a
// hundred calls made ten thousand loads, and RegExp's runBlock11, 77
// bindings over 438 calls, 34,767 of them, half its compile.
func TestBuildEntriesLeaveCapturedBindingsInCells(t *testing.T) {
	var src strings.Builder
	src.WriteString("function outer(){var ")
	for i := range 100 {
		if i > 0 {
			src.WriteString(",")
		}
		fmt.Fprintf(&src, "a%d=%d", i, i)
	}
	src.WriteString(";return function f(g){var s=0;")
	for i := range 100 {
		fmt.Fprintf(&src, "g();s+=a%d;", i)
	}
	src.WriteString("return s}}")
	outer := compiledFunctionForTest(t, src.String())
	var fn *bytecode.Function
	for _, c := range outer.Constants {
		if c.Kind == bytecode.ConstFunction {
			fn = c.Fn
		}
	}
	p, err := jitcompile.LowerSSAInline(fn)
	if err != nil {
		t.Fatal(err)
	}
	f, err := BuildWith(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, captured, locals := 0, 0, 0
	for _, b := range f.Blocks {
		if b.PC >= 0 {
			continue
		}
		entries++
		for _, v := range b.Values {
			switch {
			case v.Op != OpLoadSlot:
			case v.Aux >= p.UpvalueBase && v.Aux < p.UpvalueBase+p.Upvalues:
				captured++
			case v.Aux < p.UpvalueBase:
				locals++
			}
		}
	}
	if entries < 100 || captured != 0 || locals == 0 {
		t.Fatalf("%d entries load %d captured bindings' slots and %d locals", entries, captured, locals)
	}
	Optimize(f)
	if err := Check(f); err != nil {
		t.Fatal(err)
	}
}

// A chain of phis that only merge -- a loop's numbers at the entry after
// each of hundreds of element writes, which native code goes on past and
// Go after -- is unboxed when the last of them has
// an unboxed use: the evidence flows back through every one (unboxPhis's
// worklist), where a pass over the phis until none grew took a pass a
// phi.
func TestOptimizeUnboxesLongPhiChains(t *testing.T) {
	var src strings.Builder
	src.WriteString("function f(g,n){let s=0;for(let i=0;i<n;i++){")
	for range 300 {
		src.WriteString("g[0]=0;")
	}
	src.WriteString("s=s+i}return s}")
	p, err := jitcompile.LowerSSA(compiledFunctionForTest(t, src.String()))
	if err != nil {
		t.Fatal(err)
	}
	f, err := BuildWith(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	Optimize(f)
	if err := Check(f); err != nil {
		t.Fatal(err)
	}
	tagged, float := 0, 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			switch {
			case v.Op == OpPhi && v.Type == Tagged:
				tagged++
			case v.Op == OpPhi && v.Type == Float64:
				float++
			}
		}
	}
	// n's, s's and i's at each of the 301 joins unboxed; g's, an array,
	// tagged.
	if tagged > 301 || float < 900 {
		t.Fatalf("%d tagged and %d float phis, want g's 301 tagged and the numbers' 900 or more float", tagged, float)
	}
}

// A call Go makes from native code (GoCall) has no entry after it: Go goes
// on natively after it unless it leaves -- a throw, the runtime stopping,
// the code due to be compiled again -- and then the interpreter runs on to
// the next loop header, where there is one, as from V8's deoptimizations.
// An entry after each had been a join of every live value, a block, and a
// header's loads: a third of the compile of RegExp's run blocks, which
// call built-ins and little else. Nor does it end its block: it is a value
// as any other is.
func TestBuildNoEntriesAfterCallsOfGo(t *testing.T) {
	src := "function f(g,n){let s=0;for(let i=0;i<n;i++){g();g();g();g();s=s+i}return s}"
	p, err := jitcompile.LowerSSA(compiledFunctionForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	f, err := BuildWith(p, goCallsFeedback{})
	if err != nil {
		t.Fatal(err)
	}
	calls, blocks := 0, 0
	for _, b := range f.Blocks {
		in := false
		for _, v := range b.Values {
			if v.Op == OpCall && len(v.Calls()) == 1 && v.Calls()[0].Go == GoCall {
				calls++
				in = true
			}
		}
		if in {
			blocks++
		}
	}
	// The function's start and the loop's header; the four calls in one
	// block, as any other values are.
	if calls != 4 || len(f.Entries) != 2 || blocks != 1 {
		t.Fatalf("%d calls of Go in %d blocks, %d entries, want 4 in 1 and 2", calls, blocks, len(f.Entries))
	}
	Optimize(f)
	if err := Check(f); err != nil {
		t.Fatal(err)
	}
}
