package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Using declarations: the steps of an await using scope's disposal
// (usingStep).
//
// Its functions are variables, each assigned a closure in init, laid
// out after every other closure of the package, so that no hot code
// moves for them (see AGENTS.md).

var (
	// usingStep is a step of an await using scope's disposal, as the
	// specification's DisposeResources walks it, the awaits
	// it makes being the compiler's between the steps. cp is the scope's
	// capability. The first step, with DisposeStart in mode, takes the
	// resources, and, with DisposeRecord, the completion the finally
	// clause's record beneath -- a the value, b the kind -- says the scope
	// is leaving with. One with DisposeFail folds a, what the await before
	// it threw, into the completion. Each runs the disposers, last added
	// first, until one leaves a value to await, which it reports, or none
	// is left, when it reports false. The end, with DisposeFinish, throws
	// what the disposers threw where it is not what the scope was already
	// leaving with.
	usingStep func(r *Runtime, cp, a, b Value, mode uint32) (Value, bool, error)
)

func init() {

	usingStep = func(r *Runtime, cp, a, b Value, mode uint32) (Value, bool, error) {
		c := cp.Object().dataOf().(*disposeCapability)
		switch {
		case mode&bytecode.DisposeStart != 0:
			c.walk, c.stack = c.stack, nil
			c.next = len(c.walk) - 1
			c.needsAwait, c.hasAwaited, c.completion = false, false, nil
			if mode&bytecode.DisposeRecord != 0 && completionKind(b.Number()) == completionThrow {
				c.completion = r.throw(a)
			}
			c.leaving = c.completion
		case mode&bytecode.DisposeFail != 0:
			// What an async disposer returned rejected.
			c.completion = r.suppress(c.completion, r.throw(a))
		case mode&bytecode.DisposeFinish != 0:
			err, leaving := c.completion, c.leaving
			c.walk, c.completion, c.leaving = nil, nil, nil
			if err != nil && err != leaving {
				return Undefined, false, err
			}
			return Undefined, false, nil
		}
		for c.next >= 0 {
			res := c.walk[c.next]
			if !res.async && c.needsAwait && !c.hasAwaited {
				// A sync disposer after an await using of nothing waits
				// for the await it owed.
				c.needsAwait = false
				return Undefined, true, nil
			}
			c.next--
			if res.method.IsUndefined() {
				c.needsAwait = true
				continue
			}
			v, err := r.call(res.method, res.value, nil)
			if err != nil {
				c.completion = r.suppress(c.completion, err)
				if _, ok := err.(*Thrown); !ok {
					c.next = -1
					return Undefined, false, err
				}
				continue
			}
			if res.async {
				c.hasAwaited = true
				return v, true, nil
			}
		}
		if c.needsAwait && !c.hasAwaited {
			// An await using of nothing still awaits once.
			c.needsAwait = false
			return Undefined, true, nil
		}
		return Undefined, false, nil
	}
}
