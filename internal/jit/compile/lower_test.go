package compile

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

func compiledFunction(t *testing.T, source string) *bytecode.Function {
	t.Helper()
	ast, err := parser.Parse(source, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fn, err := compiler.Compile(ast, compiler.Options{Text: source})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fn.Constants {
		if c.Kind == bytecode.ConstFunction {
			return c.Fn
		}
	}
	t.Fatal("missing function")
	return nil
}

func fixture(code ...bytecode.Instr) *bytecode.Function {
	return &bytecode.Function{Code: code, LocalCount: 2, Locals: make([]bytecode.LocalDesc, 2), MaxStack: 4, HasSimpleParams: true}
}

func TestStringPackingLayout(t *testing.T) {
	fn := compiledFunction(t, `function pack(s){let a=[];for(let i=0;i<64;i++)a[i]=s.charCodeAt(i);return a}`)
	p, err := Lower(fn)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	seen := [4]bool{}
	for pc, in := range fn.Code {
		sp := p.Locals + p.Maps[pc].Depth
		out := p.Code[pc]
		switch in.Op {
		case bytecode.OpNewArray:
			seen[0] = true
			if out.Op != ir.Host {
				t.Fatal(out)
			}
		case bytecode.OpGetPropThis:
			seen[1] = true
			if out.Op != ir.StringMethod || out.Left != ir.Slot(sp-1) || out.Dest != sp {
				t.Fatal(out)
			}
		case bytecode.OpCallMethod:
			seen[2] = true
			if out.Op != ir.StringCode || out.Left != ir.Slot(sp-2) || out.Right != ir.Slot(sp-3) || out.Third != ir.Slot(sp-1) || out.Dest != sp-3 {
				t.Fatal(out)
			}
		case bytecode.OpSetIndex:
			seen[3] = true
			if out.Op != ir.ArrayWrite {
				t.Fatal(out)
			}
		}
	}
	if seen != [4]bool{true, true, true, true} {
		t.Fatal(seen)
	}
}

// The old pipeline lowers as String.prototype.charCodeAt's only a call
// whose function a read of "charCodeAt" took (charCodeCalls), its argument
// any expression; every other call of one argument is Go's, though the
// function reads "charCodeAt" elsewhere. Every one was lowered so, and its
// check failed at every call: PDF.js's Type1Font_wrap's cff.push left
// native code at every character. The new pipeline lowers every call as
// Go's, its read of the method still the intrinsic's: ssa makes one by
// its target (CharCodeFeedback).
func TestCharCodeCalls(t *testing.T) {
	for _, c := range []struct {
		source string
		calls  []ir.Op // the call_methods', in order, in the old pipeline
	}{
		{`function wrap(fields){var cff=[];for(var k in fields){var field=fields[k];
			for(var i=0,ii=field.length;i<ii;i++)cff.push(field.charCodeAt(i))}return cff}`, []ir.Op{ir.StringCode, ir.Host}},
		{`function f(s){let t=0;for(let i=0;i+1<s.length;i++)t+=s.charCodeAt(i+1)+s.codePointAt(i)+s.charCodeAt(t>3?i:0);return t}`,
			[]ir.Op{ir.StringCode, ir.Host, ir.StringCode}},
		{`function g(a,s){let t=0;for(let i=0;i<64;i++){a.push(i);t+=s.charCodeAt(i)}return t}`, []ir.Op{ir.Host, ir.StringCode}},
	} {
		fn := compiledFunction(t, c.source)
		for _, lowering := range []struct {
			name  string
			lower func(*bytecode.Function) (*ir.Program, error)
		}{{"Lower", Lower}, {"LowerSSA", LowerSSA}} {
			p, err := lowering.lower(fn)
			if err != nil && lowering.name == "Lower" {
				// The old pipeline's lowering takes no for...in.
				continue
			}
			if err != nil {
				t.Fatalf("%s: %s: %v", lowering.name, fn.Name, err)
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("%s: %s: %v", lowering.name, fn.Name, err)
			}
			var calls []ir.Op
			for pc, in := range fn.Code {
				switch in.Op {
				case bytecode.OpCallMethod:
					calls = append(calls, p.Code[pc].Op)
				case bytecode.OpGetPropThis:
					if fn.Names[in.A] == "charCodeAt" && p.Code[pc].Op != ir.StringMethod {
						t.Errorf("%s: %s's read of charCodeAt at %d lowered to %v", lowering.name, fn.Name, pc, p.Code[pc].Op)
					}
				}
			}
			want := c.calls
			if lowering.name == "LowerSSA" {
				want = make([]ir.Op, len(c.calls))
				for k := range want {
					want[k] = ir.Host
				}
			}
			if !slices.Equal(calls, want) {
				t.Errorf("%s: %s's calls lowered to %v, want %v", lowering.name, fn.Name, calls, want)
			}
		}
	}
}

func TestNumericPropertySelection(t *testing.T) {
	fn := compiledFunction(t, `function f(o){let n=o.n;for(let i=0;i<n;i++)o.array[i]=o.array[i]+o.step;return o.array[0]}`)
	p, err := Lower(fn)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for pc, in := range fn.Code {
		if in.Op != bytecode.OpGetProp {
			continue
		}
		name := fn.Names[in.A]
		seen[name] = true
		want := ir.PropertyRead
		if name == "array" {
			want = ir.ReferenceRead
		}
		if p.Code[pc].Op != want || p.Code[pc].Key != in.A {
			t.Fatalf("property %s lowered to %+v, want %v", name, p.Code[pc], want)
		}
	}
	if !seen["array"] || !seen["n"] || !seen["step"] {
		t.Fatal("missing numeric or reference property fixture")
	}
}

func TestSmallCalleeSelection(t *testing.T) {
	fn := compiledFunction(t, `function f(){return this.x<0?0:this}`)
	if _, err := Lower(fn); err == nil {
		t.Fatal("small field callee entered the standalone policy")
	}
	p, err := LowerSSAInline(fn)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if !p.This {
		t.Fatal("callee lost its receiver snapshot")
	}
	found := false
	for _, in := range p.Code {
		found = found || in.Op == ir.PropertyRead
	}
	if !found {
		t.Fatal("small callee lost its guarded numeric field")
	}
}

func TestReferencePropertySelection(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   ir.Op
	}{
		{`function f(o,n){let a=o.array;for(let i=0;i<n;i++)a[i]+=1;return a[0]}`, ir.Host},
		{`function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.array[0];return s}`, ir.ReferenceRead},
		{`function f(o,n,cb){let s=0;for(let i=0;i<n;i++){s+=o.array[0];cb()}return s}`, ir.Host},
		{`function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.array[0]*scale;return s}`, ir.ReferenceRead},
	} {
		fn := compiledFunction(t, tc.source)
		p, err := Lower(fn)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		found := false
		for pc, in := range fn.Code {
			if in.Op == bytecode.OpGetProp {
				found = true
				if p.Code[pc].Op != tc.want {
					t.Fatalf("%s: %+v, want %v", tc.source, p.Code[pc], tc.want)
				}
			}
		}
		if !found {
			t.Fatal("missing reference read")
		}
	}
}

