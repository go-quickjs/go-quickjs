package compile

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// denseLiveRegs is liveRegs as it was, a dense row of every register at
// every instruction and passes over all of them until none changes: the
// answer the shared sets must give.
func denseLiveRegs(p *ir.Program, rc *bytecode.RegCode) (live []uint64, words int) {
	n := p.Regs
	words = (n + 63) / 64
	live = make([]uint64, len(p.Code)*words)
	at := func(i int) []uint64 { return live[i*words : (i+1)*words] }
	set := func(s []uint64, r int) {
		if r >= 0 && r < n {
			s[r/64] |= 1 << (r % 64)
		}
	}
	kill := func(s []uint64, r int) {
		if r >= 0 && r < n {
			s[r/64] &^= 1 << (r % 64)
		}
	}
	always := make([]uint64, words)
	for r, c := range p.Captured {
		if c {
			set(always, r)
		}
	}
	in := make([]uint64, words)
	for changed := true; changed; {
		changed = false
		for i := len(p.Code) - 1; i >= 0; i-- {
			if p.Maps[i].Depth < 0 {
				continue
			}
			ins := &p.Code[i]
			clear(in)
			succ := func(j int) {
				if j >= 0 && j < len(p.Code) {
					for k, x := range at(j) {
						in[k] |= x
					}
				}
			}
			switch ins.Op {
			case ir.Jump:
				succ(ins.Target)
			case ir.Return:
			case ir.Branch, ir.IterStep:
				succ(i + 1)
				if ins.Op == ir.IterStep && !ins.When {
					// The step's register, written on the way in alone.
					if a, _ := ins.Writes(); a < 0 || a >= len(p.Captured) || !p.Captured[a] {
						kill(in, a)
					}
				}
				if ins.Op == ir.Branch || !ins.When {
					succ(ins.Target)
				}
			default:
				succ(i + 1)
				if ins.Op == ir.Host {
					if w := rc.Code[p.Origin[i]]; regGoBranches(w) {
						succ(p.At(p.Origin[i] + 2))
					}
				}
			}
			if ins.Op == ir.Host {
				reads, writes, all := RegAccess(rc, int(p.Origin[i]))
				for _, r := range writes {
					kill(in, r)
				}
				if w := rc.Code[p.Origin[i]]; w.Op() == bytecode.RCallW || w.Op() == bytecode.RCallWT || w.Op() == bytecode.RNewW {
					kill(in, int(w.A())+1)
				}
				if all {
					for k := range in {
						in[k] = ^uint64(0)
					}
				}
				for _, r := range reads {
					set(in, r)
				}
			} else {
				a, b := ins.Writes()
				if ins.Op == ir.IterStep && !ins.When {
					a = -1
				}
				kill(in, a)
				kill(in, b)
				for _, r := range irReads(ins) {
					set(in, r)
				}
			}
			if p.HandlerAt != nil && p.HandlerAt[i] != 0 {
				succ(int(p.Handlers[p.HandlerAt[i]-1].PC))
			}
			for k, x := range always {
				in[k] |= x
			}
			if n%64 != 0 {
				in[words-1] &= 1<<(n%64) - 1
			}
			if s := at(i); !slices.Equal(s, in) {
				copy(s, in)
				changed = true
			}
		}
	}
	for i := 1; i < len(p.Code); i++ {
		if p.Origin[i] == p.Origin[i-1] {
			copy(at(i), at(i-1))
		}
	}
	return live, words
}

// liveRegsMatch checks that p's liveness is what the dense dataflow finds.
func liveRegsMatch(t *testing.T, fn *bytecode.Function, p *ir.Program) bool {
	t.Helper()
	rows, words := denseLiveRegs(p, fn.RegCode())
	for pc := range p.Code {
		var want []int
		for i := range p.Regs {
			if rows[pc*words+i/64]&(1<<(i%64)) != 0 {
				want = append(want, i)
			}
		}
		if got := slices.Collect(p.Live.Slots(pc)); !slices.Equal(got, want) {
			t.Errorf("%s: pc %d: live %v, want %v", fn.Name, pc, got, want)
			return false
		}
	}
	return true
}

