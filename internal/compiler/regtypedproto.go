package compiler

import (
	"os"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/compiler/asmjs"
	"github.com/go-quickjs/go-quickjs/internal/compiler/regcomp"
	"slices"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// typedProto names the functions the typed call path's prototype gives a
// typed twin (bytecode.TypedTwin): QJS_TYPEDPROTO, a comma-separated
// list, read once, as QJS_NONATIVE is. The prototype's switch, which the
// asm.js link replaces.
var typedProto = strings.Split(os.Getenv("QJS_TYPEDPROTO"), ",")

// setTyped gives fn, the function lit compiled with resolution rec, whose
// register code is compiled when first asked for, its typed code: a
// function of the validated asm.js module c is, the code compiled from its
// types (regcomp.LaterTyped); or, where QJS_TYPEDPROTO names it, its typed
// twin, the call path's prototype. Code compiled for a debugger is never
// typed.
func (c *compiler) setTyped(lit *ast.FuncLit, fn *bytecode.Function, rec *regcomp.Record) {
	if c.opts.Debug || fn.Reg == nil {
		return
	}
	if m := c.asm; m != nil {
		for i, f := range m.Funcs {
			if f.Lit == lit || f.Lit.Start == lit.Start && f.Lit.End == lit.End {
				fn.Reg.SetTyped(regcomp.LaterTyped(lit, fn, rec, m, i))
				return
			}
		}
	}
	if fn.Name == "" || !slices.Contains(typedProto, fn.Name) {
		return
	}
	fn.Reg.SetTyped(bytecode.NewRegLater(func() *bytecode.RegCode { return bytecode.TypedTwin(fn.RegCode()) }))
}

// asmTrace prints why a "use asm" function is not a module the typed
// code is compiled for (QJS_ASMTRACE), read once.
var asmTrace = os.Getenv("QJS_ASMTRACE") != ""

func init() { regcomp.SetAsmTrace(asmTrace) }

// validateAsm validates lit, the function c compiles, as an asm.js module
// where it says "use asm" and its register code is compiled: a module that
// passes has its functions given typed code; any other runs as the
// ordinary code it also is.
func (c *compiler) validateAsm(lit *ast.FuncLit) {
	if c.reg == nil || c.opts.Debug || !asmjs.IsModule(lit, c.opts.Text) {
		return
	}
	m, err := asmjs.Validate(lit, c.opts.Text)
	if err != nil {
		if asmTrace {
			println("asm.js:", err.Error())
		}
		return
	}
	c.asm = m
}

// SetTypedProto names the functions given typed twins from now on, as
// QJS_TYPEDPROTO does, and returns the names it named: a test's switch.
func SetTypedProto(names ...string) []string {
	was := typedProto
	typedProto = names
	return was
}
