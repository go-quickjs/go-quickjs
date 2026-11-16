//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The feedback-parity check of the register form's JIT against the stack
// form's (docs/register-interpreter.md, Stage 3): a program run in two
// runtimes, one on the stack form's tiers and its JIT, one on the register
// tier and the JIT that compiles register code, warmed alike; then, for
// every function both compiled, what each form's feedback answers --
// jitFeedback's by stack pc, jitRegFeedback's by register pc -- is
// compared site by site, a register instruction matched with the stack
// instruction its source position is (regCode's src), and what each
// form's build decides from it, function by function. Only tests run it
// (TestJITFeedbackParity, internal/regdiff).

// jitParityLog, set by RecordJITCompiles, has every runtime note the
// closures it compiles, by form.
var jitParityLog struct {
	sync.Mutex
	on   atomic.Bool
	seen map[*Runtime]map[*bytecode.Function]*closure
	// order numbers each runtime's functions as it first compiled them,
	// which pairs functions of the same name and text (CompareFeedback).
	order map[*Runtime]map[*bytecode.Function]int
}

// RecordJITCompiles has every runtime note the closures it compiles, of
// either form, for CompareFeedback, or stop and forget them.
func RecordJITCompiles(on bool) {
	jitParityLog.Lock()
	defer jitParityLog.Unlock()
	jitParityLog.on.Store(on)
	jitParityLog.seen, jitParityLog.order = nil, nil
}

// jitParityNote notes that r compiles cl's function (RecordJITCompiles).
func (r *Runtime) jitParityNote(cl *closure) {
	if cl == nil {
		return
	}
	jitParityLog.Lock()
	defer jitParityLog.Unlock()
	if !jitParityLog.on.Load() {
		return
	}
	if jitParityLog.seen == nil {
		jitParityLog.seen = map[*Runtime]map[*bytecode.Function]*closure{}
		jitParityLog.order = map[*Runtime]map[*bytecode.Function]int{}
	}
	m, o := jitParityLog.seen[r], jitParityLog.order[r]
	if m == nil {
		m, o = map[*bytecode.Function]*closure{}, map[*bytecode.Function]int{}
		jitParityLog.seen[r], jitParityLog.order[r] = m, o
	}
	m[cl.fn] = cl
	if _, ok := o[cl.fn]; !ok {
		o[cl.fn] = len(o)
	}
}

// SeedRandom seeds r's Math.random, so that runtimes given one seed draw
// the same numbers: CompareFeedback's two runs of one program, which
// Math.random steers -- the micro-benchmarks', Octane's before its
// harness replaces it -- and which must make the same compiles from one
// run to the next.
func (r *Runtime) SeedRandom(seed uint64) {
	m := &mathRandom{s0: seed, s1: seed ^ 0x9e3779b97f4a7c15}
	if m.s0 == 0 && m.s1 == 0 {
		m.s1 = 1
	}
	r.rnd = m
}

// FeedbackParity is what CompareFeedback found.
type FeedbackParity struct {
	// Functions is how many functions both runtimes compiled and were
	// compared; Answers how many non-empty answers of the stack form's
	// feedback there were, Matched how many the register form's answered
	// alike at a register instruction of the same source position.
	Functions, Answers, Matched int
	// FrameOps counts the functions whose register form's build makes a
	// call of Go on its frame -- ssa.GoHost, GoArgument, GoCells,
	// GoClosure, GoClose -- and FrameOpsLeft those of them with one that
	// native code of a register frame leaves to an exit (jitRegFrameOps).
	FrameOps, FrameOpsLeft int
	// Paired counts the answers matched with register code of another
	// shape that does the same work, by method and what the register code
	// does instead; Matched counts them too.
	Paired map[string]int
	// Gaps counts the answers not matched, by method and why: the
	// register form's feedback has no such method ("unanswered"), no
	// register instruction is at the stack instruction's position ("no
	// instruction"), or its answers there differ ("differs").
	Gaps map[string]int
	// Decisions counts the functions whose builds decided differently, by
	// what: refused by one form, inlined, host operations left to Go,
	// loops' entries escape analysis dropped.
	Decisions map[string]int
	// Examples are a few of each gap and decision, as text.
	Examples map[string][]string
	// Compiled is how many functions each runtime compiled, the stack
	// form's first; Compared names those compared, with their script and
	// line, in the order compared.
	Compiled [2]int
	Compared []string
}

