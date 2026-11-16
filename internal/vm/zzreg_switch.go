package vm

import (
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regSwitchTable is a switch instruction's table, as the tree tier's
// switchLookup makes one of the same cases: a number by an array's index
// where the cases are integers of a small range, by a short list or a map
// otherwise; a string by its code units, past the longest case's length
// not even flattened; true, false and null by their kind. Each selector's
// first case is the one it goes to, as the chain of === would.
type regSwitchTable struct {
	dense  []int32
	lo, hi float64
	few    []float64
	fewTo  []int32
	nums   map[float64]int32
	// byLen is where flat's strings of each length start, as the tree's
	// switchLookup groups them.
	byLen []int32
	flat  []bytecode.SwitchCase
	other [3]int32
	// typeTo is the body of each type's name in typeofNames, for
	// switch_typeof.
	typeTo [8]int32
}

// newRegSwitchTable is the table of cases.
func newRegSwitchTable(cases []bytecode.SwitchCase) *regSwitchTable {
	t := &regSwitchTable{other: [3]int32{-1, -1, -1}, lo: math.Inf(1), hi: math.Inf(-1)}
	for i := range t.typeTo {
		t.typeTo[i] = -1
	}
	nums := map[float64]int32{}
	var order []float64
	seen := map[string]bool{}
	var strs []bytecode.SwitchCase
	ints := true
	for _, c := range cases {
		switch c.Kind {
		case bytecode.SwitchNum:
			f := c.Num
			if f != f {
				continue // NaN equals nothing.
			}
			if _, ok := nums[f]; !ok {
				nums[f] = int32(c.To)
				order = append(order, f)
				t.lo, t.hi = min(t.lo, f), max(t.hi, f)
				ints = ints && f == math.Trunc(f) && !math.IsInf(f, 0)
			}
		case bytecode.SwitchStr:
			if !seen[c.Str] {
				seen[c.Str] = true
				strs = append(strs, c)
				for i, name := range typeofNames {
					if name == c.Str {
						t.typeTo[i] = int32(c.To)
					}
				}
			}
		default:
			if i := c.Kind - bytecode.SwitchTrue; t.other[i] < 0 {
				t.other[i] = int32(c.To)
			}
		}
	}
	if len(strs) > 0 {
		most := 0
		lens := make([]int, len(strs))
		for i, s := range strs {
			lens[i] = NewString(s.Str).length
			most = max(most, lens[i])
		}
		t.byLen = make([]int32, most+2)
		for n := 0; n <= most; n++ {
			t.byLen[n] = int32(len(t.flat))
			for i, s := range strs {
				if lens[i] == n {
					t.flat = append(t.flat, s)
				}
			}
		}
		t.byLen[most+1] = int32(len(t.flat))
	}
	switch {
	case len(nums) == 0:
	case ints && t.hi-t.lo < switchDense:
		t.dense = make([]int32, int(t.hi-t.lo)+1)
		for i := range t.dense {
			t.dense[i] = -1
		}
		for f, to := range nums {
			t.dense[int(f-t.lo)] = to
		}
	case len(nums) <= 8:
		for _, f := range order {
			t.few, t.fewTo = append(t.few, f), append(t.fewTo, nums[f])
		}
	default:
		t.nums = nums
	}
	return t
}

// lookup is the pc of the body v goes to, or -1: a string the constant a
// case was made from first by its pointer, as the tree's lookup asks.
func (t *regSwitchTable) lookup(v Value, consts []Value) int32 {
	switch {
	case v.IsNumber():
		f := v.num
		if t.dense != nil {
			if f >= t.lo && f <= t.hi {
				if i := int(f - t.lo); float64(i)+t.lo == f {
					return t.dense[i]
				}
			}
			return -1
		}
		for i, g := range t.few {
			if f == g {
				return t.fewTo[i]
			}
		}
		if t.nums != nil {
			if to, ok := t.nums[f]; ok {
				return to
			}
		}
	case v.IsString():
		s := (*String)(v.ref)
		if s.length >= len(t.byLen)-1 {
			return -1
		}
		for _, e := range t.flat[t.byLen[s.length]:t.byLen[s.length+1]] {
			if int(e.K) < len(consts) && consts[e.K].ref == unsafe.Pointer(s) {
				return int32(e.To)
			}
			if s.left != nil {
				s.Go()
			}
			if s.s == e.Str {
				return int32(e.To)
			}
		}
	case v.IsBool():
		if v.Truthy() {
			return t.other[0]
		}
		return t.other[1]
	case v.IsNull():
		return t.other[2]
	}
	return -1
}
