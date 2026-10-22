package ssa

import "testing"

// knownShapes marks a property operation of the shape an earlier one on the
// same object checked, with nothing between them that may change a shape:
// not after a write that adds a property (to any object), a call of Go that
// may write or run code, or a megamorphic operation, which checks nothing
// it fails on; not of another shape; across blocks only into one of one
// predecessor.
func TestKnownShapes(t *testing.T) {
	const s1, s2, s3 = 0x1000, 0x2000, 0x3000
	f := &Func{ThisSlot: -1}
	a := f.newBlock(0)
	b := f.newBlock(1)
	c := f.newBlock(2)
	b.Preds = []*Block{a}
	c.Preds = []*Block{a, b}
	obj := f.newValue(a, OpObjectOf, Ptr, f.newValue(a, OpConst, Tagged))
	other := f.newValue(a, OpObjectOf, Ptr, f.newValue(a, OpConst, Tagged))
	prop := func(blk *Block, op Op, o *Value, shape uint64) *Value {
		v := f.newValue(blk, op, Float64, o)
		v.Const.Bits = shape
		return v
	}
	call := func(blk *Block, op GoOp) *Value {
		v := f.newValue(blk, OpCall, Tagged)
		v.IC = &InlineCache{Calls: []*CallSite{{Go: op}}}
		return v
	}
	want := map[*Value]bool{}
	read := func(blk *Block, o *Value, shape uint64, known bool) *Value {
		v := prop(blk, OpPropRead, o, shape)
		want[v] = known
		return v
	}
	read(a, obj, s1, false)
	read(a, obj, s1, true)
	w := prop(a, OpPropWrite, obj, s1)
	want[w] = true
	call(a, GoAdd)
	read(a, obj, s1, true)
	read(a, other, s1, false)
	add := prop(a, OpPropWrite, other, 0)
	add.IC = &InlineCache{Add: &PropertyAdd{}}
	read(a, obj, s1, false)
	read(a, obj, s2, false)
	read(a, obj, s2, true)
	call(a, GoCall)
	read(a, obj, s2, false)
	mega := prop(a, OpPropRead, obj, s3)
	mega.IC = &InlineCache{Mega: true}
	want[mega] = false
	read(a, obj, s3, false)
	read(a, obj, s3, true)
	megaWrite := prop(a, OpPropWrite, other, s1)
	megaWrite.Type = Bool
	want[megaWrite] = false
	read(a, other, s1, false)
	// b's one predecessor ends knowing obj is of s3, and other of s1.
	read(b, obj, s3, true)
	read(b, other, s1, true)
	call(b, GoLeaf)
	read(b, obj, s3, false)
	// c joins a and b: nothing known.
	read(c, obj, s3, false)
	read(c, obj, s3, true)
	knownShapes(f)
	for v, known := range want {
		if v.ShapeKnown != known {
			t.Errorf("%v (%v of %v, shape %#x, in b%d): ShapeKnown %v, want %v", v, v.Op, v.Args[0], v.Const.Bits, v.Block.ID, v.ShapeKnown, known)
		}
	}
}
