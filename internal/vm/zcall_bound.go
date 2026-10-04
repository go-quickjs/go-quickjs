package vm

// callBound is callObject's call of a bound function: the bound arguments
// put before the ones given, the bound this in place of the one given, a
// level of Go recursion entered -- and the target then called the way
// callFromLoop calls, which runs a compiled function directly, rather than
// through callObject again. The arguments are put together on the
// runtime's argument stack, where a callee may not keep them, rather than
// in a list made for the call.
//
// It is in the package's last file with the tail calls, and for the same
// reason: see zcall_tail.go.
func (r *Runtime) callBound(fd *funcData, args []Value) (Value, error) {
	// Each bound function in a chain is a level of Go recursion with no
	// frame of its own, so it is counted, or a long enough chain would take
	// the call past the Go stack.
	if err := r.nest(); err != nil {
		return Undefined, err
	}
	x := fd.extra
	if len(x.boundArgs) == 0 {
		v, err := r.callFromLoop(Obj(x.boundTarget), x.boundThis, args)
		r.unnest()
		return v, err
	}
	i := len(r.argStack)
	r.argStack = append(r.argStack, x.boundArgs...)
	r.argStack = append(r.argStack, args...)
	n := len(r.argStack)
	v, err := r.callFromLoop(Obj(x.boundTarget), x.boundThis, r.argStack[i:n:n])
	clear(r.argStack[i:n])
	r.argStack = r.argStack[:i]
	r.unnest()
	return v, err
}
