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
func Compile(req *wjit.Request) (code *wjit.Code, err error) {
	defer func() {
		if p := recover(); p != nil {
			if r, ok := p.(*wjit.Refusal); ok {
				code, err = nil, r
				return
			}
			code, err = nil, &wjit.Refusal{Reason: "internal", Detail: fmt.Sprint(p)}
		}
	}()
	if !Available {
		refuse(RefuseArch, "no lowering for this architecture")
	}
	start()
	f, err := Build(req.Module, req.Func)
	if err != nil {
		return nil, err
	}
	Optimize(f, nil)
	if err := checkLowerable(f); err != nil {
		panic(err)
	}
	ws := workspaces.Get().(*workspace)
	defer workspaces.Put(ws)
	return lower(f, req, ws), nil
}
