//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

// A number joined to a string by native code's + (ssa.GoAdd) -- x + '',
// '#' + f, s + 1.5 -- as the number's string and the string concatenated,
// where jitConcatAdd, which jitGoAdd asks first, answers only an
// integer's (concatInt): any other number had gone on to add, its two
// conversions to primitives and its questions of which kind each operand
// was, which jitConcatAdd had asked, on top of the call of Go. A number's
// string is at most 25 code units; jitGoAdd has checked the string
// against the margin below maxStringLength, and making the string calls
// nothing and throws nothing.
//
// It is generic, instantiated with struct{} alone, and called where
// jitGoAdd's call of add was: Go lays a generic function's
// instantiations out after every function and closure of the package (see
// zzz_direct.go), and jitGoAdd's code grows within the padding that rounds
// it to 32 bytes.

// jitAddHook is a + b, of primitives jitGoAdd has checked: a number and a
// string concatenated, and anything else as add makes it.
func jitAddHook[_ any](r *Runtime, a, b Value) (Value, error) {
	switch {
	case a.IsString() && b.IsNumber():
		if s := a.String(); s.length <= maxStringLength-64 {
			return Str(s.Concat(r.numberString(b.num))), nil
		}
	case b.IsString() && a.IsNumber():
		if s := b.String(); s.length <= maxStringLength-64 {
			return Str(r.numberString(a.num).Concat(s)), nil
		}
	}
	return r.add(a, b)
}
