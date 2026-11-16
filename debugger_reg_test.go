package quickjs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/compiler/regcomp"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// debugRegSource has a debugger stop in a function's own bindings, a
// closure's, a loop's per-turn let, a catch clause's and a for-of head's,
// with a breakpoint on a line of its own.
const debugRegSource = `function f(a) {
  let b = a + 1;
  const g = () => a + b;
  for (let i = 0; i < 2; i++) {
    let c = i * 10;
    debugger;
  }
  try { throw 7 } catch (e) { debugger; }
  for (const v of [3]) {
    b += v;
  }
  return g() + ":" + b;
}
f(1)`

// TestDebuggerRegisterCode runs the debugger over register code, the
// register form and tier on, and over the stack form, and wants the same
// from both: where each pause is, by a debugger statement, a breakpoint
// and steps; the bindings each pause sees; and what the code returns after
// the debugger has set a binding and evaluated an assignment in a frame.
// Register code is what runs: no function is refused.
func TestDebuggerRegisterCode(t *testing.T) {
	run := func(reg bool) string {
		compiler.SetRegisterForm(reg)
		vm.SetRegTier(reg)
		defer func() {
			env := bytecode.RegDefault()
			compiler.SetRegisterForm(env)
			vm.SetRegTier(env)
		}()
		var refused []string
		regcomp.OnRefuse = func(fn *bytecode.Function, why string) {
			refused = append(refused, fn.Name+": "+why)
		}
		defer func() { regcomp.OnRefuse = nil }()
		before := vm.RegsBuilt()
		r, h := newDebugged(t)
		r.rt.SetBreakpoint("reg.js", 10, 0)
		var seen []string
		steps := 0
		h.then = func(p *vm.DebugPause) vm.StepAction {
			f := p.Frames[0]
			loc := f.Location()
			seen = append(seen, fmt.Sprintf("%s %d:%d this=%s local[%s]", reasonName(p.Reason), loc.Line, loc.Column,
				h.show(f.This()), h.scope(f.Scopes()[0])))
			switch len(seen) {
			case 1:
				if err := f.SetVariable("b", vm.Int(40)); err != nil {
					t.Error(err)
				}
				if v, err := f.Evaluate("a = 2, c + a"); err != nil || h.show(v) != "2" {
					t.Errorf("evaluate: %s, %v", h.show(v), err)
				}
			}
			if p.Reason == vm.PauseDebuggerStatement && steps < 3 {
				// Steps over the next places from the second stop on.
				if len(seen) > 1 {
					steps++
					return vm.StepOver
				}
			}
			if p.Reason == vm.PauseStep && steps < 3 {
				steps++
				return vm.StepOver
			}
			return vm.Continue
		}
		v, err := r.EvalFile("reg.js", debugRegSource)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, "result "+v.String())
		if reg {
			if built := vm.RegsBuilt() - before; built == 0 {
				t.Errorf("no register code ran")
			}
			for _, r := range refused {
				t.Errorf("refused: %s", r)
			}
		}
		return strings.Join(seen, "\n")
	}
	stack, reg := run(false), run(true)
	if reg != stack {
		t.Errorf("register code:\n%s\nstack form:\n%s", reg, stack)
	}
	for _, want := range []string{"result 45:43", "c=10", "e=7", "v=3"} {
		if !strings.Contains(stack, want) {
			t.Errorf("stack form has no %q:\n%s", want, stack)
		}
	}
}
