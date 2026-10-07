package quickjs

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// recorder is a debugger that writes down what it is told and answers each
// pause with what then says.
type recorder struct {
	t        *testing.T
	r        *Runtime
	scripts  []string
	resolved []string
	pauses   []string
	then     func(p *vm.DebugPause) vm.StepAction
}

func (h *recorder) ScriptParsed(s *vm.DebugScript) {
	h.scripts = append(h.scripts, fmt.Sprintf("%d %q", s.ID, s.Name))
}

func (h *recorder) BreakpointResolved(id int, at vm.DebugLocation) {
	h.resolved = append(h.resolved, fmt.Sprintf("%d %s:%d:%d", id, at.Script.Name, at.Line, at.Column))
}

func (h *recorder) Paused(p *vm.DebugPause) vm.StepAction {
	top := p.Frames[0]
	loc := top.Location()
	h.pauses = append(h.pauses, fmt.Sprintf("%s %s:%d:%d", reasonName(p.Reason), top.FunctionName(), loc.Line, loc.Column))
	if h.then != nil {
		return h.then(p)
	}
	return vm.Continue
}

func reasonName(r vm.PauseReason) string {
	return [...]string{"breakpoint", "step", "debugger", "exception", "requested"}[r]
}

// show writes a value as the public API would.
func (h *recorder) show(v vm.Value) string { return Value{v: v, rt: h.r.rt}.String() }

// scope writes a scope's bindings as name=value, in order.
func (h *recorder) scope(s vm.DebugScope) string {
	var out []string
	for _, b := range s.Bindings {
		v := h.show(b.Value)
		if strings.HasPrefix(v, "function ") {
			v = "<fn>"
		}
		if b.Uninitialized {
			v = "<uninitialized>"
		}
		out = append(out, b.Name+"="+v)
	}
	return strings.Join(out, " ")
}

func newDebugged(t *testing.T) (*Runtime, *recorder) {
	t.Helper()
	r := New(WithDebugger())
	t.Cleanup(func() { r.Close() })
	h := &recorder{t: t, r: r}
	r.rt.SetDebugHandler(h)
	return r, h
}

const debugSource = `function add(a, b) {
  const sum = a + b;
  return sum;
}
let total = 0;
function run(n) {
  for (let i = 0; i < n; i++) {
    total = add(total, i);
  }
  return total;
}
run(3);
`

// TestDebuggerBreakpoint pins a breakpoint set by a script's name before
// the script loads: it is resolved as it loads, stops the code at every pass
// of its statement, and the pause sees the frame's bindings, the closure's
// and the script's, and evaluates code in the frame that reads and changes
// them.
func TestDebuggerBreakpoint(t *testing.T) {
	r, h := newDebugged(t)
	id, at := r.rt.SetBreakpoint("calc.js", 2, 0)
	if len(at) != 0 {
		t.Fatalf("resolved before the script loaded: %v", at)
	}
	var seen []string
	h.then = func(p *vm.DebugPause) vm.StepAction {
		f := p.Frames[0]
		scopes := f.Scopes()
		v, err := f.Evaluate("a * 10 + b")
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, fmt.Sprintf("bp=%v local[%s] eval=%s", p.Breakpoints, h.scope(scopes[0]), h.show(v)))
		if len(seen) == 3 {
			// Changing a binding from the debugger changes the code's.
			if _, err := f.Evaluate("b = 100"); err != nil {
				t.Fatal(err)
			}
		}
		return vm.Continue
	}
	v, err := r.EvalFile("calc.js", debugSource)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.resolved, "|"); got != fmt.Sprintf("%d calc.js:2:3", id) {
		t.Errorf("resolved: %s", got)
	}
	want := []string{
		fmt.Sprintf("bp=[%d] local[sum=<uninitialized> b=0 a=0] eval=0", id),
		fmt.Sprintf("bp=[%d] local[sum=<uninitialized> b=1 a=0] eval=1", id),
		fmt.Sprintf("bp=[%d] local[sum=<uninitialized> b=2 a=1] eval=12", id),
	}
	if got := strings.Join(seen, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("pauses:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if v.String() != "101" {
		t.Errorf("result %s, want 101: the debugger's b = 100 lost", v)
	}
}