func TestNumericGlobalSelection(t *testing.T) {
	fn := compiledFunction(t, `function f(a,n){for(let i=0;i<n;i++)a[i]=i*scale+offset;return a[n-1]}`)
	p, err := Lower(fn)
	if err != nil || len(p.Globals) != 2 || p.Locals != fn.LocalCount+len(fn.Upvalues)+2 {
		t.Fatalf("numeric binding layout: %+v, %v", p, err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for pc, in := range fn.Code {
		if in.Op == bytecode.OpGetGlobal {
			got := p.Code[pc]
			if got.Op != ir.BindingRead || got.Left.Slot < p.Locals-len(p.Globals) || got.Left.Slot >= p.Locals || got.Key != in.A {
				t.Fatalf("binding read lost its reserved view: %+v", got)
			}
		}
	}
}

func TestGlobalSlotBudget(t *testing.T) {
	fn := fixture(bytecode.Instr{Op: bytecode.OpGetGlobal, B: 1}, bytecode.Instr{Op: bytecode.OpPushInt, A: 2}, bytecode.Instr{Op: bytecode.OpMul}, bytecode.Instr{Op: bytecode.OpDrop}, bytecode.Instr{Op: bytecode.OpJump})
	fn.Names = []string{"scale"}
	fn.PropSites = 1
	fn.LocalCount, fn.MaxStack = MaxSlots-2, 2
	fn.Locals = make([]bytecode.LocalDesc, fn.LocalCount)
	p, err := Lower(fn)
	if err != nil || len(p.Globals) != 0 || p.Code[0].Op != ir.Host || p.Locals+p.StackSize != MaxSlots {
		t.Fatalf("binding view exceeded scratch capacity: %+v, %v", p, err)
	}
}

func TestReceiverSlot(t *testing.T) {
	fn := compiledFunction(t, `function f(a){a[0]+=1;return this}`)
	p, err := Lower(fn)
	if err != nil || !p.This || p.Locals != fn.LocalCount+len(fn.Upvalues)+1 {
		t.Fatalf("receiver layout: %+v, %v", p, err)
	}
	for pc, in := range fn.Code {
		if in.Op == bytecode.OpPushThis {
			got := p.Code[pc]
			if got.Op != ir.Copy || got.Left.Slot != p.Locals-1 || !got.Check || got.CheckSlot != p.Locals-1 {
				t.Fatalf("receiver load lost its binding guard: %+v", got)
			}
			return
		}
	}
	t.Fatal("missing receiver load")
}

func TestReceiverSlotBudget(t *testing.T) {
	fn := fixture(bytecode.Instr{Op: bytecode.OpPushThis}, bytecode.Instr{Op: bytecode.OpDrop}, bytecode.Instr{Op: bytecode.OpPushInt}, bytecode.Instr{Op: bytecode.OpBitNot}, bytecode.Instr{Op: bytecode.OpDrop}, bytecode.Instr{Op: bytecode.OpJump})
	fn.LocalCount, fn.MaxStack = MaxSlots-1, 1
	fn.Locals = make([]bytecode.LocalDesc, fn.LocalCount)
	p, err := Lower(fn)
	if err != nil || p.This || p.Locals+p.StackSize != MaxSlots || p.Code[0].Op != ir.Host {
		t.Fatalf("receiver exceeded scratch storage instead of taking the host path: %+v, %v", p, err)
	}
}

func TestMixedPropertyLoopSelection(t *testing.T) {
	fn := compiledFunction(t, `function f(a,n,cb){for(let i=0;i<n;i++){a[i]=(this.x+i)|0;this.x+=1;cb()}return a[0]}`)
	p, err := Lower(fn)
	if err != nil {
		t.Fatal(err)
	}
	if p.This || p.Locals != fn.LocalCount+len(fn.Upvalues) {
		t.Fatal("mixed loop prepared an unprofitable receiver snapshot")
	}
	for _, in := range p.Code {
		if in.Op == ir.PropertyRead || in.Op == ir.PropertyWrite {
			t.Fatal("mixed loop prepared unprofitable numeric property views")
		}
	}
}

func TestRefusals(t *testing.T) {
	for _, tc := range []struct{ source, reason string }{
		{`function f(a) { return a.x }`, "host operations without native loop or array work"},
		{`function f(a,n,cb) { for(let i=0;i<n;i++){a.x++;cb()}return a.x }`, "property operations without native array or bitwise work"},
		{`function f(a) { return new a() }`, "unsupported opcode"},
		{`function f() { try { return 1 } catch(e) { return 2 } }`, "unsupported opcode push_catch"},
		{`function f() { eval('1') }`, "direct eval"},
		{`function f() { let a=1; return () => a }`, "captured local"},
		{`function f() { return arguments[0] }`, "arguments object"},
		{`function f(a=1) { return a }`, "non-simple parameters"},
		{`function* f() { yield 1 }`, "async or generator"},
		{`async function f() { return 1 }`, "async or generator"},
		{`function f() { with ({}) { return 1 } }`, "unsupported opcode"},
		{`function f(a) { return a ** 2 }`, "host operations without native loop or array work"},
		{`function f() { const a=1; a=2; return a }`, "unsupported opcode"},
	} {
		t.Run(tc.reason+tc.source, func(t *testing.T) {
			_, err := Lower(compiledFunction(t, tc.source))
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("refusal = %v, want %q", err, tc.reason)
			}
		})
	}
}

// The in operator is the new pipeline's HasProperty of the key, below, and
// the object, on top, its boolean where the key was; the old pipeline, which
// calls no Go, refuses it.
func TestInLowering(t *testing.T) {
	fn := compiledFunction(t, `function f(a){let s=0;for(let i=0;i<10;i++)s+=(i in a)?1:0;return s}`)
	if _, err := Lower(fn); err == nil || !strings.Contains(err.Error(), "unsupported opcode in") {
		t.Fatalf("Lower: %v, want the in refused", err)
	}
	p, err := LowerSSA(fn)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	found := false
	for pc, in := range fn.Code {
		if in.Op != bytecode.OpIn {
			continue
		}
		found = true
		sp := p.Locals + p.Maps[pc].Depth
		want := ir.Instruction{Op: ir.HasProperty, Left: ir.Slot(sp - 2), Right: ir.Slot(sp - 1), Dest: sp - 2}
		if got := p.Code[pc]; got != want {
			t.Fatalf("pc %d: %+v, want %+v", pc, got, want)
		}
		if p.Maps[pc+1].Depth != p.Maps[pc].Depth-1 {
			t.Fatalf("pc %d: depth %d after, %d before", pc, p.Maps[pc+1].Depth, p.Maps[pc].Depth)
		}
	}
	if !found {
		t.Fatal("missing in")
	}
}

// ** is the new pipeline's Binary Pow of the two operands on top, the
// result where the base was; the old pipeline leaves it to Go, as a
// remainder.
func TestPowLowering(t *testing.T) {
	fn := compiledFunction(t, `function f(a,n){let s=0;for(let i=0;i<n;i++)s+=a**i;return s}`)
	old, err := Lower(fn)
	if err != nil {
		t.Fatal(err)
	}
	p, err := LowerSSA(fn)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	found := false
	for pc, in := range fn.Code {
		if in.Op != bytecode.OpPow {
			continue
		}
		found = true
		if old.Code[pc].Op != ir.Host {
			t.Fatalf("pc %d: the old pipeline's %+v, want a host operation", pc, old.Code[pc])
		}
		sp := p.Locals + p.Maps[pc].Depth
		want := ir.Instruction{Op: ir.Binary, Operator: ir.Pow, Left: ir.Slot(sp - 2), Right: ir.Slot(sp - 1), Dest: sp - 2}
		if got := p.Code[pc]; got != want {
			t.Fatalf("pc %d: %+v, want %+v", pc, got, want)
		}
	}
	if !found {
		t.Fatal("missing **")
	}
}

// The delete operator is the new pipeline's DeleteProperty of the object,
// below, and the key, on top, its boolean where the object was, strict as
// the function is -- delete o.k's key a constant pushed, delete o[k]'s any
// value; the old pipeline, which calls no Go, refuses it.
func TestDeleteLowering(t *testing.T) {
	for _, c := range []struct {
		src    string
		strict bool
	}{
		{`function f(o,k){let s=0;for(let i=0;i<10;i++)s+=(delete o[k])?1:0;return s+(delete o.x)}`, false},
		{`function f(o,k){'use strict';let s=0;for(let i=0;i<10;i++)s+=(delete o[k])?1:0;return s+(delete o.x)}`, true},
	} {
		fn := compiledFunction(t, c.src)
		if _, err := Lower(fn); err == nil || !strings.Contains(err.Error(), "unsupported opcode delete_prop") {
			t.Fatalf("Lower: %v, want the delete refused", err)
		}
		p, err := LowerSSA(fn)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		found := 0
		for pc, in := range fn.Code {
			if in.Op != bytecode.OpDeleteProp {
				continue
			}
			found++
			sp := p.Locals + p.Maps[pc].Depth
			want := ir.Instruction{Op: ir.DeleteProperty, Left: ir.Slot(sp - 2), Right: ir.Slot(sp - 1), Dest: sp - 2, Strict: c.strict}
			if got := p.Code[pc]; got != want {
				t.Fatalf("pc %d: %+v, want %+v", pc, got, want)
			}
			if p.Maps[pc+1].Depth != p.Maps[pc].Depth-1 {
				t.Fatalf("pc %d: depth %d after, %d before", pc, p.Maps[pc+1].Depth, p.Maps[pc].Depth)
			}
		}
		if found != 2 {
			t.Fatalf("%d deletes, want 2", found)
		}
	}
}

func TestRemainderHostBoundaries(t *testing.T) {
	for _, code := range [][]bytecode.Instr{
		{{Op: bytecode.OpGetLocalIndex, B: 1}, {Op: bytecode.OpPushInt, A: 3}, {Op: bytecode.OpMod}, {Op: bytecode.OpReturn}},
		{{Op: bytecode.OpGetLocalIndex, B: 1}, {Op: bytecode.OpBinLocal, A: 1, B: uint32(bytecode.OpMod)}, {Op: bytecode.OpReturn}},
		{{Op: bytecode.OpGetLocalIndex, B: 1}, {Op: bytecode.OpBinImm, A: 3, B: uint32(bytecode.OpMod)}, {Op: bytecode.OpReturn}},
		{{Op: bytecode.OpGetLocalIndex, B: 1}, {Op: bytecode.OpDrop}, {Op: bytecode.OpLocalBinImm, A: uint32(bytecode.OpMod) << 24, B: 3}, {Op: bytecode.OpReturn}},
	} {
		p, err := Lower(fixture(code...))
		if err != nil {
			t.Fatal(err)
		}
		pc := len(code) - 2
		if p.Code[pc].Op != ir.Host {
			t.Fatalf("remainder at %d did not retain its host boundary", pc)
		}
		slots := make([]ir.Value, p.Locals+p.StackSize)
		exit, err := p.Evaluate(slots, pc, 1)
		if err != nil || exit.Kind != ir.HostExit || exit.State != p.Maps[pc] || exit.Steps != 0 {
			t.Fatalf("remainder exit %+v, %v", exit, err)
		}
	}
}

func TestMalformedBytecode(t *testing.T) {
	tests := []struct {
		name   string
		fn     *bytecode.Function
		pc     int
		reason string
	}{
		{"underflow", fixture(bytecode.Instr{Op: bytecode.OpAdd}), 0, "underflow"},
		{"local", fixture(bytecode.Instr{Op: bytecode.OpGetLocal, A: ^uint32(0)}), 0, "local index"},
		{"constant", fixture(bytecode.Instr{Op: bytecode.OpPushConst, A: ^uint32(0)}), 0, "constant"},
		{"target", fixture(bytecode.Instr{Op: bytecode.OpJump, A: ^uint32(0)}), 0, "branch target"},
		{"fallthrough", fixture(bytecode.Instr{Op: bytecode.OpNop}), 0, "fallthrough"},
		{"fused", fixture(bytecode.Instr{Op: bytecode.OpBinImm, B: 256 + uint32(bytecode.OpAdd)}), 0, "fused arithmetic"},
		{"comparison", fixture(bytecode.Instr{Op: bytecode.OpJumpIfCmpFalse, B: uint32(bytecode.OpAdd)}), 0, "fused comparison"},
		{"update", fixture(bytecode.Instr{Op: bytecode.OpUpdateLocal, B: 4}), 0, "update flags"},
		{"immutable", fixture(bytecode.Instr{Op: bytecode.OpSetLocalCheck}), 0, "immutable local"},
		{"join", fixture(
			bytecode.Instr{Op: bytecode.OpPushTrue},
			bytecode.Instr{Op: bytecode.OpJumpIfTrueKeep, A: 3},
			bytecode.Instr{Op: bytecode.OpJump, A: 3},
			bytecode.Instr{Op: bytecode.OpReturnUndef}), 3, "inconsistent stack depth"},
		{"unreachable", fixture(bytecode.Instr{Op: bytecode.OpReturnUndef}, bytecode.Instr{Op: bytecode.OpTailCall}), 1, "unsupported opcode tail_call"},
	}
	tooDeep := fixture(bytecode.Instr{Op: bytecode.OpGetLocal2}, bytecode.Instr{Op: bytecode.OpReturn})
	tooDeep.MaxStack = 1
	tests = append(tests, struct {
		name   string
		fn     *bytecode.Function
		pc     int
		reason string
	}{"overflow", tooDeep, 0, "exceeds MaxStack"})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Lower(tc.fn)
			var refusal *Refusal
			if !errors.As(err, &refusal) || refusal.PC != tc.pc || !strings.Contains(refusal.Reason, tc.reason) {
				t.Fatalf("refusal = %v, want pc %d %q", err, tc.pc, tc.reason)
			}
		})
	}
}

