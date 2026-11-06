package vm

// A for...in's cursor, used again. A loop's cursor is a 160-byte object no
// script can reach, and rendering a page makes thousands of them, a loop
// over each element's props: 15% of the bytes the SSR benchmark's render
// allocates. So the runtime keeps the last cursor it made
// (Runtime.forInLast), and the next loop takes it if its loop has finished.
//
// A cursor's done is set at its last step, by iterNext or native code's
// forInStep, and that is the whole of its release: after it, what is left
// of the loop is iter_close, which leaves a for...in's cursor alone, and a
// drop, so nothing reads the cursor again, whenever the next loop takes it.
// Every loop's start comes through here, from every tier, native code's by
// jitForInPlain, so native code needs no release of its own.
//
// What that rests on. No script reaches a cursor, so its loop is its only
// reader, and a loop that is running is not done: a nested loop, a generator
// suspended in one, a proxy's trap in the middle of a step and a recursive
// call all find the last cursor not done, and make their own. Unwinding never
// marks a for...in's cursor done (iterClose and its kin pass it by), so
// a loop left and re-entered by a finally's continue keeps its own. Only done
// and idx change in a loop's life: iter, next and arr are a for-of's, so that
// a used cursor needs nothing but those two and what it walks made again.
//
// A cursor made done, of null or undefined, is not kept: it is done before
// its loop's first step, which a statement's debugger pause may come ahead
// of. One whose loop was left by a break or a throw is not done, and is
// the collector's; the next cursor takes its place.
//
// The cursor holds the object its loop walked, so the runtime lets it go
// each time the collector has run and when its turn ends
// (clearReturnedFrames), as it does a stale slot's value.
//
// The code here is generic, instantiated with struct{}, as zzz_zliteral.go's,
// and newForInObject checks the last in place: 32 bytes off it for the 32
// clearReturnedFrames gains, so that the hot paths, past both, stay where
// they were.

// forInCursor is newForInObject's new cursor over in, kept unless it is
// made done.
//
//go:noinline
func forInCursor[_ any](r *Runtime, in *forInState, done bool) Value {
	fo := &forInObject{iterObject: iterObject{Object: Object{class: ClassCursor},
		st: iterState{forIn: true, done: done}}, in: *in}
	if !done {
		r.forInLast = fo
	}
	return Obj(&fo.Object)
}

// forInAgain is newForInObject's copy of what a used cursor walks now.
//
//go:noinline
func forInAgain[_ any](fo *forInObject, in *forInState) { fo.in = *in }

// forInLetGo lets the last cursor go, and what it walked.
//
//go:noinline
func forInLetGo[_ any](r *Runtime) { r.forInLast = nil }