// TestDebuggerStepping pins step over, into and out from a breakpoint in a
// loop's body: into goes into the call, over stays in the function, and out
// returns to the caller, whose next place to stop is the loop's update, then
// its test, then its body again.
func TestDebuggerStepping(t *testing.T) {
	r, h := newDebugged(t)
	r.rt.SetBreakpoint("calc.js", 8, 0)
	steps := []vm.StepAction{vm.StepInto, vm.StepOver, vm.StepOut, vm.StepOver, vm.StepOver}
	h.then = func(p *vm.DebugPause) vm.StepAction {
		if len(steps) == 0 {
			r.rt.RemoveBreakpoint(1)
			return vm.Continue
		}
		s := steps[0]
		steps = steps[1:]
		return s
	}
	if _, err := r.EvalFile("calc.js", debugSource); err != nil {
		t.Fatal(err)
	}
	want := "breakpoint run:8:5|step add:2:3|step add:3:3|step run:7:26|step run:7:19|step run:8:5"
	if got := strings.Join(h.pauses, "|"); got != want {
		t.Errorf("pauses %s\nwant   %s", got, want)
	}
}

// TestDebuggerLoopHeads pins that a for-of and a for-in head are places to
// stop each time round, where the loop takes its next value, as a for loop's
// test is: stepping over a body's last statement stops at the head before
// the body again, and at the head before the loop ends.
func TestDebuggerLoopHeads(t *testing.T) {
	r, h := newDebugged(t)
	r.rt.SetBreakpoint("heads.js", 3, 0)
	h.then = func(p *vm.DebugPause) vm.StepAction {
		if len(h.pauses) > 12 {
			return vm.Continue
		}
		return vm.StepOver
	}
	src := `const xs = [];
for (const x of [1, 2]) {
  xs.push(x);
}
for (const k in {a: 1}) {
  xs.push(k);
}
xs.join();
`
	if _, err := r.EvalFile("heads.js", src); err != nil {
		t.Fatal(err)
	}
	want := "breakpoint :3:3|step :2:6|step :3:3|step :2:6|step :5:1|step :5:6|step :6:3|step :5:6|step :8:1"
	if got := strings.Join(h.pauses, "|"); got != want {
		t.Errorf("pauses %s\nwant   %s", got, want)
	}
}

// TestDebuggerStatement pins that a debugger statement stops the code only
// with a debugger attached, and only in a runtime made for one; a runtime
// made without one compiles no OpDebugStmt at all.
func TestDebuggerStatement(t *testing.T) {
	const src = "let x = 1;\ndebugger;\nx + 1"
	r, h := newDebugged(t)
	if v, err := r.EvalFile("d.js", src); err != nil || v.String() != "2" {
		t.Fatalf("%v %v", v, err)
	}
	if got := strings.Join(h.pauses, "|"); got != "debugger :2:1" {
		t.Errorf("pauses %q", got)
	}
	r.rt.SetDebugHandler(nil)
	if _, err := r.EvalFile("d2.js", "debugger;"); err != nil {
		t.Fatal(err)
	}
	if len(h.pauses) != 1 {
		t.Errorf("stopped with no debugger attached: %v", h.pauses)
	}

	plain := New()
	defer plain.Close()
	fn, err := plain.compile(src, "d.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range fn.Code {
		if in.Op == bytecode.OpDebugStmt {
			t.Fatal("a runtime made without WithDebugger compiled an OpDebugStmt")
		}
	}
	if fn.Debug != nil {
		t.Error("a runtime made without WithDebugger recorded statements")
	}
}