func TestCompilerBudgets(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*bytecode.Function)
	}{
		{"instructions", func(f *bytecode.Function) { f.Code = make([]bytecode.Instr, MaxInstructions+1) }},
		{"slots", func(f *bytecode.Function) { f.MaxStack = MaxSlots }},
		{"negative", func(f *bytecode.Function) { f.LocalCount = -1 }},
		{"layout", func(f *bytecode.Function) { f.Locals = nil }},
		{"upvalues", func(f *bytecode.Function) { f.Upvalues = make([]bytecode.UpvalueDesc, MaxSlots) }},
		{"mapped arguments", func(f *bytecode.Function) { f.MappedArguments = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn := fixture(bytecode.Instr{Op: bytecode.OpReturnUndef})
			tc.edit(fn)
			if _, err := Lower(fn); err == nil {
				t.Fatal("accepted function outside bounds")
			}
		})
	}
}

// TestLiveLocals pins the locals the new pipeline's frame states keep
// (ir.Program.Live): one is live where the code may read it before writing
// it -- round a loop's back edge too, and down either arm of a branch --
// and dead before its first write and after its last read.
func TestLiveLocals(t *testing.T) {
	fn := compiledFunction(t, `function f(n){let a=n*2;let s=0;for(let i=0;i<n;i++){s=(s+i)|0;if(i===7)s=(s+a)|0}let b=s+1;return b}`)
	p, err := LowerSSA(fn)
	if err != nil {
		t.Fatal(err)
	}
	local := func(name string) int {
		for i, l := range fn.Locals {
			if l.Name == name {
				return i
			}
		}
		t.Fatalf("no local %s", name)
		return -1
	}
	n, a, s, i, b := local("n"), local("a"), local("s"), local("i"), local("b")
	// The loop: the first instruction past its header's comparison.
	var loop, after int = -1, -1
	for pc, in := range fn.Code {
		if in.Op == bytecode.OpJumpIfCmpFalse && loop < 0 {
			loop, after = pc+1, int(in.A)
		}
	}
	if loop < 0 {
		t.Fatal("no loop")
	}
	for _, c := range []struct {
		pc    int
		local int
		live  bool
		what  string
	}{
		{loop, n, true, "n in the loop, read at its back edge's test"},
		{loop, a, true, "a in the loop, read on one arm"},
		{loop, s, true, "s in the loop"},
		{loop, i, true, "i in the loop"},
		{loop, b, false, "b before its first write"},
		{after, i, false, "i after the loop"},
		{after, a, false, "a after the loop"},
		{after, n, false, "n after the loop"},
		{after, s, true, "s after the loop, read once more"},
	} {
		if got := p.LiveAt(c.pc, c.local); got != c.live {
			t.Errorf("%s: live %v, want %v", c.what, got, c.live)
		}
	}
	if !p.LiveAt(0, len(fn.Locals)+5) {
		t.Error("a slot past the locals is not known dead")
	}
}

