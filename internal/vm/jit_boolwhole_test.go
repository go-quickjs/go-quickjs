//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"
)

// regBoolWholeSrc's comparisons are values -- a compare-and-branch over
// load_false and load_true, which the lowering makes the comparison's
// Binary (compile's boolBranch) -- of operands the slot IR's evaluator
// does not compare, objects and strings, into a register that held
// another value before: the VM runs the compare-and-branch whole.
const regBoolWholeSrc = `
function objs() { var k = [], x = 0, r = []; for (var i = 0; i < 2; i++) k.push({}); x = 5; r.push(x, k[0] === k[1], k[0] !== k[1], k[0] === k[0]); return r.join() }
function strs(v, t) { var a = typeof v; var r = "" + (a === "number") + (typeof v === t) + (typeof v !== t) + (a == t); return r }
function lits() { var r = [], k = [], i; for (i = 0; i < 4; i++) { var x = /a(b)?/g; k.push(x); r.push(x.lastIndex, x.exec("xabab") + "", x.lastIndex) } r.push(k[0] === k[1], k[0].source); return r.join() }
`

// A comparison's value, lowered as its Binary with the loads after its
// compare-and-branch Nops, which the evaluator does not make and the VM
// runs whole (regRunner.Run): the VM goes on with the load the branch
// goes to, and its jump, to where the program does its own instruction's
// work, as the interpreter does. It had gone on at the Nops, past the
// loads, the register as it was before: lits()'s k[0] === k[1] answered
// 0, the lastIndex the register held, and tof's typeof x === t the
// typeof another comparison had left in it (TestRegLowerEvaluate's
// RegExpLiterals and Sites).
func TestJITRegBoolWholeRun(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rts := regNativeRuntimes(t, regBoolWholeSrc)
	r := rts[0]
	for _, c := range []struct {
		name string
		args []Value
		want string
	}{
		{"objs", nil, "5,false,true,true"},
		{"strs", []Value{Float(2), Str(NewString("string"))}, "truefalsetruefalse"},
		{"strs", []Value{Float(2), Str(NewString("number"))}, "truetruefalsetrue"},
		{"strs", []Value{Str(NewString("s")), Str(NewString("string"))}, "falsetruefalsetrue"},
		{"lits", nil, "0,ab,b,3,0,ab,b,3,0,ab,b,3,0,ab,b,3,false,a(b)?"},
	} {
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
		res := r.RegLowerCall(fn, c.args, 1<<20)
		if res.Skipped != "" {
			t.Fatalf("%s: not evaluated: %s", c.name, res.Skipped)
		}
		if got := regNativeShow(r, res.Value, res.Err); got != c.want {
			t.Errorf("%s: evaluated %s, want %s", c.name, got, c.want)
		}
		if res.Steps == 0 {
			t.Errorf("%s: the VM ran no instruction whole", c.name)
		}
		plain, _ := rts[1].GetProp(Obj(rts[1].Global()), rts[1].Intern(c.name))
		v, err := rts[1].Call(plain, Undefined, c.args)
		if got := regNativeShow(rts[1], v, err); got != c.want {
			t.Errorf("%s: called %s, want %s", c.name, got, c.want)
		}
	}
}