// String is the report, gaps and decisions most first.
func (p FeedbackParity) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d functions compiled by both forms; %d answers of the stack form's feedback, %d answered alike by the register form's\n",
		p.Functions, p.Answers, p.Matched)
	fmt.Fprintf(&b, "%d of them make calls of Go on the register frame, %d one left to an exit there\n", p.FrameOps, p.FrameOpsLeft)
	fmt.Fprintf(&b, "compiled: %d functions by the stack form, %d by the register form\n", p.Compiled[0], p.Compiled[1])
	for _, part := range []struct {
		title string
		m     map[string]int
	}{{"paired with register code of another shape", p.Paired}, {"gaps", p.Gaps}, {"decisions that differ", p.Decisions}} {
		keys := slices.SortedFunc(func(yield func(string) bool) {
			for k := range part.m {
				if !yield(k) {
					return
				}
			}
		}, func(a, b string) int {
			if c := cmp.Compare(part.m[b], part.m[a]); c != 0 {
				return c
			}
			return strings.Compare(a, b)
		})
		fmt.Fprintf(&b, "%s:\n", part.title)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %7d %s\n", part.m[k], k)
			for _, ex := range p.Examples[k] {
				fmt.Fprintf(&b, "            %s\n", ex)
			}
		}
	}
	return b.String()
}

