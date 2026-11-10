//go:build amd64 || arm64

package opt

import (
	"fmt"

	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
)

// Compile compiles function req.Func of a validated module with the
// optimizing tier, or refuses it with a *jit.Refusal: the function then
// keeps its baseline code. The code keeps the baseline's frame, context,
// entries and exits, so that it replaces the baseline code in the link
// table and the tiers call each other.
//
// Forwarding the globals (forwardGlobals) keeps values live that were
// loaded where used: a function whose frame then has too few slots for
// its spills is compiled again without it.
func Compile(req *wjit.Request) (*wjit.Code, error) {
	code, forwarded, err := compile(req, true)
	if r, ok := err.(*wjit.Refusal); ok && r.Reason == RefuseFrame && forwarded {
		code, _, err = compile(req, false)
	}
	return code, err
}

// compile compiles req's function, forwarding the globals if forward says
// so, and reports whether it forwarded any read.
func compile(req *wjit.Request, forward bool) (code *wjit.Code, forwarded bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			if r, ok := p.(*wjit.Refusal); ok {
				code, err = nil, r
				return
			}
			code, err = nil, &wjit.Refusal{Reason: "internal", Detail: fmt.Sprint(p)}
		}
	}()
	start()
	ws := workspaces.Get().(*workspace)
	defer workspaces.Put(ws)
	f := build(ws, req.Module, req.Func)
	f.noForward = !forward
	defer func() { forwarded = f.forwarded }()
	Optimize(f, nil)
	if err := checkLowerable(f); err != nil {
		panic(err)
	}
	return lower(f, req, ws), false, nil
}
