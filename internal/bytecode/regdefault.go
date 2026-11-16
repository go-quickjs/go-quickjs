package bytecode

import "os"

// RegDefault reports whether the register interpreter
// (docs/register-interpreter.md) is on in this process: the compiler gives
// each function its register form, and a runtime without the JIT runs it.
// It is on by default where the register code's handlers are native
// (regArch); QJS_REG in the environment says otherwise, 0 for off and
// anything else for on.
//
// QJS_REG=0 is temporary: it runs the stack interpreter and the tree tier,
// the tiers the register interpreter replaces, until they are removed.
func RegDefault() bool {
	switch os.Getenv("QJS_REG") {
	case "":
		return regArch
	case "0":
		return false
	}
	return true
}