// boolsMatch checks that markBools marks of p's instructions what walking
// back from each operand did.
func boolsMatch(t *testing.T, fn *bytecode.Function, p *ir.Program) bool {
	t.Helper()
	a, b := *p, *p
	a.Code, b.Code = slices.Clone(p.Code), slices.Clone(p.Code)
	for i := range a.Code {
		a.Code[i].Bools, b.Code[i].Bools = 0, 0
	}
	markBools(&a)
	walkMarkBools(&b)
	for i := range a.Code {
		if a.Code[i].Bools != b.Code[i].Bools {
			t.Errorf("%s: pc %d: bools %d, want %d", fn.Name, i, a.Code[i].Bools, b.Code[i].Bools)
			return false
		}
	}
	return true
}

// The register lowering's liveness, found block by block over shared sets,
// is the dense dataflow's at every instruction of every function of the
// sources here, and of the files under QUICKJS_JIT_LIVE_CORPUS's
// directories: loops, branches, handlers, closures' captures, Go's own
// branches, and the lowerings of a register instruction in several. And
// markBools, which goes forward over each block, marks what walking back
// from each operand did, in both lowerings.
func TestLiveRegsMatchesDense(t *testing.T) {
	compiler.SetRegisterForm(true)
	defer compiler.SetRegisterForm(bytecode.RegDefault())
	sources := []string{
		`function f(n){let a=n*2;let s=0;for(let i=0;i<n;i++){s=(s+i)|0;if(i===7)s=(s+a)|0}let b=s+1;return b}`,
		`function g(o, k){var r=[]; for (var x in o) { try { r.push(o[x] + k) } catch (e) { r.push(e) } finally { k++ } } return r}`,
		`function h(a){let t=0; for (const v of a) { t += v; if (t > 100) break } let c = () => t + a.length; return c() + [t, a, 1, 2].length}`,
		`function w(s){switch(s){case 1: return 'a'; case 2: s += 3; case 3: return s * 2; default: while(s>0) s--; } return s ?? 0}`,
		`function q(x, y){ var z = x && y || (x ? y : -y); do { z = z * 2 } while (z < 1000 && x); return {z, x, y: y?.p} }`,
	}
	check := func(text, source string) {
		prog, err := parser.Parse(text, parser.Options{})
		if err != nil {
			t.Logf("%s: %v", source, err)
			return
		}
		top, err := compiler.Compile(prog, compiler.Options{Source: source, Text: text})
		if err != nil {
			t.Logf("%s: %v", source, err)
			return
		}
		var walk func(fn *bytecode.Function)
		walk = func(fn *bytecode.Function) {
			if fn.RegCode() != nil {
				for _, l := range []func(*bytecode.RegCode, *bytecode.Function) (*ir.Program, error){LowerReg, LowerRegHot, LowerRegInline} {
					if p, err := l(fn.RegCode(), fn); err == nil && p.Live != nil {
						if !liveRegsMatch(t, fn, p) || !boolsMatch(t, fn, p) {
							return
						}
					}
				}
			}
			for _, l := range []func(*bytecode.Function) (*ir.Program, error){LowerSSA, LowerSSAHot, LowerSSAInline} {
				if p, err := l(fn); err == nil && !boolsMatch(t, fn, p) {
					return
				}
			}
			for _, c := range fn.Constants {
				if c.Kind == bytecode.ConstFunction && c.Fn != nil {
					walk(c.Fn)
				}
			}
		}
		walk(top)
	}
	for i, src := range sources {
		check(src, "source"+string(rune('0'+i)))
	}
	for _, dir := range filepath.SplitList(os.Getenv("QUICKJS_JIT_LIVE_CORPUS")) {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			check(string(b), p)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// walkMarkBools is markBools as it was, walking back from each operand: it sets the Bools of p's instructions (ir.Instruction's): the
// operands a comparison or a Not wrote in the same block, found by walking
// back over instructions that write slots alone, and through copies.
func walkMarkBools(p *ir.Program) {
	starts := make([]bool, len(p.Code)+1)
	starts[0] = true
	for _, e := range p.Entries {
		if e >= 0 && e < len(starts) {
			starts[e] = true
		}
	}
	for _, h := range p.Handlers {
		if int(h.PC) < len(starts) {
			starts[h.PC] = true
		}
	}
	for pc, in := range p.Code {
		switch in.Op {
		case ir.Jump, ir.Branch:
			if in.Target >= 0 && in.Target < len(starts) {
				starts[in.Target] = true
			}
			starts[pc+1] = true
		case ir.Return:
			starts[pc+1] = true
		}
	}
	boolean := func(in *ir.Instruction) bool {
		return in.Op == ir.Binary && in.Operator >= ir.Lt && in.Operator <= ir.Ne || in.Op == ir.Unary && in.Operator == ir.Not
	}
	// written reports whether slot s, read at pc, was written last, in its
	// block, by a comparison or a Not, or a copy of what one wrote, with
	// only instructions between that write slots alone: Binary, Unary and
	// the copies.
	written := func(pc, s int) bool {
		for q := pc - 1; q >= 0 && !starts[q+1] && s >= 0; q-- {
			in := &p.Code[q]
			switch in.Op {
			case ir.Nop:
			case ir.Copy:
				if in.Dest == s {
					s = in.Left.Slot
				}
			case ir.CopyPair, ir.StoreLoad:
				switch s {
				case in.Dest:
					s = in.Left.Slot
				case in.Extra:
					s = in.Right.Slot
					if in.Op == ir.StoreLoad && s == in.Dest {
						// Read after Dest is written: set_local_get's.
						s = in.Left.Slot
					}
				}
			case ir.Binary, ir.Unary:
				if in.Dest == s {
					return boolean(in)
				}
			default:
				return false
			}
		}
		return false
	}
	for pc := range p.Code {
		in := &p.Code[pc]
		switch {
		case in.Op == ir.Binary && in.Operator != ir.Eq && in.Operator != ir.Ne:
			if in.Left.Slot >= 0 && written(pc, in.Left.Slot) {
				in.Bools |= 1
			}
			if in.Right.Slot >= 0 && written(pc, in.Right.Slot) {
				in.Bools |= 2
			}
		case in.Op == ir.Unary && in.Operator != ir.Not:
			if in.Left.Slot >= 0 && written(pc, in.Left.Slot) {
				in.Bools |= 1
			}
		}
	}
}

// A construction's receiver register, R[A+1] of new_w, is the
// construction's to write, as a plain call's is: no register the code
// reads, it is live nowhere -- not at the function's entry, nor round the
// loop that constructs, where a phi of the last turn's object with the
// slot the entry loads had kept escape analysis from leaving the object
// unmade.
func TestLiveRegsConstructionReceiver(t *testing.T) {
	compiler.SetRegisterForm(true)
	defer compiler.SetRegisterForm(bytecode.RegDefault())
	text := `function V(x) { this.x = x }
	function f(n) { let s = 0; for (let i = 0; i < n; i++) { const v = new V(i); s += v.x } return s }`
	prog, err := parser.Parse(text, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	top, err := compiler.Compile(prog, compiler.Options{Source: "construct.js", Text: text})
	if err != nil {
		t.Fatal(err)
	}
	var fn *bytecode.Function
	for _, c := range top.Constants {
		if c.Kind == bytecode.ConstFunction && c.Fn != nil && c.Fn.Name == "f" {
			fn = c.Fn
		}
	}
	if fn == nil || fn.RegCode() == nil {
		t.Fatal("f has no register code")
	}
	rc := fn.RegCode()
	recv := -1
	for _, w := range rc.Code {
		if w.Op() == bytecode.RNewW {
			recv = int(w.A()) + 1
		}
	}
	if recv < 0 {
		t.Fatal("f constructs nothing")
	}
	p, err := LowerReg(rc, fn)
	if err != nil {
		t.Fatal(err)
	}
	for pc := range p.Code {
		if slices.Contains(slices.Collect(p.Live.Slots(pc)), recv) {
			t.Errorf("pc %d: the construction's receiver register %d is live", pc, recv)
		}
	}
	if !liveRegsMatch(t, fn, p) {
		t.Error("liveness is not the dense dataflow's")
	}
}