func TestLoopStateMaps(t *testing.T) {
	fn := compiledFunction(t, `function sum(n) { let s=0; for(let i=0; i<n; i++) s+=i; return s }`)
	p, err := Lower(fn)
	if err != nil {
		t.Fatal(err)
	}
	depths := []int{0, 1, 0, 1, 0, 1, 0, 2, 0, 2, 1, 0, 0, 0, 1, -1}
	if len(p.Maps) != len(depths) {
		t.Fatalf("unexpected fixture bytecode:\n%s", fn.Disassemble())
	}
	for pc, depth := range depths {
		if p.Maps[pc] != (ir.StateMap{PC: uint32(pc), Depth: depth}) {
			t.Fatalf("pc %d map = %+v, want depth %d", pc, p.Maps[pc], depth)
		}
	}
	slots := make([]ir.Value, p.Locals+p.StackSize)
	slots[0] = ir.Float(10)
	pc, exits := 0, 0
	for {
		exit, err := p.Evaluate(slots, pc, 3)
		if err != nil {
			t.Fatal(err)
		}
		if exit.Kind == ir.Returned {
			if exit.Value != ir.Float(45) || exits < 10 {
				t.Fatalf("result = %+v, budget exits = %d", exit, exits)
			}
			break
		}
		if exit.Kind != ir.BudgetExit || exit.Steps != 3 {
			t.Fatalf("exit = %+v", exit)
		}
		pc = int(exit.State.PC)
		exits++
		if exits > 100 {
			t.Fatal("did not finish bounded loop")
		}
	}
}

