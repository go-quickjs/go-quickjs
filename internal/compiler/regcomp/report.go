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

// OnRefuse, where set, is told each refusal as it is made: the refusal
// inventory's (internal/cmd/regrefusals). Set it before compiling.
var OnRefuse func(fn *bytecode.Function, why string)

// Refuse records why fn has no register code, for the report.
func Refuse(fn *bytecode.Function, why string) {
	if trace {
		println("regcomp: not", fn.Name, why)
	}
	if OnRefuse != nil {
		OnRefuse(fn, why)
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
	case fn.TopLevel:
		return "top-level code"
	case fn.UsesArguments || fn.MakesArguments:
		return "arguments"
	case fn.Kind == bytecode.KindClassFieldInit || fn.Kind == bytecode.KindStaticBlock:
		return "getter, setter, field initializer or static block"
	}
	return ""
}
