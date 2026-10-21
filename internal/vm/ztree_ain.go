package vm

// The in operator answered from what plain objects and arrays hold, by
// every tier: the tree tier's node (inNode) and native code's call of Go
// (jitHas) ask hasQuick first, and inOperator, as executeAt's case does,
// only for what it does not answer -- a proxy's trap, a key to convert, a
// typed array's index, a module namespace, an exotic object's own keys.
//
// The code is closures assigned in this file's init, the first of the
// package's inits with closures, whose closures are laid out after every
// other closure (see ztree_zkself.go): code added here moves nothing the
// tree tier or the interpreter runs.

var (
	// hasQuick is key in o where the answer is in o's and its prototypes'
	// own storage and asking it runs nothing: each of them an ordinary
	// object or an array, whose own properties are its elements and its
	// table, and an array's length -- as hasPropErr and hasOwnProp find
	// them -- by a key that is an index, a number's or a string's, or a
	// string whose atom the runtime remembers with it (keyAtoms), as
	// toPropertyName finds it. An index absent from an object's elements
	// is looked for in its table only where the table may have one
	// (noIndexKeys). It reports false for anything else, which inOperator
	// answers: a key of another kind, a string whose atom is not
	// remembered, a chain with any other object on it.
	hasQuick func(r *Runtime, key Value, o *Object) (has, ok bool)

	// hasIndexQuick is hasQuick of the index i, below atomIndexTag.
	hasIndexQuick func(o *Object, i uint32) (has, ok bool)

	// inNode builds in, the in operator, at pc: hasQuick's answer where
	// it has one, inOperator's otherwise.
	inNode func(b *tbuilder, pc int)
)

func init() {
	hasIndexQuick = func(o *Object, i uint32) (bool, bool) {
		for p := o; p != nil; p = p.proto {
			if p.class > ClassArray {
				return false, false
			}
			if e := p.elemsOf(); uint(i) < uint(len(e)) && !isHole(e[i]) {
				return true, true
			}
			if !p.noIndexKeys() {
				if j := p.findOwn(Atom(atomIndexTag | i)); j >= 0 && p.shape.keys[j].flags&propPrivate == 0 {
					return true, true
				}
			}
		}
		return false, true
	}
	hasQuick = func(r *Runtime, key Value, o *Object) (bool, bool) {
		var a Atom
		switch {
		case key.IsNumber():
			// An index, as toPropertyName makes it: -0 is 0.
			f := key.num
			if i := uint32(f); float64(i) == f && i < atomIndexTag {
				return hasIndexQuick(o, i)
			}
			return false, false
		case key.IsString():
			s := key.String()
			e := &r.keyAtoms[keySlot(s)]
			if e.s != s {
				return false, false
			}
			if e.none {
				// A key no property has, while it names no atom, on a
				// chain of objects that holds its keys.
				if e.atom != r.atoms.next() {
					return false, false
				}
				for p := o; p != nil; p = p.proto {
					if p.class > ClassArray {
						return false, false
					}
				}
				return false, true
			}
			a = e.atom
		default:
			return false, false
		}
		if a.IsIndex() {
			return hasIndexQuick(o, a.Index())
		}
		for p := o; p != nil; p = p.proto {
			if p.class > ClassArray {
				return false, false
			}
			if a == atomLength && p.class == ClassArray {
				return true, true
			}
			if j := p.findOwn(a); j >= 0 && p.shape.keys[j].flags&propPrivate == 0 {
				return true, true
			}
		}
		return false, true
	}
	inNode = func(b *tbuilder, pc int) {
		y, x := b.pop().tree(), b.pop().tree()
		b.push(func(c *tctx) Value {
			l := x(c)
			r := y(c)
			if r.IsObject() {
				if has, ok := hasQuick(c.r, l, r.object()); ok {
					return Bool(has)
				}
			}
			c.at(pc)
			v, err := inOperator(c.r, l, r)
			if err != nil {
				c.throw(err)
			}
			return v
		})
	}
}