func TestKeepBranchDepths(t *testing.T) {
	for _, op := range []bytecode.Op{bytecode.OpJumpIfTrueKeep, bytecode.OpJumpIfFalseKeep} {
		fn := fixture(
			bytecode.Instr{Op: bytecode.OpGetLocal},
			bytecode.Instr{Op: op, A: 3},
			bytecode.Instr{Op: bytecode.OpPushInt, A: 7},
			bytecode.Instr{Op: bytecode.OpReturn})
		p, err := Lower(fn)
		if err != nil {
			t.Fatal(err)
		}
		if p.Maps[2].Depth != 0 || p.Maps[3].Depth != 1 {
			t.Fatalf("keep edge depths = %+v", p.Maps)
		}
		for _, v := range []ir.Value{ir.Bool(true), ir.Bool(false), ir.Float(math.NaN()), ir.Float(math.Copysign(0, -1)), {Kind: ir.Null}, {Kind: ir.Undefined}} {
			slots := make([]ir.Value, p.Locals+p.StackSize)
			slots[0] = v
			exit, err := p.Evaluate(slots, 0, 2)
			if err != nil || exit.Kind != ir.BudgetExit || exit.State.Depth != p.Maps[exit.State.PC].Depth {
				t.Fatalf("branch exit = %+v, %v", exit, err)
			}
			wantTaken := v == ir.Bool(true)
			if op == bytecode.OpJumpIfFalseKeep {
				wantTaken = !wantTaken
			}
			if (exit.State.PC == 3) != wantTaken {
				t.Fatalf("op %s value %+v: exit = %+v", op, v, exit)
			}
		}
	}
}