// TestDebuggerExceptions pins pausing on exceptions: all of them, or only
// those no catch clause on the stack catches, once each however many frames
// they unwind.
func TestDebuggerExceptions(t *testing.T) {
	const src = `function thrower() { throw new Error("boom"); }
function caught() { try { thrower(); } catch (e) { return "caught"; } }
caught();
thrower();`
	for _, c := range []struct {
		mode vm.ExceptionPause
		want string
	}{
		{vm.PauseOnNoExceptions, ""},
		{vm.PauseOnAllExceptions, "exception thrower:1 caught=true|exception thrower:1 caught=false"},
		{vm.PauseOnUncaughtExceptions, "exception thrower:1 caught=false"},
	} {
		r, h := newDebugged(t)
		r.rt.SetPauseOnExceptions(c.mode)
		var got []string
		h.then = func(p *vm.DebugPause) vm.StepAction {
			got = append(got, fmt.Sprintf("%s %s:%d caught=%v", reasonName(p.Reason),
				p.Frames[0].FunctionName(), p.Frames[0].Location().Line, p.Caught))
			if !strings.Contains(h.show(p.Exception), "boom") {
				t.Errorf("exception %s", h.show(p.Exception))
			}
			return vm.Continue
		}
		if _, err := r.EvalFile("e.js", src); err == nil {
			t.Fatal("no uncaught error")
		}
		if s := strings.Join(got, "|"); s != c.want {
			t.Errorf("mode %d: %q, want %q", c.mode, s, c.want)
		}
	}
}

// TestDebuggerRequestPause pins a pause asked for from another goroutine
// while the code runs a loop: it stops within the interrupt check's
// interval, and code the debugger evaluates there ends the loop.
func TestDebuggerRequestPause(t *testing.T) {
	r, h := newDebugged(t)
	h.then = func(p *vm.DebugPause) vm.StepAction {
		if _, err := p.Frames[0].Evaluate("stop = true"); err != nil {
			t.Fatal(err)
		}
		return vm.Continue
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		r.rt.RequestPause()
	}()
	v, err := r.EvalFile("loop.js", "let stop = false, n = 0;\nwhile (!stop) {\n  n++;\n}\n'stopped'")
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "stopped" || len(h.pauses) != 1 || !strings.HasPrefix(h.pauses[0], "requested ") {
		t.Errorf("%v %v", v, h.pauses)
	}
}

// TestDebuggerScripts pins the scripts reported: a script, a Program run
// twice, each run compiled for the runtime, and the code of an eval, which
// has no name.
func TestDebuggerScripts(t *testing.T) {
	r, h := newDebugged(t)
	p, err := Compile("prog.js", "eval('1 + 1')")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.EvalFile("a.js", "1"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := r.RunProgram(p); err != nil {
			t.Fatal(err)
		}
	}
	want := `1 "a.js"|2 "prog.js"|3 ""|4 "prog.js"|5 ""`
	if got := strings.Join(h.scripts, "|"); got != want {
		t.Errorf("scripts %s\nwant    %s", got, want)
	}
	// The program a runtime made for a debugger compiles for itself leaves
	// the one any runtime may run as it was.
	plain := New()
	defer plain.Close()
	if v, err := plain.RunProgram(p); err != nil || v.String() != "2" {
		t.Errorf("%v %v", v, err)
	}
	if fn, _ := p.code(plain); fn.Debug != nil {
		t.Error("the shared program was compiled for a debugger")
	}
}

