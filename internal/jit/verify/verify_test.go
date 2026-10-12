//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

// Package verify checks the JIT's emitters against independent
// disassemblers, golang.org/x/arch's. It is a module of its own, so that
// go-quickjs never depends on them.
//
//	cd internal/jit/verify && go test -tags quickjs_jit ./...
//
// Generated code runs on a goroutine's stack between Go frames, with no
// metadata of its own, and Go cannot recover a fault in it. So every function
// the backend compiles, for every target, must decode in full, and must keep
// the registers and the stack Go relies on: no calls, pushes or pops, no
// system calls or traps, and no use of SP, BP and R14 (g) on amd64, or SP,
// R18 (platform), R28 (g), R29 (FP) and R30 (LR) on arm64 beyond the RET
// that returns through it.
package verify

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"testing"

	"golang.org/x/arch/arm64/arm64asm"
	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

var (
	x86Reserved  = regexp.MustCompile(`\b(RSP|ESP|SP|SPL|RBP|EBP|BP|BPL|R14|R14L|R14W|R14B|R14D)\b`)
	x86Forbidden = map[string]bool{"CALL": true, "PUSH": true, "POP": true, "PUSHF": true, "POPF": true, "SYSCALL": true, "INT": true, "ENTER": true, "LEAVE": true, "HLT": true, "UD2": true}
	a64Reserved  = regexp.MustCompile(`\b(SP|WSP|X18|W18|X28|W28|X29|W29|X30|W30)\b`)
	a64Forbidden = map[string]bool{"BL": true, "BLR": true, "SVC": true, "HVC": true, "SMC": true, "BRK": true, "HLT": true, "UDF": true}
	arches       = []string{"amd64", "arm64"}
	checkedBytes = map[string]int{}
	checkedInsts = map[string]int{}
)

// check disassembles code and reports the first problem in it.
func check(arch string, code []byte) error {
	switch arch {
	case "amd64":
		for pc := 0; pc < len(code); {
			inst, err := x86asm.Decode(code[pc:], 64)
			if err != nil {
				return fmt.Errorf("offset %#x: does not decode (% x): %v", pc, code[pc:min(pc+16, len(code))], err)
			}
			text := inst.String()
			if x86Forbidden[inst.Op.String()] || x86Reserved.MatchString(text) {
				return fmt.Errorf("offset %#x: %s", pc, text)
			}
			pc += inst.Len
			checkedInsts[arch]++
		}
	case "arm64":
		if len(code)%4 != 0 {
			return fmt.Errorf("%d bytes is not whole instructions", len(code))
		}
		for pc := 0; pc < len(code); pc += 4 {
			inst, err := arm64asm.Decode(code[pc:])
			if err != nil {
				return fmt.Errorf("offset %#x: does not decode (%08x): %v", pc, binary.LittleEndian.Uint32(code[pc:]), err)
			}
			text := inst.String()
			if inst.Op == arm64asm.RET {
				if text != "RET" && text != "RET X30" {
					return fmt.Errorf("offset %#x: %s returns through a register other than X30", pc, text)
				}
			} else if a64Forbidden[inst.Op.String()] || a64Reserved.MatchString(text) {
				return fmt.Errorf("offset %#x: %s", pc, text)
			}
			checkedInsts[arch]++
		}
	}
	checkedBytes[arch] += len(code)
	return nil
}

