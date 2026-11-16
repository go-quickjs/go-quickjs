package ssa

// defTable is the builder's definitions (Braun et al.'s currentDef): each
// block's value of each slot it wrote or found, by block ID and slot, in
// a table of what there is. A dense list of every slot for each block that
// had any had made a function of n slots and m blocks take n·m words,
// most of them nil.
type defTable struct {
	keys  []uint64
	vals  []*Value
	count int
	shift uint
	// spareKeys and spareVals are the tables the last growth left, which
	// the next one takes where they have the room: kept from build to
	// build, a build's growths allocate nothing once the builds before
	// have grown them as far.
	spareKeys []uint64
	spareVals []*Value
}

// defKey is the key of block id's slot.
func defKey(id, slot int) uint64 { return uint64(uint32(id)+1)<<32 | uint64(uint32(slot)) }

// reset empties t, keeping room for some n definitions, its tables from
// keys and vals, which it may take as they are.
func (t *defTable) reset(keys []uint64, vals []*Value, spareKeys []uint64, spareVals []*Value, n int) {
	t.spareKeys, t.spareVals = spareKeys, spareVals
	size := 64
	for size < 2*n {
		size <<= 1
	}
	if cap(keys) >= size && cap(vals) >= size {
		keys, vals = keys[:size], vals[:size]
		clear(keys)
		clear(vals)
	} else {
		keys, vals = make([]uint64, size), make([]*Value, size)
	}
	t.keys, t.vals, t.count = keys, vals, 0
	t.shift = uint(64 - defLog2(size))
}

func defLog2(n int) int {
	k := 0
	for 1<<k < n {
		k++
	}
	return k
}

// at is where key k is, or the empty entry it would be put in.
func (t *defTable) at(k uint64) int {
	mask := len(t.keys) - 1
	i := int((k * 0x9E3779B97F4A7C15) >> t.shift)
	for t.keys[i] != k && t.keys[i] != 0 {
		i = (i + 1) & mask
	}
	return i
}

// get is block id's value of slot, or nil.
func (t *defTable) get(id, slot int) *Value {
	return t.vals[t.at(defKey(id, slot))]
}

// put has block id's slot hold v.
func (t *defTable) put(id, slot int, v *Value) {
	k := defKey(id, slot)
	i := t.at(k)
	if t.keys[i] == 0 {
		if 2*(t.count+1) > len(t.keys) {
			t.grow()
			i = t.at(k)
		}
		t.keys[i] = k
		t.count++
	}
	t.vals[i] = v
}

func (t *defTable) grow() {
	keys, vals := t.keys, t.vals
	if n := 2 * len(keys); cap(t.spareKeys) >= n && cap(t.spareVals) >= n {
		t.keys, t.vals = t.spareKeys[:n], t.spareVals[:n]
		clear(t.keys)
		clear(t.vals)
	} else {
		t.keys, t.vals = make([]uint64, n), make([]*Value, n)
	}
	t.shift--
	for i, k := range keys {
		if k != 0 {
			j := t.at(k)
			t.keys[j], t.vals[j] = k, vals[i]
		}
	}
	clear(vals)
	t.spareKeys, t.spareVals = keys, vals
}
