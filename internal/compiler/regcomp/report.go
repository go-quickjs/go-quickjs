package regcomp

import (
	"os"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The backend's refusals, kept where QJS_REGREPORT asks for the register
// tier's report (the VM's regReport): what each function the backend left
// to the stack form has that it does not compile.

var reportOn = os.Getenv("QJS_REGREPORT") != ""

var refused sync.Map // *bytecode.Function -> string

// Refuse records why fn has no register code, for the report.
func Refuse(fn *bytecode.Function, why string) {
	if trace {
		println("regcomp: not", fn.Name, why)
	}
	if reportOn {
		refused.Store(fn, why)
	}
}

// Refusal is why fn has no register code, as Refuse recorded it, or "".
func Refusal(fn *bytecode.Function) string {
	if v, ok := refused.Load(fn); ok {
		return v.(string)
	}
	return ""
}

// why is what of a function's kind and flags the backend does not compile
// yet, or "".
func why(fn *bytecode.Function) string {
	switch {
	case fn.Generator || fn.Async:
		return "generator or async"
	case fn.HasDirectEval:
		return "direct eval"
	case fn.Debug != nil:
		return "compiled for a debugger"
	case fn.TopLevel:
		return "top-level code"
	case fn.UsesArguments || fn.MakesArguments:
		return "arguments"
	case fn.HasRest || !fn.HasSimpleParams || fn.ParamsAreLexical:
		return "parameter patterns, defaults or rest"
	case fn.Kind == bytecode.KindConstructor || fn.Kind == bytecode.KindDerivedConstructor:
		return "class constructor"
	case fn.Kind != bytecode.KindNormal && fn.Kind != bytecode.KindArrow && fn.Kind != bytecode.KindMethod:
		return "getter, setter, field initializer or static block"
	case fn.PrivateSites != 0:
		return "private members"
	case fn.HasTailCall:
		return "tail call"
	}
	return ""
}