// jsCorpus covers what lowering accepts: arithmetic, comparisons and every
// bitwise operator in their fused and immediate forms, arrays, properties,
// globals, captured bindings, calls, strings and the stack shuffles of
// compound assignments.
var jsCorpus = []string{
	`function f(n){let s=0;for(let i=0;i<n;i++)s+=i*0.5-i/3;return s}`,
	`function f(n){var x=0.1;for(var i=0;i<n;i++)x=3.7*x*(1-x);return x}`,
	`function f(n){let h=0;for(let i=0;i<n;i++){h=(h<<5)-h+i|0;h^=h>>>13;h&=0x7fffffff;h|=1;h=~h;h>>=1}return h}`,
	`function f(a,b){let s=0;for(let i=0;i<a.length;i++){s+=a[i]*b[i];a[i]+=1;b[i]-=a[i]}return s}`,
	`function f(a){for(let i=1;i<a.length-1;i++)a[i]=(a[i-1]+a[i]+a[i+1])/3;return a[0]}`,
	`function f(a,n){for(let i=0;i<n;i++)a[i]=i*i;return a.length}`,
	`function f(o,n){for(let i=0;i<n;i++){o.x=(o.x*31+i)|0;o.y+=o.x&255}return o.x}`,
	`var scale=3,offset=7;function f(a){let s=0;for(let i=0;i<a.length;i++)s+=a[i]*scale+offset;return s}`,
	`function g(x){return x*2+1}function f(n){let s=0;for(let i=0;i<n;i++)s+=g(i);return s}`,
	`function f(s){let h=0;for(let i=0;i<s.length;i++)h=(h*33+s.charCodeAt(i))|0;return h}`,
	`function f(n){let s=0;for(let i=0;i<n;i++){if(i%3===0)continue;if(s>1e6)break;s+=i<10?i:-i}return s}`,
	`function f(a,b,n){let c=0;for(let i=0;i<n;i++){c+=a[i]&b[i];c^=a[i]|b[i];c=c<<1>>>1}return c}`,
	`function mk(k){return function f(n){let s=0;for(let i=0;i<n;i++)s+=k*i;return s}}`,
	`function f(n){let x=-0,y=NaN,z=Infinity;for(let i=0;i<n;i++){x=x*-1;y=y===y?0:1;z=1/z}return x+y+z}`,
	`function f(a){let m=a[0];for(let i=1;i<a.length;i++){if(a[i]>m)m=a[i];if(a[i]<=-m)m=-a[i]}return m}`,
	`function f(o,a){let s=0;for(let i=0;i<a.length;i++)s=(s+o.p.q+a[i])|0;return s}`,
}

func lowered(t *testing.T, src string) []*ir.Program {
	t.Helper()
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	top, err := compiler.Compile(prog, compiler.Options{})
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	var out []*ir.Program
	var walk func(fn *bytecode.Function)
	walk = func(fn *bytecode.Function) {
		for _, c := range fn.Constants {
			if c.Kind != bytecode.ConstFunction {
				continue
			}
			for _, lower := range []func(*bytecode.Function) (*ir.Program, error){jitcompile.Lower, jitcompile.LowerSSA, jitcompile.LowerSSAInline} {
				if p, err := lower(c.Fn); err == nil {
					out = append(out, p)
				}
			}
			walk(c.Fn)
		}
	}
	walk(top)
	return out
}

var literals = []ir.Value{ir.Float(0), ir.Float(math.Copysign(0, -1)), ir.Float(1), ir.Float(-1), ir.Float(0.5),
	ir.Float(255), ir.Float(1 << 31), ir.Float(1 << 32), ir.Float(math.NaN()), ir.Float(math.Inf(1)),
	ir.Bool(true), ir.Bool(false), {Kind: ir.Undefined}, {Kind: ir.Null}}

