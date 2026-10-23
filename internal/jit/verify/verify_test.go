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

// Every compile checks its register allocation (mir.CheckAllocations).
func init() { mir.CheckAllocations = true }

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

// Accessor has every property read and write call an accessor found on a
// prototype, natively, so that the code of its checks and its call is
// checked: a getter's at even PCs, a setter's at odd ones, which the
// builder takes where the operation is a read or a write.
func (globals) Accessor(pc int) (ssa.AccessorSite, bool) {
	site := ssa.PropertySite{Key: 9, Shape: 0x20000 + uintptr(pc), Index: int32(pc % 5),
		Holders: [2]ssa.Holder{{Object: 0x30000, Shape: 0x40000}}}
	if pc%3 == 0 {
		site.Cases = []ssa.PropertyCase{{Shape: 0x28000, Index: 2, Holders: [2]ssa.Holder{{Object: 0x30000, Shape: 0x40000}, {Object: 0x31000, Shape: 0x41000}}}}
	}
	call := &ssa.CallSite{Callee: 0x50000, Closure: 0x51000, Entry: 0x52000, Count: 0x53000, Params: 1, LocalCount: 2, MaxStack: 4, ThisSlot: 2}
	return ssa.AccessorSite{Property: site, Function: 0x50000, Setter: pc%2 == 1, Call: call}, true
}

// StringProperty has every method read take its receivers for strings.
func (globals) StringProperty(pc int) (ssa.PropertySite, bool) {
	return ssa.PropertySite{Key: 9, Shape: 0x10000 + uintptr(pc), Index: int32(pc % 7)}, true
}

// bigCorpus is BigInt arithmetic of 64 bits, which bigs has made natively
// (ssa's bigint.go).
var bigCorpus = []string{
	`function f(n,x){for(let i=0;i<n;i++){x^=BigInt.asUintN(64,x<<13n);x^=x>>7n;x^=BigInt.asIntN(64,x<<17n)}return x}`,
	`function f(n,s){let h=0xcbf29ce484222325n;for(let i=0;i<n;i++)h=BigInt.asUintN(64,(h^BigInt(s.charCodeAt(i)))*0x100000001b3n);return h}`,
	`function f(n,x){let s=0n;while(x>0n){s+=x%10n;x/=10n;x=-x;x=~x}return s}`,
	`function f(a,b){let c=0;for(let i=0;i<9;i++){if(a<b)c++;a=(a&b)|(a^b);b=b<<3n;a=a>>2n;a=a-b*a+0xffffffffffffffffn}return c}`,
	`function f(n,x){let s=0n;for(let i=0;i<n;i++)s+=x**3n+BigInt(i)**BigInt(n);return s}`,
	`function f(n,x){let s="";for(let i=0;i<n;i++)s=(x+BigInt(i)).toString();return s}`,
	`function f(n,x){let s="";for(let i=0;i<n;i++)s=x.toFixed(2)+x.toString(16)+x.valueOf()+x.toPrecision(3);return s}`,
}

// bigs is globals with every operator's site one that met BigInts held in
// their values, as Uint64s at odd PCs, every BigInt constant one of 64 bits
// or fewer, and every call of two arguments BigInt.asUintN's or asIntN's,
// of one BigInt's.
type bigs struct {
	globals
	p *ir.Program
}

func (bigs) BigInt(int) bool        { return true }
func (bigs) BigIntWide(pc int) bool { return pc%2 == 1 }
func (bigs) BigIntFailed(int) bool  { return false }
func (bigs) GoCalls() bool          { return true }

// Accessor has no read call an accessor: each is a method's read.
func (bigs) Accessor(int) (ssa.AccessorSite, bool) { return ssa.AccessorSite{}, false }

