package compiler

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// leafKind is which of the bodies bytecode.LeafKind names fn's compiled code
// is, or LeafNone. Only a function whose call does nothing before its body
// qualifies: no rest parameter or arguments object to make, no default or
// pattern to bind, no generator or async function to set up, no eval scope,
// and a `this` of its own -- not an arrow's, and not a derived
// constructor's, which super() binds.
func leafKind(fn *bytecode.Function) bytecode.LeafKind {
	if fn.Generator || fn.Async || fn.HasRest || fn.HasDirectEval || !fn.HasSimpleParams || fn.ParamsAreLexical {
		return bytecode.LeafNone
	}
	// this.k.apply(this, arguments) mentions arguments, but apply_arguments
	// hands the arguments on without the object being made.
	code := fn.Code
	if fn.Kind == bytecode.KindNormal && len(code) == 7 &&
		code[0].Op == bytecode.OpPushThis && code[1].Op == bytecode.OpGetProp &&
		code[2].Op == bytecode.OpGetPropThis && fn.Names[code[2].A] == "apply" &&
		code[3].Op == bytecode.OpPushThis && code[4].Op == bytecode.OpApplyArguments &&
		code[5].Op == bytecode.OpDrop && code[6].Op == bytecode.OpReturnUndef {
		return bytecode.LeafForward
	}
	if fn.UsesArguments {
		return bytecode.LeafNone
	}
	switch fn.Kind {
	case bytecode.KindNormal, bytecode.KindMethod, bytecode.KindGetter, bytecode.KindConstructor:
	default:
		return bytecode.LeafNone
	}
	param := func(in bytecode.Instr) bool {
		return in.Op == bytecode.OpGetLocal && int(in.A) < fn.ParamCount
	}
	// The return_undef that ends every body is unreachable after a return.
	if len(code) >= 4 && code[0].Op == bytecode.OpPushThis && code[1].Op == bytecode.OpGetProp {
		switch {
		case len(code) == 4 && code[2].Op == bytecode.OpReturn:
			return bytecode.LeafGetThis
		case len(code) == 5 && code[2].Op == bytecode.OpGetLength && code[3].Op == bytecode.OpReturn:
			return bytecode.LeafGetThisLength
		case len(code) == 6 && param(code[2]) && code[3].Op == bytecode.OpGetIndex && code[4].Op == bytecode.OpReturn:
			return bytecode.LeafGetThisIndex
		}
	}
	n := 0
	for n+3 <= len(code) && code[n].Op == bytecode.OpPushThis && param(code[n+1]) && code[n+2].Op == bytecode.OpSetProp {
		n += 3
	}
	rest := code[n:]
	ends := len(rest) == 1 && rest[0].Op == bytecode.OpReturnUndef ||
		fn.Kind == bytecode.KindConstructor && len(rest) == 3 &&
			rest[0].Op == bytecode.OpPushThis && rest[1].Op == bytecode.OpReturn
	if n > 0 && ends {
		return bytecode.LeafSetThis
	}
	return bytecode.LeafNone
}
