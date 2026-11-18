package vm

// A builder asks as it goes whether what it has written is longer than a
// string may be (partsBuilder.check), but what is written after its last
// check -- a join's separators after its last elements, holes, a replace's
// last replacement and the tail after it -- is seen first by these, where
// the builder's string is made: the RangeError V8 throws, rather than a
// string longer than any may be.
//
// Generic, instantiated with struct{}, so that they are laid out after every
// function and closure of the package; their callers keep their sizes, so
// that nothing before them moves (AGENTS.md).

// asciiParts is the String of t, made of ASCII alone and as long as its
// bytes, or the RangeError for one longer than a string may be.
func asciiParts[_ any](r *Runtime, t string) (Value, error) {
	if len(t) > maxStringLength {
		return Undefined, r.throwStringLength()
	}
	return Str(&String{s: t, length: len(t), ascii: true}), nil
}

// stringFits is the RangeError for a string of n code units, longer than a
// string may be, or nil.
func stringFits[_ any](r *Runtime, n int) error {
	if n > maxStringLength {
		return r.throwStringLength()
	}
	return nil
}