// CompareFeedback compares the feedback of stack's JIT, a runtime that ran
// the stack form, with reg's, a runtime that ran the same program,
// compiled again, on the register tier, for every function both compiled
// since RecordJITCompiles: a function is the same in both where its name
// and its source text are.
func CompareFeedback(stack, reg *Runtime) FeedbackParity {
	jitParityLog.Lock()
	ss, rs := jitParityLog.seen[stack], jitParityLog.seen[reg]
	so, ro := jitParityLog.order[stack], jitParityLog.order[reg]
	jitParityLog.Unlock()
	res := FeedbackParity{Paired: map[string]int{}, Gaps: map[string]int{}, Decisions: map[string]int{}, Examples: map[string][]string{}, Compiled: [2]int{len(ss), len(rs)}}
	note := func(m map[string]int, k, ex string) {
		m[k]++
		if len(res.Examples[k]) < 3 {
			res.Examples[k] = append(res.Examples[k], ex)
		}
	}
	key := func(fn *bytecode.Function) string { return fn.Name + "\x00" + fn.Text }
	// Each runtime's functions by key and in the order they were first
	// compiled: functions of one name and text -- an eval's, a function
	// made of code made anew -- are paired in that order, the k-th of the
	// stack form's with the k-th of the register form's.
	sorted := func(seen map[*bytecode.Function]*closure, order map[*bytecode.Function]int) []*bytecode.Function {
		return slices.SortedFunc(func(yield func(*bytecode.Function) bool) {
			for fn := range seen {
				if !yield(fn) {
					return
				}
			}
		}, func(a, b *bytecode.Function) int {
			if c := strings.Compare(key(a), key(b)); c != 0 {
				return c
			}
			return cmp.Compare(order[a], order[b])
		})
	}
	byKey := map[string][]*closure{}
	for _, fn := range sorted(rs, ro) {
		byKey[key(fn)] = append(byKey[key(fn)], rs[fn])
	}
	nth := map[string]int{}
	for _, fn := range sorted(ss, so) {
		k := key(fn)
		scl, rcls, i := ss[fn], byKey[k], nth[k]
		nth[k]++
		if i >= len(rcls) || rcls[i].reg == nil {
			continue
		}
		rcl := rcls[i]
		se := stack.jit.cache[stack.jitKeyOf(fn)]
		if se == nil || se.ssaProg == nil {
			continue
		}
		sp := se.ssaProg
		rp, err := jitcompile.LowerReg(rcl.reg.rc, rcl.fn)
		if err != nil {
			continue
		}
		res.Functions++
		jf := jitFunctionOf(fn)
		res.Compared = append(res.Compared, fmt.Sprintf("%s %s:%d", jf.Name, jf.Script, jf.Line))
		sfb := &jitFeedback{r: stack, fn: fn, cl: scl, e: se, prog: sp}
		rfb := reg.jitRegFeedbackFor(rcl, rp)
		if rfb == nil {
			continue
		}
		name := fn.Name
		if name == "" {
			name = "(anonymous)"
		}
		// The register program's instructions by the stack pc their
		// register instruction's position is, and by that position: a
		// register instruction's is the first stack instruction's of
		// those of its source position (regcomp's finish), so that the
		// instructions of one expression, an object literal's, all have
		// the stack pc of its first.
		byStack := map[int][]int{}
		byPos := map[int32][]int{}
		for j := range rp.Code {
			rpc := int(rp.Origin[j])
			if rpc < len(rcl.reg.src) {
				s := int(rcl.reg.src[rpc])
				byStack[s] = append(byStack[s], j)
				if pos, ok := fn.PosAt(uint32(s)); ok {
					byPos[pos] = append(byPos[pos], j)
				}
			}
		}
		ordinal := jitParityOrdinals(sp, rp)
		sv, rv := reflect.ValueOf(sfb), reflect.ValueOf(rfb)
		st := sv.Type()
		stext := func(m reflect.Value, args []reflect.Value) (string, bool) { return jitParityCall(stack, m, args) }
		rtext := func(m reflect.Value, args []reflect.Value) (string, bool) { return jitParityCall(reg, m, args) }
		for m := range st.NumMethod() {
			meth := st.Method(m)
			mt := meth.Type
			pcArg := mt.NumIn() == 2 && mt.In(1).Kind() == reflect.Int
			if !pcArg && mt.NumIn() != 1 || mt.NumOut() == 0 {
				continue
			}
			rm := rv.MethodByName(meth.Name)
			if meth.Name == "BigIntSole" {
				// Compared by what the builds decide of it
				// (jitParityDecisions): each form numbers its bindings its
				// own way.
				continue
			}
			if !pcArg {
				call := func(text func(reflect.Value, []reflect.Value) (string, bool), v reflect.Value) (string, bool) {
					return text(v, nil)
				}
				a, ok := call(stext, sv.Method(m))
				if !ok || a == "" {
					continue
				}
				res.Answers++
				switch b, _ := call(rtext, rm); {
				case !rm.IsValid():
					note(res.Gaps, meth.Name+": unanswered", fmt.Sprintf("%s: %s", name, a))
				case b != a:
					note(res.Gaps, meth.Name+": differs", fmt.Sprintf("%s: stack %s, register %s", name, a, b))
				default:
					res.Matched++
				}
				continue
			}
			asked, known := jitParityMethods[meth.Name]
			for s := range sp.Code {
				if known && !jitParityAsked(asked, sp.Code[s].Op) {
					// The build never asks the method there (jitSnapOps):
					// its answer, whatever it is, is no feedback.
					continue
				}
				a, ok := stext(sv.Method(m), []reflect.Value{reflect.ValueOf(s)})
				if !ok || a == "" {
					continue
				}
				res.Answers++
				where := fmt.Sprintf("%s pc %d (%v)", name, s, jitParityStackOp(fn, s))
				if !rm.IsValid() {
					note(res.Gaps, meth.Name+": unanswered", fmt.Sprintf("%s: %s", where, a))
					continue
				}
				// The register instructions of the same position and of the
				// same operation; a register instruction made of several
				// stack ones -- a constant folded into its user, a method's
				// read and its call -- has the position of one of them: the
				// nearest of the same operation, a few instructions on, those
				// of the same source position, and the literal's of the same
				// rank (jitParityOrdinals); failing those, those of the
				// position, of other operations.
				var js []int
				for d := 0; d <= 8; d++ {
					for _, q := range []int{s + d, s - d} {
						if d == 0 && q != s+d {
							continue
						}
						for _, j := range byStack[q] {
							if rp.Code[j].Op == sp.Code[s].Op && !slices.Contains(js, j) {
								js = append(js, j)
							}
						}
					}
				}
				if pos, ok := fn.PosAt(uint32(s)); ok {
					// Those of the same operation and source position.
					for _, j := range byPos[pos] {
						if rp.Code[j].Op == sp.Code[s].Op && !slices.Contains(js, j) {
							js = append(js, j)
						}
					}
				}
				if j, ok := ordinal[s]; ok && !slices.Contains(js, j) {
					js = append(js, j)
				}
				if sp.Code[s].Op == ir.FieldDefine {
					// A field's definition, of its key: a nested literal's
					// register code defines the inner literal's fields first.
					for j := range rp.Code {
						if rp.Code[j].Op == ir.FieldDefine && rp.Code[j].Key == sp.Code[s].Key && !slices.Contains(js, j) {
							js = append(js, j)
						}
					}
				}
				if len(js) == 0 {
					js = byStack[s]
				}
				if len(js) == 0 {
					if why := jitParityPaired(fn, rcl.reg.rc, meth.Name, s); why != "" {
						res.Matched++
						note(res.Paired, meth.Name+": "+why, where)
						continue
					}
					note(res.Gaps, meth.Name+": no instruction", fmt.Sprintf("%s: %s", where, a))
					continue
				}
				matched := false
				var got string
				for _, j := range js {
					arg := j
					if meth.Name == "Resumed" || meth.Name == "EntryGeneric" {
						// Asked by a state's PC, register code's a register
						// pc.
						arg = int(rp.Maps[j].PC)
					}
					b, _ := rtext(rm, []reflect.Value{reflect.ValueOf(arg)})
					if b == a || meth.Name == "Frequency" && jitParityFrequencyAlike(a, b) {
						matched = true
						break
					}
					if got == "" || b != "" {
						got = fmt.Sprintf("%s at %d (ir op %d, %v)", b, j, rp.Code[j].Op, rcl.reg.rc.Code[rp.Origin[j]].Op())
					}
				}
				if matched {
					res.Matched++
				} else {
					note(res.Gaps, meth.Name+": differs", fmt.Sprintf("%s: stack %s; register %s", where, a, got))
				}
			}
		}
		jitParityDecisions(&res, name, fn, rcl.reg.src, sp, sfb, rp, rfb, note)
	}
	return res
}

