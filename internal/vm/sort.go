package vm

// stableSort sorts values by less, keeping equal ones in the order they came
// in, as Array.prototype.sort must, and stops at the first error less reports.
//
// It is a merge sort. The comparison is what a sort costs here -- a
// comparator is a call into the script -- so it is the comparisons it saves
// on: a short run is sorted by binary insertion, which asks the fewest, and
// two runs already in order are not merged at all, so that sorted input costs
// a comparison per run. It reorders values in place with one buffer, where
// the library's stable sort swaps through reflection and makes more
// comparisons than this.
//
// A comparator that is not consistent -- one answering at random -- still
// leaves a permutation of the values, which is all the specification asks.
func stableSort(vals []Value, less func(a, b Value) (bool, error)) error {
	if len(vals) < 2 {
		return nil
	}
	s := sorter{vals: vals, less: less}
	s.buf = make([]Value, len(vals)/2+1)
	s.sort(0, len(vals))
	clear(s.buf)
	return s.err
}

type sorter struct {
	vals []Value
	buf  []Value
	less func(a, b Value) (bool, error)
	err  error
}

// lessThan is less, which after an error answers false without asking, so
// that a failed sort ends quickly with what it had.
func (s *sorter) lessThan(a, b Value) bool {
	if s.err != nil {
		return false
	}
	lt, err := s.less(a, b)
	if err != nil {
		s.err = err
		return false
	}
	return lt
}

// insertionRun is how short a run is sorted by insertion rather than split.
const insertionRun = 12

func (s *sorter) sort(lo, hi int) {
	if hi-lo <= insertionRun {
		s.insertion(lo, hi)
		return
	}
	mid := int(uint(lo+hi) / 2)
	s.sort(lo, mid)
	s.sort(mid, hi)
	s.merge(lo, mid, hi)
}

// insertion sorts vals[lo:hi] by binary insertion: each value goes after
// every one it is not less than, which keeps equal values in order.
func (s *sorter) insertion(lo, hi int) {
	v := s.vals
	for i := lo + 1; i < hi && s.err == nil; i++ {
		x := v[i]
		if !s.lessThan(x, v[i-1]) {
			// Where it is: a run already in order costs a comparison a value.
			continue
		}
		a, b := lo, i-1
		for a < b {
			m := int(uint(a+b) / 2)
			if s.lessThan(x, v[m]) {
				b = m
			} else {
				a = m + 1
			}
		}
		copy(v[a+1:i+1], v[a:i])
		v[a] = x
	}
}

// merge merges the sorted runs vals[lo:mid] and vals[mid:hi].
func (s *sorter) merge(lo, mid, hi int) {
	v := s.vals
	if s.err != nil || !s.lessThan(v[mid], v[mid-1]) {
		// Already in order, or failed.
		return
	}
	left := s.buf[:mid-lo]
	copy(left, v[lo:mid])
	i, j, k := 0, mid, lo
	for i < len(left) && j < hi {
		// The right one goes first only when it is less, so that equal
		// values keep their order.
		if s.lessThan(v[j], left[i]) {
			v[k] = v[j]
			j++
		} else {
			v[k] = left[i]
			i++
		}
		k++
	}
	copy(v[k:], left[i:])
}
