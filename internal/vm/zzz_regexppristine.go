package vm

// pristineMinProps is the fewest properties an object armPristine arms may
// have: more than native code searches a table of for a write (abi.MaxScan),
// which writes through no entry of a cache's, and so would not clear
// pristine.
const pristineMinProps = 8

// armRealm is armPristine, which initRealm calls through it so that the
// package's generic code that comes after it is laid out where it was.
var armRealm = armPristine[struct{}]

// armPristine makes the layouts of the realm's RegExp.prototype and RegExp,
// just built, pristine, as V8 sets its RegExp protectors: the RegExp
// methods' fast paths then ask nothing more of them, until a script writes
// to one or changes its layout (shape's pristine). Each is given a layout of
// its own first, which no cache can have an entry of, and which is
// remembered, so that any change to its layout gives the object another.
func armPristine[_ any](r *Runtime) {
	p := &r.proto
	armObject[struct{}](r, p.regexp, r.regexpFlagProps, r.regexpExecProps, r.regexpTestProps,
		r.regexpReplaceProps, r.regexpSplitProps, r.regexpSymSplitProps, r.regexpSymMatchProps)
	armObject[struct{}](r, p.regexpCtor, r.regexpSpeciesProps)
}

// armObject makes o's layout pristine, if o is as its realm recorded it in
// lists, every one of them.
func armObject[_ any](r *Runtime, o *Object, lists ...[]builtinProp) {
	if o == nil || len(o.vals) <= pristineMinProps || r.shapes.building {
		return
	}
	r.ensureProtoShape(o)
	s := o.shape
	if s == nil || s.loose {
		return
	}
	for _, l := range lists {
		if len(l) == 0 || !propsIntact(o, l) {
			return
		}
		for k := range l {
			if o.class != ClassObject && !l[k].p.isAccessor() {
				// A write a function's walk makes in place does not
				// clear pristine (setProp): only the fast path's, for an
				// ordinary object, does. An accessor is written by no
				// assignment, only defined.
				return
			}
		}
	}
	// A layout of its own that no cache has seen: the old one remembered,
	// the change is to a new one.
	s.seen = true
	o.layoutChanged()
	ns := o.shape
	ns.seen = true
	ns.pristine = true
}