// jitParityOrdinals pairs the object and array literals and the object
// literals' fields' definitions of sp, a function's stack form's program,
// with rp's, its register form's, the k-th of each operation with the
// k-th, where each form has as many of it: both make the literals and
// define their fields in about the source's order -- a nested literal's
// register code first -- but register code places a literal's
// instructions at the position of the statement it is in -- an object
// literal returned, at the return -- or at none, where the stack form's
// lie between those of the instructions around them.
func jitParityOrdinals(sp, rp *ir.Program) map[int]int {
	out := map[int]int{}
	for _, op := range [...]ir.Op{ir.ObjectLiteral, ir.FieldDefine, ir.ArrayLiteral} {
		var ss, rs []int
		for i, in := range sp.Code {
			if in.Op == op {
				ss = append(ss, i)
			}
		}
		for j, in := range rp.Code {
			if in.Op == op {
				rs = append(rs, j)
			}
		}
		if len(ss) == len(rs) {
			for k, i := range ss {
				out[i] = rs[k]
			}
		}
	}
	return out
}

// jitParityPaired is why the answer of method at stack pc s of fn's code,
// which no register instruction of rc, its register code, asks, is
// answered by register code of another shape that does the same work, or
// "": a RegExp literal that is a call's first argument, which the
// register call makes in Go (RMarkLitArg), as the stack form's call of Go
// is given the literal it made -- String.prototype's replace, split or
// match, which Go searches with it.
func jitParityPaired(fn *bytecode.Function, rc *bytecode.RegCode, method string, s int) string {
	if method != "Literal" || s >= len(fn.Code) || fn.Code[s].Op != bytecode.OpNewRegExp || int(fn.Code[s].A) >= len(fn.Constants) {
		return ""
	}
	lit := fn.Constants[fn.Code[s].A]
	consts := regConsts(fn, rc)
	for q, w := range rc.Code {
		if q >= len(rc.Marks) || rc.Marks[q]&bytecode.RMarkLitArg == 0 || w.Op() != bytecode.RCallMW || q >= len(rc.Aux) || int(rc.Aux[q]) >= len(consts) {
			continue
		}
		// The call of the literal's site: its first argument the same
		// literal, and the call the stack form's next of it, a few
		// instructions on.
		k := consts[rc.Aux[q]]
		if c := int(rc.SrcPC[q]); k.Kind == lit.Kind && k.Str == lit.Str && k.Flags == lit.Flags && c >= s-8 && c <= s+8 {
			return "made by Go in the call it is the first argument of (RMarkLitArg)"
		}
	}
	return ""
}

