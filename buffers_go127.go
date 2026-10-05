//go:build go1.27

package quickjs

// NewTypedArray is the package's NewTypedArray as a method, for a program
// built with Go 1.27 or later, which has generic methods: the typed array of
// s's element type over s's elements, copied or shared as mode says.
//
//	a, err := rt.NewTypedArray(samples, quickjs.ShareMemory)
//
// A program built with an older Go has the function, which this calls.
func (r *Runtime) NewTypedArray[T TypedArrayElement](s []T, mode BufferMode) (Value, error) {
	return NewTypedArray(r, s, mode)
}
