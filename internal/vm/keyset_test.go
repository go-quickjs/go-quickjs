package vm

import "testing"

// TestKeySetStrides checks keyAtoms' slots: eight strings allocated one
// after another, at the distance of each of the allocator's size classes
// up to 2 KiB or of any multiple of 16 bytes up to it, wherever the first
// one is, put no more of them in one set than its four ways hold. The
// address itself multiplied had put five of strings 144 bytes apart in
// one set: a site storing five keys in turn had evicted one at every
// conversion, and native code had found none of them.
func TestKeySetStrides(t *testing.T) {
	strides := []uint64{8, 24}
	for s := uint64(16); s <= 2048; s += 16 {
		strides = append(strides, s)
	}
	// Bases spread over a heap's addresses, from a fixed generator.
	x := uint64(0x2545F4914F6CDD1D)
	var bases [16]uint64
	for i := range bases {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		bases[i] = (0xc000000000 + x%0x7e0000000000) &^ 15
	}
	for _, s := range strides {
		for _, b := range bases {
			var n [len(Runtime{}.keyAtoms) / keyWays]int
			for j := range uint64(8) {
				set := keySetOf(b+j*s) >> keyWaysLog
				if n[set]++; n[set] > keyWays {
					t.Fatalf("strings %d bytes apart from %#x: %d of 8 in set %d of %d ways", s, b, n[set], set, keyWays)
				}
			}
		}
	}
}
