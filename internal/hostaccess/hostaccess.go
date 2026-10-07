// Package hostaccess lets this module's own packages -- the standard
// library, qjs and the inspector -- reach what a quickjs.Runtime keeps from
// its users: the VM runtime under it, its values, and evaluating code of the
// module's own that a debugger is not to see.
//
// Package quickjs sets the functions when it is initialized; the types are
// the package's, passed as any because this package cannot import it.
package hostaccess

import "github.com/go-quickjs/go-quickjs/internal/vm"

var (
	// VM is the VM runtime of a *quickjs.Runtime.
	VM func(rt any) *vm.Runtime
	// Wrap is a VM value as the quickjs.Value of a *quickjs.Runtime, and
	// Unwrap the VM value of a quickjs.Value.
	Wrap   func(rt any, v vm.Value) any
	Unwrap func(v any) vm.Value
	// EvalInternal evaluates a script of the module's own in a
	// *quickjs.Runtime, as EvalFile does but compiled as though the runtime
	// had no debugger: one made WithDebugger neither lists it nor stops in
	// it, and runs it as fast as any other runtime would. It returns a
	// quickjs.Value.
	EvalInternal func(rt any, name, src string) (any, error)
)