// jitParityBigIntSole compares BigIntSole's answers -- the bindings that
// hold their values alone, the stack form's locals, the register form's
// registers, each numbered its own way -- by what the builds made of them:
// the source positions of the operators that give back the old value of
// the binding they store to, stack and reg, the instructions of sp and rp
// that do. It is an answer of the stack form's only where its build asks
// it: where an operator of its program met BigInts (ssa's bigRelease).
func jitParityBigIntSole(res *FeedbackParity, name string, fn *bytecode.Function, src []uint32, sp *ir.Program, sfb ssa.Feedback, rp *ir.Program, stack, reg []int, note func(map[string]int, string, string)) {
	bf, ok := sfb.(ssa.BigIntFeedback)
	if !ok {
		return
	}
	asked := false
	for pc := range sp.Code {
		if bf.BigInt(pc) {
			asked = true
			break
		}
	}
	if !asked {
		return
	}
	positions := func(sites []int, stackPC func(int) int) string {
		var pos []int32
		for _, site := range sites {
			if pc := stackPC(site); pc >= 0 {
				p, _ := fn.PosAt(uint32(pc))
				pos = append(pos, p)
			}
		}
		slices.Sort(pos)
		return fmt.Sprint(pos)
	}
	a := positions(stack, func(site int) int { return site })
	b := positions(reg, func(site int) int {
		if site < 0 || site >= len(rp.Origin) || int(rp.Origin[site]) >= len(src) {
			return -1
		}
		return int(src[rp.Origin[site]])
	})
	res.Answers++
	if a == b {
		res.Matched++
		return
	}
	note(res.Gaps, "BigIntSole: differs", fmt.Sprintf("%s: old values given back at positions: stack %s, register %s", name, a, b))
}

// jitParityFrequencyAlike reports whether a and b, two forms' call
// frequencies as text, decide alike: both known, and on the same side of
// the inliner's cut (ssa.MinInliningFrequency), what decides whether the
// call is inlined. Each form counts over runs of its own -- the stack
// form's tiers while its entry warms, the feedback dispatcher from its
// call sites' first caches on, past the runs native code made -- so that
// the two are seldom the same number, nor within a factor of another.
func jitParityFrequencyAlike(a, b string) bool {
	var x, y float64
	if _, err := fmt.Sscan(a, &x); err != nil {
		return false
	}
	if _, err := fmt.Sscan(b, &y); err != nil {
		return false
	}
	return x < ssa.MinInliningFrequency == (y < ssa.MinInliningFrequency)
}

// jitParityStackOp is the stack instruction at pc of fn's code.
func jitParityStackOp(fn *bytecode.Function, pc int) any {
	if pc < len(fn.Code) {
		return fn.Code[pc].Op
	}
	return "past the code"
}

// jitParityMethods are the methods of a PC by name, as the snapshot numbers
// them (jitSnapNames).
var jitParityMethods = func() map[string]jitSnapMethod {
	m := map[string]jitSnapMethod{}
	for i, name := range jitSnapNames {
		if jitSnapAtPC[i] {
			m[name] = jitSnapMethod(i)
		}
	}
	return m
}()

// jitParityAsked reports whether the build asks method m at an instruction
// of op (jitSnapAsks): the sparse methods, Generic, EntryGeneric and
// Resumed, it asks at every one.
func jitParityAsked(m jitSnapMethod, op ir.Op) bool {
	return jitSnapSparse&(1<<m) != 0 || int(op) < len(jitSnapAsks) && jitSnapAsks[op]&(1<<m) != 0
}

// jitParityCall calls method m with args and returns its results as text,
// "" where they are all zero, a property's or a global's key by its name in
// r; ok is false where it panicked.
func jitParityCall(r *Runtime, m reflect.Value, args []reflect.Value) (s string, ok bool) {
	if !m.IsValid() {
		return "", false
	}
	defer func() {
		if recover() != nil {
			s, ok = "", false
		}
	}()
	out := m.Call(args)
	if sites, ok := out[0].Interface().([]ssa.CallSite); ok && len(out) == 1 {
		// Native calls: what is called, and how, not the callee's frame,
		// which each form lays out its own way (ssa.CallSite's Window).
		out[0] = reflect.ValueOf(jitParitySites(sites))
	}
	if site, ok := out[0].Interface().(ssa.InlineSite); ok {
		// A callee to inline: which, at a call of what shape, not its
		// program, its feedback or its frame's layout, each form's own
		// (ssa.InlineSite's Window) -- nor whether its receiver is
		// coerced, which the register form says only of a callee that
		// reads it, nor the cells of its receiver and captured bindings.
		site.Program, site.Feedback, site.Params, site.ThisSlot, site.Window = nil, nil, 0, 0, false
		site.Coerce, site.ThisCell, site.Upvalues = false, 0, 0
		out[0] = reflect.ValueOf(site)
	}
	if last := out[len(out)-1]; last.Kind() == reflect.Bool && !last.Bool() && len(out) > 1 {
		// An answer with its ok false is none.
		return "", true
	}
	zero := true
	parts := make([]string, len(out))
	for i, v := range out {
		if !v.IsZero() {
			zero = false
		}
		parts[i] = jitParityText(r, v, 0)
	}
	if zero {
		return "", true
	}
	return strings.Join(parts, " "), true
}

