//go:build arm64

package opt

import wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"

// Available says this architecture has the optimizing tier's lowering:
// only amd64 does, and elsewhere every function keeps its baseline code.
const Available = false

func lower(*Func, *wjit.Request, *workspace) *wjit.Code { panic("unreachable") }
