package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// An object literal's fields, in the tree tier, as one node that makes the
// object with its final shape, as V8 makes a literal of its boilerplate's
// map (CreateShallowObjectLiteral) and never walks its transitions again.
// A literal had been a new_object's node, then a statement for each
// define_field, which found the transition from the shape the object had
// (Object.transition) and appended the value along it
// (appendTransition): for { a: j, b: j + 1, c: "key", ... } of eight
// fields, eight closures, eight transitions and eight shape stores per
// object, the define_fields alone 40% of the loop making it.
//
// The new_object's entry (tentryRare's lit) takes each define_field that
// follows it while it is still a tree: the field's value, its own tree or
// what it reads in place -- a local, a constant -- is evaluated by the
// literal's node, in the fields' order, where the define_field's statement
// would have evaluated it. Anything that evaluates the entry before --
// a spill, for a statement or a call, a spread or an accessor's
// definition -- takes it with the fields it has, and the rest are
// define_fields as before, on the object in its slot. So the values are
// evaluated in the order they were, and nothing else runs between them
// and the object's making that had not before: the object is nothing
// anything can see until the node returns it.
//
// The node makes the object as the new_object's did, from the site's root
// (siteRoot) with its room. Its first evaluations define the fields as the
// define_fields did, along the transitions from the object's shape, or by
// defineOwnProp; where every field was added along a transition from the
// root, the shape it ends with is the literal's, which the site keeps
// (propCache's next, with its root in s1: a new_object's site holds
// nothing else), and an object made from that root again is given its
// values in place and that shape: transitions are never undone, so the
// root's chain for the same keys leads to it every time. Keys that repeat,
// which the second defines again, are not taken.
//
// Its functions are generic, instantiated with struct{} alone, which Go
// lays out after every function and closure of the package (see
// zzz_direct.go); opHook (zzz_typeof.go) asks this file first, and
// tentryRare has a word more, for lit.

// litOf is a literal's entry's record of the fields it took: its
// new_object's operand count and site, how many statements the block had
// when it was made, and the fields. closed says it takes no more.
type litOf struct {
	fields []litField
	// keys is the hash of the literal's keys (LiteralKeys), 0 where its
	// function does not say them.
	keys   uint64
	n      int
	body   int
	site   uint32
	closed bool
}

// litField is one field a literal took: its name, the pc of its
// define_field, and its value -- v, a tree, or, by kind, the constant n or
// local k, read in place.
type litField struct {
	n    Value
	v    tval
	name uint32
	pc   int32
	k    uint32
	kind uint32
}

// The ways a field's value is had.
const (
	litTree = iota
	litConst
	litLocal
)

// maxLitFields is the most fields a literal's node takes: past it, the
// define_fields are statements of their own.
const maxLitFields = 64

// literalNew is opHook's for a new_object: its entry, which takes the
// define_fields after it.
//
//go:noinline
func literalNew[_ any](b *tbuilder, in bytecode.Instr) {
	lit := &litOf{n: int(in.A), site: in.B, body: len(b.body), keys: keysOf[struct{}](b.fn, in.B)}
	b.pushEntry(tentry{v: literalNode[struct{}](lit), slot: -1, rare: &tentryRare{lit: lit}})
}

// literalField is opHook's for a define_field at pc: true where the
// object it defines on is a literal's entry still a tree, which takes the
// field, its value popped; false for op to build it.
func literalField[_ any](b *tbuilder, pc int, in bytecode.Instr) bool {
	n := len(b.stack)
	if n < 2 || pc > 1<<31-1 {
		return false
	}
	o := b.stack[n-2]
	if o.rare == nil || o.rare.lit == nil || o.v == nil || o.slot >= 0 {
		return false
	}
	lit := o.rare.lit
	if lit.closed || lit.body != len(b.body) || len(lit.fields) >= maxLitFields {
		lit.closed = true
		return false
	}
	name := b.fn.Names[in.A]
	for _, f := range lit.fields {
		if b.fn.Names[f.name] == name {
			lit.closed = true
			return false
		}
	}
	e := b.pop()
	f := litField{name: in.A, pc: int32(pc)}
	switch {
	case e.v == nil && e.slot < 0 && (e.number || e.literal):
		f.kind, f.n = litConst, e.n
	case e.v == nil && e.slot < 0 && e.local:
		f.kind, f.k = litLocal, e.k
	default:
		f.kind, f.v = litTree, e.tree()
	}
	lit.fields = append(lit.fields, f)
	return true
}

// literalNode is a literal's node: the object new_object makes, with the
// fields lit took. A method value of a generic type's, rather than a
// closure, which Go would lay out before the generic functions' closures
// that are there already, moving them.
func literalNode[_ any](lit *litOf) tval {
	return (*litNodeOf[struct{}])(lit).make
}

// litNodeOf is litOf with the node's method.
type litNodeOf[_ any] litOf

// make is the node.
func (l *litNodeOf[_]) make(c *tctx) Value {
	lit := (*litOf)(l)
	ic := &c.ic[lit.site]
	if lit.keys != 0 {
		if s := ic.shape; s != nil && s.next == nil {
			literalRoot[struct{}](c, lit, ic)
		}
	}
	root := c.r.shapes.siteRoot(ic, c.r.proto.object)
	m := lit.n
	if root != nil && int(root.slack) > m {
		m = int(root.slack)
	}
	o := newShapedLiteral(root.orRoot(ClassObject, c.r.proto.object), ClassObject, m)
	fs := lit.fields
	if len(fs) == 0 {
		return Obj(o)
	}
	if final := ic.next; final != nil && root != nil && ic.s1 == root && len(fs) <= int(o.valc) {
		// The shape its fields lead to from the root: the values in
		// place.
		vals := o.vals()[:len(fs)]
		for i := range fs {
			f := &fs[i]
			switch f.kind {
			case litConst:
				vals[i] = f.n
			case litLocal:
				vals[i] = c.locals[f.k]
			default:
				vals[i] = f.v(c)
			}
		}
		o.setVals(vals)
		o.shape = final
		if verifyShapes {
			checkShape(o)
		}
		return Obj(o)
	}
	literalDefine[struct{}](c, o, root, ic, fs)
	return Obj(o)
}

// literalDefine defines fs on o, of root, as their define_fields would
// have, and keeps the shape they lead to in ic where each was added along
// a transition.
func literalDefine[_ any](c *tctx, o *Object, root *shape, ic *propCache, fs []litField) {
	along := root != nil
	for i := range fs {
		f := &fs[i]
		var v Value
		switch f.kind {
		case litConst:
			v = f.n
		case litLocal:
			v = c.locals[f.k]
		default:
			v = f.v(c)
		}
		key := c.names[f.name]
		if o.class == ClassObject && o.flags&objExtensible != 0 {
			if next := o.transition(key, propDefault); next != nil {
				o.appendTransition(v, next)
				continue
			}
		}
		along = false
		c.at(int(f.pc))
		if err := c.r.defineOwnProp(o, key, v, propDefault); err != nil {
			c.throw(err)
		}
	}
	if s := o.shape; along && s != nil && !s.unique && !s.loose && int(s.n) == len(fs) && int(o.valn) == len(fs) &&
		(s.index != nil || len(fs) <= linearScanLimit) {
		ic.next, ic.s1 = s, root
	}
}
