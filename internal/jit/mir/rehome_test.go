package mir

import (
	"slices"
	"testing"

	jscompile "github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// A value live at a loop header's poll that the header's state does not
// name is followed back to what holds the objects it reads (rehomeFollow):
// a frame register it reads through is pinned, which Go then never writes
// there; a value of the state, or a cell the runtime or the context keeps,
// ends the way; and a phi the state does not name, whose value at run time
// may be any turn's, leaves the header with nothing Go may write.
func TestRehomeFollow(t *testing.T) {
	c := &core{f: &ssa.Func{FrameLocals: 8}}
	var all []*ssa.Value
	val := func(op ssa.Op, typ ssa.Type, aux int, args ...*ssa.Value) *ssa.Value {
		v := &ssa.Value{ID: len(all), Op: op, Type: typ, Aux: aux, Args: args}
		all = append(all, v)
		return v
	}
	cell := func(obj *ssa.Value) *ssa.Value {
		p := val(ssa.OpPropCell, ssa.Source, 0, val(ssa.OpObjectOf, ssa.Ptr, 0, obj))
		v := val(ssa.OpLoadCell, ssa.Tagged, 0, p)
		v.Shadow = p
		return v
	}
	state := val(ssa.OpPhi, ssa.Tagged, 0)
	other := val(ssa.OpPhi, ssa.Tagged, 0)
	cases := []struct {
		name   string
		u      *ssa.Value
		pinned []int32
		ok     bool
	}{
		{"a cell of a state's value", val(ssa.OpPropCell, ssa.Source, 0, val(ssa.OpObjectOf, ssa.Ptr, 0, state)), nil, true},
		{"a value read from an object a register held at the entry", cell(val(ssa.OpLoadSlot, ssa.Tagged, 3)), []int32{3}, true},
		{"through two objects", cell(cell(val(ssa.OpLoadSlot, ssa.Tagged, 5))), []int32{5}, true},
		{"the receiver's, never written", cell(val(ssa.OpLoadSlot, ssa.Tagged, 9)), nil, true},
		{"a slot's address", val(ssa.OpConstSource, ssa.Source, 6), []int32{6}, true},
		{"no slot's", val(ssa.OpConstSource, ssa.Source, -1), nil, true},
		{"a global's cell", cell(func() *ssa.Value {
			g := val(ssa.OpGlobalCell, ssa.Source, 0)
			v := val(ssa.OpLoadCell, ssa.Tagged, 0, g)
			v.Shadow = g
			return v
		}()), nil, true},
		{"an object's address, of a phi no state names", val(ssa.OpObjectOf, ssa.Ptr, 0, other), nil, false},
		{"a cell of one", cell(other), nil, false},
	}
	for _, tc := range cases {
		stamp := make([]int32, len(all))
		stamp[state.ID] = 2
		var reached []int32
		pinned, ok := c.rehomeFollow(tc.u, stamp, 1, nil, &reached)
		if ok != tc.ok || ok && !slices.Equal(sortedSlots(pinned), tc.pinned) {
			t.Errorf("%s: pinned %v, %v; want %v, %v", tc.name, pinned, ok, tc.pinned, tc.ok)
		}
	}
	if got := sortedSlots([]int32{4, 1, 4, 2, 1}); !slices.Equal(got, []int32{1, 2, 4}) {
		t.Errorf("sortedSlots: %v", got)
	}
}

// A loop's back edge whose header's state has a register the loop
// reassigns polls with what Go may write there (abi.GoCallState's Rehome):
// the header's exit description, and the register, whose value's source
// is a shadow phi, among those whose source Go may make its own.
func TestRehomeDescribed(t *testing.T) {
	a, err := parser.Parse(`function f(n, a, b) { for (let i = 0; i < n; i++) a = i & 1 ? a : b; return a }`, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fn, err := jscompile.Compile(a, jscompile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fn = fn.Constants[0].Fn
	p, err := jitcompile.LowerReg(fn.RegCode(), fn)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ssa.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	ssa.Optimize(f)
	for _, arch := range []string{"amd64", "arm64"} {
		var w Workspace
		w.init()
		enc := abi.Encoding{ValueSize: 16, StoreRef: 1, CallGo: 1}
		var code *Code
		if arch == "amd64" {
			code, err = compileAMD64(&w, f, enc, Settings())
		} else {
			code, err = compileARM64(&w, f, enc, Settings())
		}
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, st := range code.GoCalls {
			if h := st.Rehome; ssa.GoOp(st.Op&0xff) == ssa.GoPoll && h != nil {
				found = true
				if h.Desc == nil || h.Desc.Root == 0 || !slices.Contains(h.Marks, abi.RehomeRewire) {
					t.Errorf("%s: a poll's rehome %+v, want the header's description and a register to rewire", arch, h)
				}
			}
		}
		if !found {
			t.Errorf("%s: no poll with what Go may write", arch)
		}
	}
}