func TestSharedProgram(t *testing.T) {
	fn := compiledFunction(t, `function f(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }`)
	previous := fn.VMCode
	p, err := Lower(fn)
	if err != nil {
		t.Fatal(err)
	}
	if fn.VMCode != previous {
		t.Fatal("lowering changed the shared tree cache")
	}
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				slots := make([]ir.Value, p.Locals+p.StackSize)
				slots[0] = ir.Float(float64(n))
				exit, err := p.Evaluate(slots, 0, 1000)
				if err != nil || exit.Kind != ir.Returned || exit.Value != ir.Float(float64(n*(n-1)/2)) {
					t.Errorf("shared program n=%d: %+v, %v", n, exit, err)
					return
				}
			}
		}(n)
	}
	wg.Wait()
}

// Accepted bytecode must be executable within the declared slots and bounded
// instruction count, even when its operands did not come from the compiler.
func FuzzLower(f *testing.F) {
	for _, code := range [][]bytecode.Instr{
		{{Op: bytecode.OpReturnUndef}},
		{{Op: bytecode.OpPushInt}, {Op: bytecode.OpReturn}},
		{{Op: bytecode.OpGetLocal2, B: 1}, {Op: bytecode.OpAdd}, {Op: bytecode.OpReturn}},
		{{Op: bytecode.OpJump}},
	} {
		data := make([]byte, 9*len(code))
		for pc, in := range code {
			data[pc*9] = byte(in.Op)
			binary.LittleEndian.PutUint32(data[pc*9+1:], in.A)
			binary.LittleEndian.PutUint32(data[pc*9+5:], in.B)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1024 {
			return
		}
		fn := fixture()
		fn.Constants = []bytecode.Constant{{Kind: bytecode.ConstNumber, Num: 1}}
		for i := 0; i+9 <= len(data); i += 9 {
			fn.Code = append(fn.Code, bytecode.Instr{Op: bytecode.Op(data[i]), A: binary.LittleEndian.Uint32(data[i+1:]), B: binary.LittleEndian.Uint32(data[i+5:])})
		}
		p, err := Lower(fn)
		if err != nil {
			return
		}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		slots := make([]ir.Value, p.Locals+p.StackSize)
		slots[0], slots[1] = ir.Float(2), ir.Float(3)
		if _, err := p.Evaluate(slots, 0, 64); err != nil {
			t.Fatal(err)
		}
	})
}

