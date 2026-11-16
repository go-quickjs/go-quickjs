package vm

// The close of a for await loop's or a yield*'s asynchronous iterator
// (asyncIterClose).
//
// Its functions are variables, each assigned a closure in init, laid
// out after every other closure of the package, so that no hot code
// moves for them (see AGENTS.md).

var (
	// asyncIterClose is async_iter_close: with mode 0, the
	// call of the return method of the iterator of v, a for await's cursor,
	// which reports what it returned, or false where an asynchronous
	// iterator has none; with mode 1, the check of the result awaited, v;
	// with mode 2, v marked done, unclosed.
	asyncIterClose func(r *Runtime, v Value, mode uint32) (Value, bool, error)
)

func init() {
	asyncIterClose = func(r *Runtime, v Value, mode uint32) (Value, bool, error) {
		switch mode {
		case 1:
			if !v.IsObject() {
				return Undefined, false, r.throwTypeError("the iterator's return method must return an object")
			}
			return Undefined, false, nil
		case 2:
			if st := cursorOf(v); st != nil {
				st.done = true
			}
			return Undefined, false, nil
		}
		st := cursorOf(v)
		if st == nil || st.done {
			return Undefined, false, nil
		}
		st.done = true
		if st.asyncIter {
			ret, err := r.getValueProp(st.iter, atomReturn)
			if err != nil {
				return Undefined, false, err
			}
			if ret.IsNullish() {
				return Undefined, false, nil
			}
			if !isCallable(ret) {
				return Undefined, false, r.throwTypeError("the iterator's return method is not callable")
			}
			res, err := r.call(ret, st.iter, nil)
			if err != nil {
				return Undefined, false, err
			}
			return res, true, nil
		}
		// A synchronous iterator is driven through the spec's wrapper of it,
		// whose return method always answers: with what the iterator's own
		// return gives, its value awaited, or, where it has none, a result
		// saying it is done.
		if st.arr == nil {
			ret, err := r.getValueProp(st.iter, atomReturn)
			if err != nil {
				return Undefined, false, err
			}
			if !ret.IsNullish() {
				if !isCallable(ret) {
					return Undefined, false, r.throwTypeError("the iterator's return method is not callable")
				}
				res, err := r.call(ret, st.iter, nil)
				if err != nil {
					return Undefined, false, err
				}
				return r.asyncResult(st, res), true, nil
			}
		}
		return Obj(r.iterResult(Undefined, true)), true, nil
	}
}
