package ssa

import (
	"reflect"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

func TestPropertyLoweringKeepsCachedFeedback(t *testing.T) {
	for _, arena := range []bool{false, true} {
		t.Run(map[bool]string{false: "heap", true: "workspace"}[arena], func(t *testing.T) {
			p := &ir.Program{Locals: 2, Code: []ir.Instruction{
				{Op: ir.PropertyWrite, Left: ir.Slot(0), Right: ir.Slot(1), Key: 7},
				{Op: ir.Return, Left: ir.Slot(1)},
			}, Maps: []ir.StateMap{{PC: 0}, {PC: 1}}}
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
			want := PropertySite{Key: 7, Index: -1,
				Cases: []PropertyCase{{Shape: 0x10000, Index: 3}},
			}
			fb := siteFeedback{want}
			f := &Func{Locals: p.Locals, FrameLocals: p.Locals, ThisSlot: -1}
			if arena {
				f.ws = &Workspace{}
				f.ws.init()
				defer f.ws.Rewind()
			}
			f.written = f.bools(p.Locals)
			fr := &frame{p: p, fb: fb, at: -1, givenPC0: -1}
			b := &builder{p: p, fb: fb, f: f, nslots: p.Locals, cur: fr, root: fr}
			if err := b.plan(); err != nil {
				t.Fatal(err)
			}
			b.translate()
			if b.atomicErr != nil {
				t.Fatal(b.atomicErr)
			}
			if count(f, func(v *Value) bool {
				return v.Op == OpPropWrite && v.Const.Bits == 0x10000 && v.Index == 3
			}) != 1 {
				t.Fatalf("write did not specialize its leading case:\n%s", f)
			}
			if got := b.answer(0).site; !reflect.DeepEqual(got, want) {
				t.Fatalf("lowering changed cached feedback: got %+v, want %+v", got, want)
			}
		})
	}
}