// TestDebuggerScopesAndFrames pins what a pause shows of the stack: each
// frame of compiled code with its function's name and where it is, its own
// bindings, the ones it shares with the function it is in, a with
// statement's object, the script's top-level let and const, and the global
// object; and code evaluated in a caller's frame sees the caller's bindings.
func TestDebuggerScopesAndFrames(t *testing.T) {
	r, h := newDebugged(t)
	const src = `let top = "T";
function outer(o) {
  const shared = o + 1;
  function inner(x) {
    with ({w: 7}) {
      debugger;
    }
    return x + shared;
  }
  return inner(2);
}
outer(10);
`
	var got []string
	h.then = func(p *vm.DebugPause) vm.StepAction {
		for _, f := range p.Frames {
			loc := f.Location()
			var kinds []string
			for _, s := range f.Scopes() {
				switch s.Kind {
				case vm.ScopeLocal:
					kinds = append(kinds, "local["+h.scope(s)+"]")
				case vm.ScopeClosure:
					kinds = append(kinds, "closure["+h.scope(s)+"]")
				case vm.ScopeWith:
					kinds = append(kinds, "with")
				case vm.ScopeScript:
					kinds = append(kinds, "script")
				case vm.ScopeGlobal:
					kinds = append(kinds, "global")
				}
			}
			got = append(got, fmt.Sprintf("%s %d:%d %s", f.FunctionName(), loc.Line, loc.Column, strings.Join(kinds, " ")))
		}
		in, err := p.Frames[0].Evaluate("w + x + shared + top")
		if err != nil {
			t.Fatal(err)
		}
		caller, err := p.Frames[1].Evaluate("typeof x + ' ' + o")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, "eval "+h.show(in)+" | "+h.show(caller))
		return vm.Continue
	}
	if _, err := r.EvalFile("s.js", src); err != nil {
		t.Fatal(err)
	}
	// top and outer are the script's, which its own scopes show.
	want := strings.Join([]string{
		"inner 6:7 local[x=2] with closure[inner=<fn> shared=11 o=10] script global",
		"outer 10:10 local[inner=<fn> shared=11 o=10] script global",
		" 12:1 local[] script global",
		"eval 20T | undefined 10",
	}, "\n")
	if s := strings.Join(got, "\n"); s != want {
		t.Errorf("got:\n%s\nwant:\n%s", s, want)
	}
}

// TestDebuggerEvaluate pins code evaluated in a pause: an exception it
// throws is returned rather than stopping the code, a breakpoint in a
// function it calls does not stop it, and a frame is of no use once the
// pause is over.
func TestDebuggerEvaluate(t *testing.T) {
	r, h := newDebugged(t)
	r.rt.SetPauseOnExceptions(vm.PauseOnAllExceptions)
	r.rt.SetBreakpoint("v.js", 1, 0)
	var kept *vm.DebugFrame
	var got []string
	h.then = func(p *vm.DebugPause) vm.StepAction {
		if p.Reason != vm.PauseDebuggerStatement {
			got = append(got, "stopped at "+reasonName(p.Reason))
			return vm.Continue
		}
		f := p.Frames[0]
		if _, err := f.Evaluate("null.x"); err == nil || !strings.Contains(err.Error(), "TypeError") {
			t.Errorf("throwing evaluation: %v", err)
		}
		v, err := f.Evaluate("hit()")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, "hit() = "+h.show(v))
		kept = f
		return vm.Continue
	}
	// The breakpoint is in hit(), on line 1, which the evaluation calls.
	if _, err := r.EvalFile("v.js", "function hit() { return 'h'; }\ndebugger;\n"); err != nil {
		t.Fatal(err)
	}
	if s := strings.Join(got, "|"); s != "hit() = h" {
		t.Errorf("%s", s)
	}
	if _, err := kept.Evaluate("1"); err == nil {
		t.Error("a frame evaluated code after its pause was over")
	}
}

// TestDebuggerModulesAndAsync pins a breakpoint in a module, set by its
// specifier, and one in an async function after an await, which the code
// reaches in a later job.
func TestDebuggerModulesAndAsync(t *testing.T) {
	r, h := newDebugged(t)
	r.rt.SetBreakpoint("m.mjs", 4, 0)
	var got []string
	h.then = func(p *vm.DebugPause) vm.StepAction {
		v, err := p.Frames[0].Evaluate("x")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s x=%s", h.pauses[len(h.pauses)-1], h.show(v)))
		return vm.Continue
	}
	const src = `async function f() {
  let x = 1;
  await null;
  x = x + 41;
  return x;
}
export const result = await f();
`
	if _, err := r.EvalModule("m.mjs", src); err != nil {
		t.Fatal(err)
	}
	if s := strings.Join(got, "|"); s != "breakpoint f:4:3 x=1" {
		t.Errorf("%s", s)
	}
}
