package quickjs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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

// debugRegWant is where the debugger pauses in debugRegSource, the bindings
// each pause sees, and the result, as the stack interpreter gave them
// before register code replaced it.
const debugRegWant = `debugger 6:5 this=undefined local[c=0 i=0 g=() => a + b b=2 a=1]
debugger 6:5 this=undefined local[c=10 i=1 g=() => a + b b=40 a=2]
step 4:26 this=undefined local[i=1 g=() => a + b b=40 a=2]
step 4:19 this=undefined local[i=2 g=() => a + b b=40 a=2]
step 8:3 this=undefined local[g=() => a + b b=40 a=2]
debugger 8:31 this=undefined local[e=7 g=() => a + b b=40 a=2]
breakpoint 10:5 this=undefined local[v=3 g=() => a + b b=40 a=2]
result 45:43`

// TestDebuggerRegisterCode runs the debugger over register code: where
// each pause is, by a debugger statement, a breakpoint and steps; the
// bindings each pause sees; and what the code returns after the debugger
// has set a binding and evaluated an assignment in a frame. No function
// is refused.
func TestDebuggerRegisterCode(t *testing.T) {
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
	if built := vm.RegsBuilt() - before; built == 0 {
		t.Errorf("no register code ran")
	}
	for _, r := range refused {
		t.Errorf("refused: %s", r)
	}
	if got := strings.Join(seen, "\n"); got != debugRegWant {
		t.Errorf("got:\n%s\nwant:\n%s", got, debugRegWant)
	}
}