// StringProperty has every method read another primitive's than a
// string's, off a prototype: a BigInt's, a number's or a boolean's, by its
// PC.
func (bigs) StringProperty(pc int) (ssa.PropertySite, bool) {
	kind := [3]ssa.Primitive{ssa.PrimitiveBigInt, ssa.PrimitiveNumber, ssa.PrimitiveBoolean}[pc%3]
	return ssa.PropertySite{Key: 9, Shape: 0x10000 + uintptr(pc), Index: int32(pc % 7), Holders: [2]ssa.Holder{{Object: 0x63000, Shape: 0x10000 + uintptr(pc)}}, Primitive: kind}, true
}
func (bigs) BigIntCell(pc int) (uintptr, uint64, int, bool) {
	if pc%3 == 0 {
		return 0x61000 + uintptr(pc)*16, 1<<63 + uint64(pc), ssa.BigConstUint64, true
	}
	return 0x61000 + uintptr(pc)*16, uint64(pc % 40), ssa.BigConstInt64, true
}
func (b bigs) Intrinsic(pc int) (ssa.Intrinsic, bool) {
	k := ssa.Intrinsic{Callee: 0x62000, Op: ssa.OpZextW, Fn: 64}
	if pc+1 < len(b.p.Maps) && b.p.Maps[pc+1].Depth == b.p.Maps[pc].Depth-1 {
		// A call of one argument and no receiver.
		k.Op, k.Fn, k.Plain = ssa.OpBigOfF64, 0, true
		return k, true
	}
	switch pc % 3 {
	case 1:
		k.Op, k.Fn = ssa.OpSextW, 13
	case 2:
		k.Fn = 0
	}
	return k, true
}

// genCorpus is generators a loop's step resumes natively (ssa.Func's
// GenEntry, Block's Yield) and the steps that resume them (ssa.CallSite's
// Gens): for...of loops and destructurings, with operands beneath a yield
// and none, handlers in force at a yield and not, a receiver read.
var genCorpus = []string{
	`function* f(n){for(let i=0;i<n;i++)yield i*2}`,
	`function* f(a){for(const x of a)yield x+1;return 3}`,
	`function* f(n){let s=0;while(n-->0){s+=(yield s)|0}return s}`,
	`function* f(n){try{for(let i=0;i<n;i++)yield i}finally{n=0}}`,
	`var o={*f(n){for(let i=0;i<n;i++)yield this.k+i}}`,
	`function f(g,n){let s=0;for(let j=0;j<n;j++)for(const v of g)s=(s+v)|0;return s}`,
	`function f(g){let s=0;for(const v of g){const [a,b]=v;s+=a*b}return s}`,
}

// gens is globals whose loops' steps have met generators of one function
// (ssa.GenStepFeedback), which they resume by a native call.
type gens struct{ globals }

func (gens) GoCalls() bool { return true }
func (gens) GenSteps(int) []ssa.GenStep {
	return []ssa.GenStep{{Fn: 0x80000, Env: 0x81000, Realm: 0x82000, Entry: 0x83000, Count: 0x83010}}
}
func (gens) GenNext() uintptr { return 0x84000 }

// genResumes is where code built as f, of p, a generator's, resumes it
// natively (ssa.Func's GenResumes), as the VM finds them: each entry after a
// yield, and the first other as its start.
func genResumes(p *ir.Program, f *ssa.Func) []ssa.GenResume {
	var out []ssa.GenResume
	start := false
	for _, e := range f.Entries {
		switch {
		case e.PC > 0 && p.Code[e.PC-1].Suspend && e.Depth >= 1:
			out = append(out, ssa.GenResume{PC: e.PC, Saved: e.Depth - 1, Started: true})
		case !start:
			start = true
			out = append(out, ssa.GenResume{PC: e.PC, Saved: e.Depth})
		}
	}
	return out
}

// spreadCorpus is object and array spread, a literal's fields after a
// spread and an array literal's elements, which native code makes itself
// where it can (mir's copyProps, defineNext, spreadElems, appendElem) and
// calls Go for otherwise.
var spreadCorpus = []string{
	`function f(o,n){let s=0,x;for(let i=0;i<n;i++){x={...o,k:i};s+=x.k}return x}`,
	`function f(a,n){let s=0,x;for(let i=0;i<n;i++){x=[i,...a,7];s+=x.length}return x}`,
	`function f(a,b,n){let x;for(let i=0;i<n;i++)x=Math.max(...a,...b,i);return x}`,
	`function f(o,p){return{a:1,...o,...p,b:2}}`,
}

