//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package verify

import (
	"math"
	"math/big"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regCodes is the register code each program the tests lowered was
// lowered from, which regShape and the feedback fakes read: what the VM
// keeps beside a program (its regCode); and regFns its function.
var (
	regCodes = map[*ir.Program]*bytecode.RegCode{}
	regFns   = map[*ir.Program]*bytecode.Function{}
)

// regLowerForTest lowers fn's register code for an entry.
func regLowerForTest(fn *bytecode.Function) (*ir.Program, error) {
	p, err := jitcompile.LowerReg(fn.RegCode(), fn)
	if err == nil {
		regCodes[p], regFns[p] = fn.RegCode(), fn
	}
	return p, err
}

// regLowerInlineForTest lowers fn's register code for a native caller or
// inlining.
func regLowerInlineForTest(fn *bytecode.Function) (*ir.Program, error) {
	p, err := jitcompile.LowerRegInline(fn.RegCode(), fn)
	if err == nil {
		regCodes[p], regFns[p] = fn.RegCode(), fn
	}
	return p, err
}

// regLowerWithForTest is regLowerForTest with hints, by register pc.
func regLowerWithForTest(fn *bytecode.Function, h jitcompile.Hints) (*ir.Program, error) {
	p, err := jitcompile.LowerRegWith(fn.RegCode(), fn, h)
	if err == nil {
		regCodes[p], regFns[p] = fn.RegCode(), fn
	}
	return p, err
}

// regShape sets f, built from p, register code's program, for the register
// frame as the VM does (its jitRegShape): the window's registers are the
// frame's locals; the captured bindings and a derived constructor's this
// past them, in their cells and the context; the expansions' scratch slots
// past those, which are only values; the parameters from register 2; and
// the bindings the call makes undefined after them. The entries the VM asks
// for are the caller's to choose. A program of the stack form's is left as
// Build made it.
func regShape(f *ssa.Func, p *ir.Program) {
	if p.Origin == nil {
		return
	}
	f.FrameLocals = p.Regs
	base := p.UpvalueBase + p.Upvalues
	f.ThisSlot = -1
	if p.This {
		f.ThisSlot = base
		base++
	}
	f.Scratch = p.Locals - base
	f.Params, f.StartOnly = p.ParamBase+p.Params, false
	f.InitEnd = f.Params
	if rc := regCodes[p]; rc != nil {
		f.InitEnd += rc.Inits
	}
	f.Cells = p.Cells
}

// regInstr is the register instruction program pc pc of p was lowered
// from, and whether p is register code's that the tests lowered.
func regInstr(p *ir.Program, pc int) (bytecode.RInstr, bool) {
	rc := regCodes[p]
	if rc == nil || pc < 0 || pc >= len(p.Origin) {
		return 0, false
	}
	return rc.Code[p.Origin[pc]], true
}

// regElementReads are rc's element reads, by register pc, which the VM
// learns to read as what the element holds (jitcompile.Hints'
// ElementReads, by register pc, the VM's jitRegIndexSeen): the register
// form's get_index, of a register's key, a constant's, or a step's.
func regElementReads(rc *bytecode.RegCode) []int32 {
	var pcs []int32
	for pc, w := range rc.Code {
		switch w.Op() {
		case bytecode.RGetIndex, bytecode.RGetIndexI, bytecode.RGetIndexPre, bytecode.RGetIndexPost:
			pcs = append(pcs, int32(pc))
		}
	}
	return pcs
}

// regCall reports whether program pc pc of p is register code's call --
// call_w's, call_method_w's, new_w's or their tail position's, a Window
// Host -- and its arguments and whether it is a method's, as the VM's
// NativeCalls reads them (its nativeCalls): the stack form's call read its
// operands' count from the states' depths, which register code's are all 0.
func regCall(p *ir.Program, pc int) (argc int, method, ok bool) {
	w, ok := regInstr(p, pc)
	if !ok || p.Code[pc].Op != ir.Host || !p.Code[pc].Window {
		return 0, false, false
	}
	switch w.Op() {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW, bytecode.RCallWT, bytecode.RCallMWT:
		return int(w.X()), w.Op().MethodCall(), true
	}
	return 0, false, false
}

// regCallee is a native call's site of p's call at pc, of a function of
// params parameters and regs registers, as the VM's regCallSite makes it
// for register code: its parameters past the callee's function and this,
// its window at the call's first register.
func regCallee(p *ir.Program, pc, argc, params, regs int, method bool) ssa.CallSite {
	return ssa.CallSite{Callee: 0x50000, Closure: 0x51000, Entry: 0x52000, Count: 0x53000, Argc: argc, Method: method,
		Params: 2 + params, LocalCount: regs, ThisSlot: -1, Window: p.Code[pc].Left.Slot + 1}
}

// regCalleeName is the name a call of p at pc, register code's, calls by:
// the property or the global binding read into its window's first
// register, through the copies after the read, on the way to the call
// alone; "" if none is found.
func regCalleeName(p *ir.Program, pc int) string {
	rc, fn := regCodes[p], regFns[p]
	if rc == nil || p.Code[pc].Op != ir.Host || !p.Code[pc].Window {
		return ""
	}
	names := fn.Names
	if rc.Names != nil {
		names = rc.Names
	}
	slot := p.Code[pc].Left.Slot
	for q := pc - 1; q >= 0; q-- {
		in := &p.Code[q]
		switch in.Op {
		case ir.Jump, ir.Branch, ir.Host, ir.Call, ir.Return:
			return ""
		}
		if in.Dest != slot {
			continue
		}
		switch in.Op {
		case ir.Copy:
			if in.Left.Slot < 0 {
				return ""
			}
			slot = in.Left.Slot
		case ir.PropertyRead, ir.ReferenceRead, ir.BindingRead:
			if int(in.Key) < len(names) {
				return names[in.Key]
			}
			return ""
		default:
			return ""
		}
	}
	return ""
}

// regWhole reports whether program pc pc of p is register code's
// instruction Go runs whole, called from native code -- a Whole Host, or
// define_field's FieldDefine -- as the VM's NativeCalls finds one (its
// nativeCalls), and its register instruction and operands' count.
func regWhole(p *ir.Program, pc int) (bytecode.RInstr, int, bool) {
	w, ok := regInstr(p, pc)
	in := &p.Code[pc]
	if !ok || !(in.Op == ir.Host && in.Whole || in.Op == ir.FieldDefine && w.Op() == bytecode.RDefineField) {
		return 0, 0, false
	}
	_, argc, _, ok := in.WholeOperands(p.Regs)
	return w, argc, ok
}

// regBigConst is the BigInt constant program pc pc of p, register code's
// BigIntConst of load_const or of a literal's elements, reads, as the VM's
// constant reads it.
func regBigConst(p *ir.Program, pc int) (*big.Int, bool) {
	w, ok := regInstr(p, pc)
	if !ok || p.Code[pc].Op != ir.BigIntConst {
		return nil, false
	}
	switch w.Op() {
	case bytecode.RLoadConst, bytecode.RSwitch, bytecode.RNewArrayK:
	default:
		return nil, false
	}
	consts := regFns[p].Constants
	if rc := regCodes[p]; rc.Constants != nil {
		consts = rc.Constants
	}
	k := int(p.Code[pc].Key)
	if k >= len(consts) || consts[k].Kind != bytecode.ConstBigInt {
		return nil, false
	}
	x, ok := new(big.Int).SetString(consts[k].Str, 0)
	return x, ok
}

// regConstOperand is the number p's instruction pc, a window's call, finds
// in slot, its window's register, as the VM's regConstOperand finds it: a
// constant copied there before the call, with nothing between that may
// write the slot, nor jumps or is jumped to, nor an entry, nor a handler.
func regConstOperand(p *ir.Program, pc, slot int) (float64, bool) {
	q := pc - 1
	for ; q >= 0; q-- {
		in := &p.Code[q]
		switch {
		case in.Op == ir.Jump, in.Op == ir.Branch, in.Op == ir.IterStep, in.Op == ir.Call,
			in.Op == ir.Host && !in.Window && !in.Whole:
			return 0, false
		}
		if in.Dest == slot {
			break
		}
	}
	if q < 0 {
		return 0, false
	}
	in := &p.Code[q]
	if in.Op != ir.Copy || in.Left.Slot >= 0 || in.Left.Literal.Kind != ir.Number {
		return 0, false
	}
	for k := range p.Code {
		if t := &p.Code[k]; (t.Op == ir.Jump || t.Op == ir.Branch || t.Op == ir.IterStep) && t.Target > q && t.Target <= pc {
			return 0, false
		}
	}
	for _, e := range p.Entries {
		if e > q && e <= pc {
			return 0, false
		}
	}
	for _, h := range p.Handlers {
		if int(h.PC) > q && int(h.PC) <= pc {
			return 0, false
		}
	}
	return math.Float64frombits(in.Left.Literal.Bits), true
}