// jitParitySites are sites, the native calls a call makes, with the fields
// of the callee's frame cleared -- its parameters, locals, operand slots,
// receiver's slot, captured bindings' cells, register window, where its
// function object and its call's arguments go, its tail calls' frame,
// whether its start makes its locals -- each form's own, and a receiver's
// coercion only where the callee reads it (the builder clears it
// otherwise).
func jitParitySites(sites []ssa.CallSite) []ssa.CallSite {
	out := make([]ssa.CallSite, len(sites))
	for i, s := range sites {
		s.Coerce = s.Coerce && (s.ThisSlot >= 0 || s.Window != 0)
		s.Params, s.LocalCount, s.MaxStack, s.ThisSlot, s.StartOnly, s.Upvalues, s.Window = 0, 0, 0, 0, false, 0, 0
		s.Function, s.FrameArgs, s.Tail, s.TailFrame = false, false, false, false
		out[i] = s
	}
	return out
}

// jitParityText is v as text that two runtimes' answers share where they
// mean the same: an address is only whether it is one, a pointer's target
// is shown, to a depth, and a key, an atom of r's, is its name.
func jitParityText(r *Runtime, v reflect.Value, depth int) string {
	switch v.Kind() {
	case reflect.Uintptr, reflect.UnsafePointer:
		if v.IsZero() {
			return "0"
		}
		return "@"
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "nil"
		}
		if depth > 2 {
			return "&"
		}
		return "&" + jitParityText(r, v.Elem(), depth+1)
	case reflect.Struct:
		var b strings.Builder
		b.WriteString("{")
		t := v.Type()
		for i := range v.NumField() {
			if v.Field(i).IsZero() {
				continue
			}
			if f := v.Field(i); t.Field(i).Name == "Key" && f.Kind() == reflect.Uint32 {
				fmt.Fprintf(&b, "Key:%q ", r.AtomName(Atom(f.Uint())))
				continue
			}
			fmt.Fprintf(&b, "%s:%s ", t.Field(i).Name, jitParityText(r, v.Field(i), depth+1))
		}
		b.WriteString("}")
		return b.String()
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return "[]"
		}
		n := v.Len()
		var b strings.Builder
		fmt.Fprintf(&b, "[%d:", n)
		for i := range min(n, 8) {
			b.WriteString(jitParityText(r, v.Index(i), depth+1))
			b.WriteString(" ")
		}
		b.WriteString("]")
		return b.String()
	case reflect.Map, reflect.Func, reflect.Chan:
		if v.IsNil() {
			return "nil"
		}
		return "#"
	}
	return fmt.Sprint(v.Interface())
}

