package quickjs_test

import "testing"

// An arrow whose body reads new.target only through a direct eval, itself
// or in an arrow of its own, still captures its maker's: the evaluated
// code may name it however it is written (the VM's TestArrowNewTarget).
func TestArrowNewTargetEval(t *testing.T) {
	checkEval(t, `function F() { this.e = () => eval('new.target'); this.n = () => () => eval('new.target') }
		var f = new F(); [f.e() === F, f.n()() === F].join()`, "true,true")
}
