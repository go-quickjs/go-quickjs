package ssa

// Shapes known.
//
// A property operation of one shape its site knows (Const.Bits) -- with no
// cases or holders of its own, no search of the object's table or of the
// megamorphic cache past it, and no addition -- exits unless its object is
// of that shape: past it, the object is, until something changes a shape.
// Native code changes one only where a write adds a property; Go changes
// them in a call of Go that writes or deletes properties, or runs code (a
// call, a step of a generator, a leaf, a loop's poll), not in one that only
// computes, reads or makes something new (shapeSafeGo). An operation of
// the same shape on the same object after the first, with nothing between
// them that may change a shape, checks nothing (Value's ShapeKnown), as
// V8's load elimination drops a map check another dominating it made:
// knownShapes finds them in each block, and in a block whose one
// predecessor is earlier in the order, from where that one ends. Which
// object is a value's: two values that may be the same object are each
// checked once, and anything that changes a shape forgets every object's.

// shapeFact is what knownShapes knows at a block's end: the object, a
// value's ID, is of shape.
type shapeFact struct {
	obj   int
	shape uint64
}

// checksShape reports whether v exits unless its object is of the shape it
// names, and does nothing else with the shape.
func checksShape(v *Value) bool {
	switch v.Op {
	case OpPropRead, OpPropCell, OpPropWrite:
	default:
		return false
	}
	// A megamorphic write's result is whether it stored (Bool): one that
	// did not, Go stores on a branch of its own, without an exit.
	if v.Const.Bits == 0 || v.Upvalue || v.Global || len(v.Args) == 0 || v.Op == OpPropWrite && v.Type == Bool {
		return false
	}
	if ic := v.IC; ic != nil && (ic.Holders != nil || len(ic.Cases) != 0 || ic.Add != nil || len(ic.Adds) != 0 || ic.Mega || ic.Scan || ic.Try) {
		return false
	}
	return true
}

// changesShapes reports whether v may change any object's shape.
func changesShapes(v *Value) bool {
	switch v.Op {
	case OpPropWrite:
		return v.IC != nil && (v.IC.Add != nil || len(v.IC.Adds) != 0)
	case OpCall:
		sites := v.Calls()
		if len(sites) == 0 {
			return true
		}
		for _, s := range sites {
			if !shapeSafeGo(s.Go) {
				return true
			}
		}
	}
	return false
}

// shapeSafeGo reports whether a call of Go of op changes no shape: it
// computes, reads, or makes something new, and runs no code.
func shapeSafeGo(op GoOp) bool {
	switch op {
	case GoAdd, GoRefill, GoPow, GoMath, GoAtan2, GoHypot, GoMod, GoTypedRead, GoElemCell, GoConcat, GoIndex, GoAppend,
		GoCompare, GoRandom, GoNumber, GoEqual, GoCells, GoHas, GoClosure, GoIndexOf, GoNewArray, GoArith, GoBigString:
		return true
	}
	return false
}

// knownShapes marks the property operations whose object's shape an
// earlier one checked (Value's ShapeKnown).
func knownShapes(f *Func) {
	sc := f.scr()
	known := idTable(sc.shapes, f.nextID)
	sc.shapes = known
	ends := sc.shapeEnds[:0]
	starts := idTable(sc.shapeStarts, 2*len(f.Blocks))
	sc.shapeStarts = starts
	// pos is each block's place in the order, past one; 0 for one not met
	// yet.
	maxID := 0
	for _, b := range f.Blocks {
		maxID = max(maxID, b.ID)
	}
	pos := idTable(sc.blockPos, maxID+1)
	sc.blockPos = pos
	held := sc.shapeHeld[:0]
	for i, b := range f.Blocks {
		pos[b.ID] = i + 1
		for _, id := range held {
			known[id] = 0
		}
		held = held[:0]
		if len(b.Preds) == 1 {
			if j := pos[b.Preds[0].ID] - 1; j >= 0 && j < i {
				for _, x := range ends[starts[2*j]:starts[2*j+1]] {
					known[x.obj] = x.shape
					held = append(held, x.obj)
				}
			}
		}
		for _, v := range b.Values {
			if changesShapes(v) {
				for _, id := range held {
					known[id] = 0
				}
				held = held[:0]
				continue
			}
			if !checksShape(v) {
				continue
			}
			obj := v.Args[0].ID
			if known[obj] == v.Const.Bits {
				v.ShapeKnown = true
				continue
			}
			if known[obj] == 0 {
				held = append(held, obj)
			}
			known[obj] = v.Const.Bits
		}
		starts[2*i] = len(ends)
		for _, id := range held {
			if known[id] != 0 {
				ends = append(ends, shapeFact{obj: id, shape: known[id]})
			}
		}
		starts[2*i+1] = len(ends)
	}
	for _, id := range held {
		known[id] = 0
	}
	clear(pos)
	sc.shapeEnds, sc.shapeHeld = ends[:0], held[:0]
}