// jitParityDecisions builds both forms' programs with their feedback and
// notes where what the builds decided differs; and compares BigIntSole's
// answers by what the builds made of them, src being the stack pc of each
// register instruction of fn's register code.
func jitParityDecisions(res *FeedbackParity, name string, fn *bytecode.Function, src []uint32, sp *ir.Program, sfb ssa.Feedback, rp *ir.Program, rfb ssa.Feedback, note func(map[string]int, string, string)) {
	type built struct {
		refused                   bool
		why                       string
		inlined, hosts, unentered int
		frameOps, frameLeft       bool
		// releasedOld are the program's instructions whose BigInt operator
		// gives back the old value of the binding it stores to, which holds
		// its values alone (ssa.ReleaseOld, of BigIntSole).
		releasedOld []int
	}
	build := func(p *ir.Program, fb ssa.Feedback) (b built) {
		defer func() {
			if x := recover(); x != nil {
				b.refused, b.why = true, fmt.Sprint("panic: ", x)
			}
		}()
		w := jit.GetWorkspace()
		defer jit.PutWorkspace(w)
		f, err := ssa.BuildIn(&w.SSA, p, fb)
		if err != nil {
			return built{refused: true, why: err.Error()}
		}
		b = built{inlined: len(f.Inlined), hosts: len(f.HostsLeft), unentered: len(f.Unentered)}
		for _, blk := range f.Blocks {
			for _, v := range blk.Values {
				if v.Op != ssa.OpCall || len(v.Calls()) == 0 {
					continue
				}
				if v.Calls()[0].Release&ssa.ReleaseOld != 0 && v.State != nil {
					b.releasedOld = append(b.releasedOld, v.State.Site)
				}
				switch op := v.Calls()[0].Go; op {
				case ssa.GoHost, ssa.GoArgument, ssa.GoCells, ssa.GoClosure, ssa.GoClose:
					b.frameOps = true
					if !jitRegFrameOps[op] {
						b.frameLeft = true
					}
				}
			}
		}
		return b
	}
	s, r := build(sp, sfb), build(rp, rfb)
	if r.frameOps {
		res.FrameOps++
		if r.frameLeft {
			res.FrameOpsLeft++
		}
	}
	pair := func(what string, a, b int) {
		if a != b {
			note(res.Decisions, what, fmt.Sprintf("%s: stack %d, register %d", name, a, b))
		}
	}
	switch {
	case s.refused != r.refused:
		note(res.Decisions, "refused", fmt.Sprintf("%s: stack %v, register %v (%s%s)", name, s.refused, r.refused, s.why, r.why))
		return
	case s.refused:
		return
	}
	jitParityBigIntSole(res, name, fn, src, sp, sfb, rp, s.releasedOld, r.releasedOld, note)
	pair("inlined", s.inlined, r.inlined)
	pair("host operations left to Go", s.hosts, r.hosts)
	pair("loops' entries dropped", s.unentered, r.unentered)
}

// JITRefusals is why the JIT would not compile fn, a function of a program
// compiled with the register form, from each form's code, as far as its
// code alone says, before any of it runs: from its register code -- what
// jitRegTakes refuses, LowerReg's refusal (LowerRegHot's for a function of
// host operations alone, which the register tier compiles once a call it
// makes is native, as the stack form does), and the SSA build's with no
// feedback -- and from its stack code (LowerSSA, LowerSSAHot, the build),
// each "" where it is taken; a function too large for the JIT's default
// budget is neither's (jitCompiles). The refusals that what runs teaches --
// a deoptimization limit, the cache's room -- are not these. TestJITRefusals counts
// them over the corpora, the list of what the register form refuses that
// the stack form takes only ever shrinking.
func JITRefusals(fn *bytecode.Function) (reg, stack string) {
	build := func(p *ir.Program, err error, hot func() (*ir.Program, error)) string {
		if err != nil && jitcompile.HostOnly(err) {
			p, err = hot()
		}
		if err == nil {
			err = p.Validate()
		}
		if err == nil {
			_, err = ssa.Build(p)
		}
		if err != nil {
			var rf *jitcompile.Refusal
			if errors.As(err, &rf) {
				return rf.Reason
			}
			return err.Error()
		}
		return ""
	}
	if !jitCompiles(fn, jitCacheBytes) {
		// Too large for the default budget, whose compiles both forms
		// make none of (jitCompiles); not built here.
		const big = "too large for the JIT's memory"
		return big, big
	}
	stack = func() string {
		p, err := jitcompile.LowerSSA(fn)
		return build(p, err, func() (*ir.Program, error) { return jitcompile.LowerSSAHot(fn) })
	}()
	rc := fn.RegCode()
	switch {
	case rc == nil:
		return "no register code", stack
	case !rc.Wide:
		return "register code of the stack form's layout", stack
	}
	switch fn.Kind {
	case bytecode.KindNormal, bytecode.KindMethod, bytecode.KindArrow, bytecode.KindGetter, bytecode.KindSetter,
		bytecode.KindConstructor, bytecode.KindDerivedConstructor:
	default:
		return "function kind " + fmt.Sprint(fn.Kind), stack
	}
	p, err := jitcompile.LowerReg(rc, fn)
	return build(p, err, func() (*ir.Program, error) { return jitcompile.LowerRegHot(rc, fn) }), stack
}