// spreads is globals whose operations Go makes, by its bytecode, are the
// VM's sites of them (ssa.CallSite's Instr), with what native code makes
// them by: the table of object spreads by layout, the shapes' first
// transitions, the arrays' iteration's methods, room to append into.
type spreads struct {
	globals
	fn *bytecode.Function
}

func (spreads) GoCalls() bool { return true }
func (s spreads) NativeCalls(pc int) []ssa.CallSite {
	in := s.fn.Code[pc]
	if !jitcompile.GoOperation(in.Op) {
		return nil
	}
	site := ssa.CallSite{Go: ssa.GoCall, Instr: int(in.Op) | int(in.A)<<8, Argc: jitcompile.GoOperands(in), ThisSlot: -1}
	switch in.Op {
	case bytecode.OpCopyDataProps:
		site.Copies = 0x90000
	case bytecode.OpDefineField:
		site.DefineNext, site.Key = true, 77
	case bytecode.OpArraySpread, bytecode.OpSpreadIter:
		site.ArrayIter = 0x91000
	case bytecode.OpArrayPush:
		site.Append = true
	}
	return []ssa.CallSite{site}
}

// inCorpus is the in operator, whose answers Go makes from native code
// (ssa.GoHas) and native code itself where the object is an array and the
// key an index (mir's keyedHas).
var inCorpus = []string{
	`function f(a,n){let s=0;for(let i=0;i<n;i++){if(i in a)s++;if((i&7) in a)s+=2;if("x" in a)s--}return s}`,
	`function f(o,k,n){let s=0;for(let i=0;i<n;i++){if(k in o)s+=i;if(o.x in o)s--}return s}`,
}

// hases is globals whose in operators Go answers from native code, and,
// with protos, whose arrays' prototypes are known (ssa.Func's ArrayProtos),
// so that keyedHas answers an absent index too, and whose VM has a cache of
// the operator's answers (ssa.Func's HasCache), which it looks a key
// string up in.
type hases struct {
	globals
	protos bool
}

func (hases) GoCalls() bool { return true }
func (h hases) ArrayProtos() ([2]ssa.Holder, bool) {
	if !h.protos {
		return [2]ssa.Holder{}, false
	}
	return [2]ssa.Holder{{Object: 0x64000, Shape: 0x65000}, {Object: 0x66000, Shape: 0x67000}}, true
}
func (h hases) HasCache() uintptr {
	if !h.protos {
		return 0
	}
	return 0x68000
}