// randomProgram builds a program from random instructions over a few locals;
// the caller keeps only those Validate accepts.
func randomProgram(r *rand.Rand) *ir.Program {
	locals := 1 + r.IntN(12)
	n := 1 + r.IntN(40)
	operand := func() ir.Operand {
		if r.IntN(3) == 0 {
			return ir.Literal(literals[r.IntN(len(literals))])
		}
		return ir.Slot(r.IntN(locals))
	}
	p := &ir.Program{Locals: locals}
	ops := []ir.Op{ir.Copy, ir.Binary, ir.Binary, ir.Binary, ir.Unary, ir.Update, ir.Branch, ir.Jump,
		ir.ArrayRead, ir.ArrayWrite, ir.ArrayLength, ir.ArrayUpdate, ir.ArrayKey, ir.Swap, ir.CopyPair, ir.Host, ir.Nop}
	for pc := 0; pc < n; pc++ {
		in := ir.Instruction{Op: ops[r.IntN(len(ops))], Left: operand(), Right: operand(), Third: operand(),
			Dest: r.IntN(locals), Extra: r.IntN(locals), Operator: ir.Operator(r.IntN(int(ir.BitNot) + 1)),
			Target: r.IntN(n + 1), Postfix: r.IntN(2) == 0, When: r.IntN(2) == 0}
		if r.IntN(8) == 0 {
			in.Check, in.CheckSlot = true, r.IntN(locals)
		}
		p.Code = append(p.Code, in)
	}
	p.Code = append(p.Code, ir.Instruction{Op: ir.Return, Left: operand()})
	p.Maps = make([]ir.StateMap, len(p.Code))
	for pc := range p.Maps {
		p.Maps[pc].PC = uint32(pc)
	}
	return p
}

// globals is feedback that has found every global binding, a script's
// lexical binding at odd PCs, and every method read's receivers strings,
// so that the code of those cells is checked.
type globals struct{}

func (globals) Property(int) (ssa.PropertySite, bool) { return ssa.PropertySite{}, false }
func (globals) Global(pc int) (ssa.GlobalSite, bool) {
	return ssa.GlobalSite{Key: 7, Index: int32(pc % 5), Lexical: pc%2 == 1}, true
}
func (globals) Generic(int) bool                  { return false }
func (globals) EntryGeneric(int) bool             { return false }
func (globals) Inline(int) (ssa.InlineSite, bool) { return ssa.InlineSite{}, false }
func (globals) NativeCalls(int) []ssa.CallSite    { return nil }

// StringProperty has every method read take its receivers for strings.
func (globals) StringProperty(pc int) (ssa.PropertySite, bool) {
	return ssa.PropertySite{Key: 9, Shape: 0x10000 + uintptr(pc), Index: int32(pc % 7)}, true
}

// The backend's code (internal/jit/mir) keeps the rules, for every function
// its random and JavaScript corpora compile.
func TestMirEmitted(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000}
	cells := map[ssa.Op]int{}
	compile := func(name string, p *ir.Program, fb ssa.Feedback) bool {
		f, err := ssa.BuildWith(p, fb)
		if err != nil {
			return false
		}
		ssa.Optimize(f)
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				cells[v.Op]++
			}
		}
		for _, arch := range arches {
			compile := mir.CompileAMD64
			if arch == "arm64" {
				compile = mir.CompileARM64
			}
			mc, err := compile(f, enc)
			if err != nil {
				t.Fatalf("%s, %s: %v", name, arch, err)
			}
			if err := check(arch, mc.Bytes); err != nil {
				t.Fatalf("%s, %s: %v", name, arch, err)
			}
		}
		return true
	}
	n := 0
	for _, src := range jsCorpus {
		for i, p := range lowered(t, src) {
			if compile(fmt.Sprintf("%s (%d)", src, i), p, nil) {
				n++
			}
			if compile(fmt.Sprintf("%s (%d, globals)", src, i), p, globals{}) {
				n++
			}
		}
	}
	r := rand.New(rand.NewPCG(5, 6))
	for attempt := 0; attempt < 50000 && n < 2000; attempt++ {
		p := randomProgram(r)
		if p.Validate() == nil && compile(fmt.Sprintf("random %d", attempt), p, nil) {
			n++
		}
	}
	if n < 500 {
		t.Fatalf("only %d functions compiled", n)
	}
	t.Logf("%d functions; checked %v bytes, %v instructions", n, checkedBytes, checkedInsts)
	if cells[ssa.OpGlobalCell] == 0 || cells[ssa.OpLexCell] == 0 || cells[ssa.OpStringProp] == 0 {
		t.Fatalf("global cells %d, lexical cells %d, string methods %d", cells[ssa.OpGlobalCell], cells[ssa.OpLexCell], cells[ssa.OpStringProp])
	}
}
