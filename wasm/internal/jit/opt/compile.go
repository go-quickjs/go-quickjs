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
// Inlining calls (inline.go) and forwarding the globals (forwardGlobals)
// keep values live that were made where used: a function whose frame then
// has too few slots for its spills is compiled again inlining less deep,
// then without forwarding the globals. One whose code, or whose working
// sets (Request.MaxWorkBytes), inlining made too large is compiled again
// without inlining.
func Compile(req *wjit.Request) (*wjit.Code, error) {
	inline := int32(0)
	if req.InlineSite != nil {
		inline = inlineDepth
	}
	forward := true
	for {
		code, used, err := compile(req, forward, inline)
		r, ok := err.(*wjit.Refusal)
		switch {
		case !ok:
			return code, err
		case used.inlined && (r.Reason == RefuseFrame || r.Reason == RefuseSize || r.Reason == refuseInline || r.Reason == wjit.RefuseWork):
			inline = min(inline, used.depth) - 1
			if r.Reason != RefuseFrame {
				inline = 0
			}
		case r.Reason == RefuseFrame && used.forwarded:
			forward = false
		default:
			return code, err
		}
	}
}

// refuseInline is the refusal of a compile whose inlined bodies have more
// sites than the engine gives offsets for, compiled again without inlining.
const refuseInline = "inline"

// compileUse is what a compile did that may cost a function its compile:
// whether it forwarded a global's read and inlined a call, and how deep.
type compileUse struct {
	forwarded, inlined bool
	depth              int32
}

// compile compiles req's function, forwarding the globals if forward says
// so and inlining calls up to inline calls deep, and reports what it did.
func compile(req *wjit.Request, forward bool, inline int32) (code *wjit.Code, used compileUse, err error) {
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
	ws.arrayClass = nil
	if req.Layout != nil {
		ws.arrayClass = req.Layout.ArrayClass
	}
	f := build(ws, req.Module, req.Func, inline)
	f.noForward, f.listing = !forward, req.Marks
	used.inlined = ws.bd.inl.used
	used.depth = ws.bd.inl.maxLevel
	defer func() { used.forwarded = f.forwarded }()
	Optimize(f, nil)
	if err := checkLowerable(f); err != nil {
		panic(err)
	}
	if !ws.bd.resolveInline(f, req.InlineSite) {
		refuse(refuseInline, "more inlined sites than offsets")
	}
	return lower(f, req, ws), used, nil
}