// The backend's code (internal/jit/mir) keeps the rules, for every function
// its random and JavaScript corpora compile.
func TestMirEmitted(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Mega: true, UndefinedCell: 0x60000,
		// Never run: an address for the stores to jump to while the
		// collector marks.
		StoreRef: 0x70000}
	cells := map[ssa.Op]int{}
	primProps := map[ssa.Primitive]int{}
	// hasCalls counts the in operators Go answers from native code, with
	// and without the arrays' prototypes and the cache known.
	hasCalls := map[bool]int{}
	compile := func(name string, p *ir.Program, fb ssa.Feedback) bool {
		f, err := ssa.BuildWith(p, fb)
		if err != nil {
			return false
		}
		// Its call entry, which native callers jump to, as the VM asks.
		f.CallEntry = true
		ssa.Optimize(f)
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				cells[v.Op]++
				if calls := v.Calls(); len(calls) == 1 && calls[0].Go == ssa.GoHas {
					hasCalls[f.ArrayProtos[0].Object != 0 && f.HasCache != 0]++
				}
				if v.Op == ssa.OpStringProp {
					primProps[v.Primitive()]++
				}
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
			// Past Text, the code's constants, which are data.
			if err := check(arch, mc.Bytes[:mc.Text]); err != nil {
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
	for _, src := range bigCorpus {
		for i, p := range lowered(t, src) {
			if compile(fmt.Sprintf("%s (%d, bigs)", src, i), p, bigs{p: p}) {
				n++
			}
		}
	}
	for _, src := range inCorpus {
		for i, p := range lowered(t, src) {
			for _, protos := range []bool{false, true} {
				if compile(fmt.Sprintf("%s (%d, hases %v)", src, i, protos), p, hases{protos: protos}) {
					n++
				}
			}
		}
	}
	// Spreads, of an encoding that has what native code makes them by.
	spreadEnc := enc
	spreadEnc.WriteBarrier, spreadEnc.Object, spreadEnc.ShapeNext, spreadEnc.ShapeNextKey, spreadEnc.ShapeNextFlags = 0x71000, 0xFFFC000000000000, 40, 48, 52
	spreadEnc.ShapeIndex, spreadEnc.LinearScan, spreadEnc.PropDefault, spreadEnc.ObjectShape, spreadEnc.ObjectProps = 56, 8, 7, 8, 16
	spreadEnc.ObjectElems, spreadEnc.ObjectClass, spreadEnc.ObjectFlags, spreadEnc.ObjectProto, spreadEnc.ClassArray = 64, 1, 2, 40, 1
	spreadEnc.FlagSparse, spreadEnc.FlagExtensible = 4, 1
	natives := map[string]int{}
	for _, src := range spreadCorpus {
		prog, err := parser.Parse(src, parser.Options{})
		if err != nil {
			t.Fatal(err)
		}
		top, err := compiler.Compile(prog, compiler.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range top.Constants {
			if c.Kind != bytecode.ConstFunction {
				continue
			}
			for _, lower := range []func(*bytecode.Function) (*ir.Program, error){jitcompile.LowerSSA, jitcompile.LowerSSAInline} {
				p, err := lower(c.Fn)
				if err != nil {
					continue
				}
				f, err := ssa.BuildWith(p, spreads{fn: c.Fn})
				if err != nil {
					t.Fatalf("%s: %v", src, err)
				}
				f.CallEntry = true
				ssa.Optimize(f)
				for _, b := range f.Blocks {
					for _, v := range b.Values {
						for _, site := range v.Calls() {
							switch {
							case site.Copies != 0:
								natives["copies"]++
							case site.DefineNext:
								natives["defines"]++
							case site.ArrayIter != 0:
								natives["spreads"]++
							case site.Append:
								natives["appends"]++
							}
						}
					}
				}
				for _, arch := range arches {
					compile := mir.CompileAMD64
					if arch == "arm64" {
						compile = mir.CompileARM64
					}
					mc, err := compile(f, spreadEnc)
					if err != nil {
						t.Fatalf("%s (spreads), %s: %v", src, arch, err)
					}
					if err := check(arch, mc.Bytes[:mc.Text]); err != nil {
						t.Fatalf("%s (spreads), %s: %v", src, arch, err)
					}
				}
				n++
			}
		}
	}
	for _, k := range []string{"copies", "defines", "spreads", "appends"} {
		if natives[k] == 0 {
			t.Errorf("no native %s compiled", k)
		}
	}
	// A generator's layout, and a cursor's, for the generators' code and
	// the steps that resume them.
	genEnc := enc
	genEnc.IterState, genEnc.IterDone, genEnc.IterArr, genEnc.IterIdx, genEnc.IterIter, genEnc.IterNext = 0x1111, 8, 16, 24, 32, 48
	genEnc.ObjectExt, genEnc.ObjectData, genEnc.ObjectElems = 40, 8, 64
	genEnc.GenData, genEnc.GenClosure, genEnc.GenThis, genEnc.GenVals, genEnc.GenArgs = 0x2222, 0, 8, 24, 48
	genEnc.GenStack, genEnc.GenPC, genEnc.GenHandlers, genEnc.GenRare = 56, 80, 88, 112
	genEnc.GenState, genEnc.GenStarted, genEnc.GenDelegating = 120, 121, 122
	genEnc.GenSuspendedYield, genEnc.GenExecuting, genEnc.GenCompleted = 1, 2, 3
	genEnc.ClosureFn, genEnc.ClosureEnv, genEnc.ClosureRealm, genEnc.ClosureUpvalues = 0, 8, 16, 24
	genEnc.LinkIn, genEnc.LinkGen, genEnc.WriteBarrier, genEnc.CallGo = 16, 72, 0x71000, 0x72000
	genEnc.HandlerSize, genEnc.HandlerPC, genEnc.HandlerDepth, genEnc.HandlerFinally = 24, 0, 8, 16
	genEntries, genYields, genSteps := 0, 0, 0
	for _, src := range genCorpus {
		for i, p := range lowered(t, src) {
			f, err := ssa.BuildWith(p, gens{})
			if err != nil {
				continue
			}
			for _, in := range p.Code {
				if in.Suspend {
					f.GenEntry = true
				}
			}
			if f.GenEntry {
				f.GenResumes = genResumes(p, f)
			}
			ssa.Optimize(f)
			for _, b := range f.Blocks {
				if b.Yield && f.GenEntry {
					genYields++
				}
				for _, v := range b.Values {
					if calls := v.Calls(); len(calls) == 1 && len(calls[0].Gens) != 0 {
						genSteps++
					}
				}
			}
			for _, arch := range arches {
				compile := mir.CompileAMD64
				if arch == "arm64" {
					compile = mir.CompileARM64
				}
				mc, err := compile(f, genEnc)
				if err != nil {
					t.Fatalf("%s (%d, gens), %s: %v", src, i, arch, err)
				}
				if err := check(arch, mc.Bytes[:mc.Text]); err != nil {
					t.Fatalf("%s (%d, gens), %s: %v", src, i, arch, err)
				}
				if mc.GenEntry >= 0 {
					genEntries++
				}
			}
			n++
		}
	}
	if genEntries == 0 || genYields == 0 || genSteps == 0 {
		t.Errorf("generators' entries %d, native yields %d, steps resuming generators %d compiled", genEntries, genYields, genSteps)
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
	for _, ops := range [][]ssa.Op{{ssa.OpBigUnboxU64}, {ssa.OpBigLow}, {ssa.OpXorW}, {ssa.OpShlW}, {ssa.OpShrW, ssa.OpSarW}, {ssa.OpZextW},
		{ssa.OpSextW}, {ssa.OpDivI64, ssa.OpDivU64}, {ssa.OpModI64, ssa.OpModU64}, {ssa.OpBigOfF64}, {ssa.OpBigBox}, {ssa.OpPowI64}} {
		n := 0
		for _, op := range ops {
			n += cells[op]
		}
		if n == 0 {
			t.Errorf("none of %v compiled", ops)
		}
	}
	if hasCalls[false] == 0 || hasCalls[true] == 0 {
		t.Errorf("in operators compiled, without and with the arrays' prototypes: %d, %d", hasCalls[false], hasCalls[true])
	}
	for _, k := range []ssa.Primitive{ssa.PrimitiveBigInt, ssa.PrimitiveNumber, ssa.PrimitiveBoolean} {
		if primProps[k] == 0 {
			t.Errorf("no method read of a primitive of kind %d compiled", k)
		}
	}
	if cells[ssa.OpGlobalCell] == 0 || cells[ssa.OpLexCell] == 0 || cells[ssa.OpStringProp] == 0 || cells[ssa.OpAccessorIs] == 0 {
		t.Fatalf("global cells %d, lexical cells %d, string methods %d, accessors %d", cells[ssa.OpGlobalCell], cells[ssa.OpLexCell],
			cells[ssa.OpStringProp], cells[ssa.OpAccessorIs])
	}
}