// A panic in analysis is a refusal, not a crashed host.
func TestLowerPanicRefuses(t *testing.T) {
	previous := lowerImpl
	defer func() { lowerImpl = previous }()
	lowerImpl = func(*bytecode.Function, lowering) (*ir.Program, error) { panic("analysis bug") }
	for _, lower := range []func(*bytecode.Function) (*ir.Program, error){Lower, LowerSSA, LowerSSAInline} {
		p, err := lower(&bytecode.Function{})
		var refusal *Refusal
		if p != nil || !errors.As(err, &refusal) {
			t.Fatalf("lowering after a panic = %v, %v; want a refusal", p, err)
		}
	}
}

// Every opcode describe accepts has a case in lower. One without would have
// become a Nop carrying describe's stack effect, a silent miscompile; now it
// panics, which lowering turns into a refusal, and this test fails.
func TestLowerCoversDescribedOpcodes(t *testing.T) {
	fn := &bytecode.Function{
		ParamCount: 1, LocalCount: 4, PropSites: 4,
		Code:      make([]bytecode.Instr, 16),
		Constants: []bytecode.Constant{{Kind: bytecode.ConstNumber, Num: 1}},
		Names:     []string{"x", "charCodeAt"},
		Locals:    []bytecode.LocalDesc{{Mutable: true}, {Mutable: true}, {Mutable: true}, {Mutable: true}},
		Upvalues:  make([]bytecode.UpvalueDesc, 2),
	}
	operands := []uint32{0, 1, 2, 3, uint32(bytecode.OpAdd), uint32(bytecode.OpLt), uint32(bytecode.OpBitAnd), 1<<24 | 1}
	accepted := map[bytecode.Op]bool{}
	for op := 0; op < 256; op++ {
		code := bytecode.Op(op)
		if strings.HasPrefix(code.String(), "op(") {
			continue
		}
		for _, a := range operands {
			for _, b := range operands {
				in := bytecode.Instr{Op: code, A: a, B: b}
				if _, err := describe(fn, 0, in); err != nil {
					continue
				}
				accepted[code] = true
				func() {
					defer func() {
						if v := recover(); v != nil {
							t.Errorf("%v (A=%d B=%d) is described but not lowered: %v", code, a, b, v)
						}
					}()
					lower(fn, in, 8, false, false)
				}()
			}
		}
	}
	if len(accepted) < 40 {
		t.Fatalf("only %d opcodes accepted: the fixture no longer exercises describe", len(accepted))
	}
}
