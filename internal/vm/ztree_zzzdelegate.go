package vm

// yield* to a generator, in the tree tier.
//
// A synchronous yield* calls its delegate's next method and yields the
// result object it gets, as it is: the caller of the outer generator's
// next sees the delegate's own object. Where the delegate is a generator
// whose next is the intrinsic one, of the running realm, nothing else sees
// that object either -- it is made by the delegate's next and read only by
// the yield* -- so the delegation steps the generator directly, as a
// for...of loop does (see stepIter): the delegate's next still has its
// frame, but neither the object nor the call's arguments are made, and
// what the delegate yields is yielded on as a value, which the outer
// generator's next puts in a result object of the same realm's, and a
// for...of loop over it reads as it is. A chain of delegations then hands
// a value up without an object at any level.
//
// The value travels from iter_resume to yield_star in the operand stack:
// the cursor itself stands where the result object would, a mark no result
// object can be, since a cursor is never seen by a script, and the value is
// in the slot above it; a delegate that has finished has its cursor marked
// done, as iter_unpack_delegate marks it. A delegate's result that is an
// object to hand over as it is -- a generator itself delegating to some
// other iterator -- goes the way any result goes.
//
// It is a variable assigned a closure, as the rest of the tree tier's
// suspension is, and in the last of its files, so that it is laid out
// after every closure already there (see AGENTS.md).

var (
	// delegateNext steps the delegate of the yield* whose cursor is in
	// slot d-1 with next(sent), where it is a generator whose next is the
	// intrinsic one of the running realm, and reports whether it did: the
	// cursor is then in slot d and what the delegate gave in slot d+1, or
	// a result object to hand over as it is in slot d.
	delegateNext func(c *tctx, d int, sent Value) bool
)

func init() {
	delegateNext = func(c *tctx, d int, sent Value) bool {
		cur := c.stack[d-1]
		st := iterStateOf(cur)
		if st == nil || st.forIn || st.arr != nil || !st.iter.IsObject() || !st.next.IsObject() {
			return false
		}
		n, it := st.next.Object(), st.iter.Object()
		fd := n.fn()
		if fd == nil || fd.iterNext != iterNextGenerator || (fd.realm != nil && fd.realm != c.r.Realm) ||
			it.class != ClassGenerator {
			return false
		}
		g, ok := it.dataOf().(*generator)
		if !ok {
			return false
		}
		r := c.r
		st.delegation = true
		// As stepIter, the delegate's next has its frame, which a stack
		// trace taken in the delegate sees.
		if err := r.tick(); err != nil {
			c.throw(err)
		}
		if err := r.nativeFrame(n, fd, st.iter, nil, Undefined); err != nil {
			c.throw(err)
		}
		var got resumeResult
		var err error
		ran := false
		if t := g.nextTree(); t != nil {
			got, ran, err = resumeTree(r, g, t, sent)
		}
		if !ran {
			got, err = r.resumeFull(g, sent, resumeNext)
		}
		r.frameDepth--
		if err != nil {
			c.throw(err)
		}
		if got.raw && got.value.IsObject() {
			c.stack[d] = got.value
			return true
		}
		c.stack[d] = cur
		c.stack[d+1] = got.value
		if got.done {
			st.done = true
		}
		return true
	}
}
