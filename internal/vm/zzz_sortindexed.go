package vm

// The index keys a sparse array or an ordinary object keeps in its table,
// sorted by index: ownKeys lists them in order, among the dense elements,
// and shrinkArray deletes them from the highest down. They are in the
// table's order, the order they were added in, which for an array filled
// from its end down is the reverse of theirs: the insertion sort they had
// been sorted with took the square of their number, 3.2 s for Object.keys
// of 50,000 of them and a length set below them. Keys in order, either way
// round, take one pass now, and any others a heapsort's n log n.
//
// It is a heapsort of its own rather than slices.SortFunc, whose pdqsort's
// instantiations for the key type are 6 KB laid out among the package's
// generics, ahead of the tree tier's literal node (litNodeOf.make):
// escape_rare was 16% slower at every placement with them there.
//
// Its functions are generic, instantiated with struct{} alone: Go lays a
// generic function's instantiations out after every function and closure of
// the package (see zzz_direct.go); sortIndexedAtoms, which ownKeys and
// shrinkArray inline, calls it.

// sortIndexed sorts a by index. No two keys have the same index.
//
//go:noinline
func sortIndexed[_ any](a []indexedAtom) {
	n := len(a)
	up, down := true, true
	for i := 1; i < n; i++ {
		if a[i-1].index < a[i].index {
			down = false
		} else {
			up = false
		}
	}
	switch {
	case up:
	case down:
		for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
			a[i], a[j] = a[j], a[i]
		}
	default:
		for i := n/2 - 1; i >= 0; i-- {
			siftIndexed[struct{}](a, i, n)
		}
		for end := n - 1; end > 0; end-- {
			a[0], a[end] = a[end], a[0]
			siftIndexed[struct{}](a, 0, end)
		}
	}
}

// siftIndexed moves a[i] down the max-heap a[:n] to its place.
func siftIndexed[_ any](a []indexedAtom, i, n int) {
	for {
		c := 2*i + 1
		if c >= n {
			return
		}
		if c+1 < n && a[c+1].index > a[c].index {
			c++
		}
		if a[i].index >= a[c].index {
			return
		}
		a[i], a[c] = a[c], a[i]
		i = c
	}
}
