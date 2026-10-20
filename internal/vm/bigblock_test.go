package vm

import (
	"math/big"
	"runtime"
	"sync/atomic"
	"testing"
)

// TestBigIntBlocks pins BigInts as one allocation of no pointer -- a slice
// of words, which the collector never scans -- a header and their digits:
// of each size from none to 40 words, of both signs, each made from a
// big.Int, read back as one (Cmp, String, Text), and as a Value (Big),
// while the collector runs, the blocks garbage in between;
// zero's digits are no slice, which would point past its block.
func TestBigIntBlocks(t *testing.T) {
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			runtime.GC()
		}
	}()
	defer func() { stop.Store(true); <-done }()
	var kept []*BigInt
	var wants []*big.Int
	for round := range 50 {
		for n := 0; n <= 40; n++ {
			for _, neg := range []bool{false, true} {
				want := new(big.Int)
				for i := range n {
					want.Lsh(want, 64)
					want.Or(want, new(big.Int).SetUint64(uint64(round*7919+n*104729+i*31+1)|1<<63))
				}
				if neg {
					want.Neg(want)
				}
				b := blockOf(want)
				// Garbage between the blocks kept.
				_ = make([]byte, 64+n*8)
				if b.digits() != len(want.Bits()) || b.negative() != (want.Sign() < 0) {
					t.Fatalf("%v: %d digits, negative %v", want, b.digits(), b.negative())
				}
				if n == 0 && b.words() != nil {
					t.Fatalf("zero's digits are a slice")
				}
				if bigIntOf(b).Cmp(want) != 0 || b.String() != want.String() || b.Text(16) != want.Text(16) {
					t.Fatalf("%v read back as %v", want, bigIntOf(b))
				}
				v := valueOf(want)
				// Held in its Value below 2^64, in either sign.
				if got := bigIntOf(v.BigInt()); got.Cmp(want) != 0 || v.isShortBig() != (want.BitLen() <= 64) || v.isInt64Big() != want.IsInt64() {
					t.Fatalf("%v as a Value: %v, short %v", want, got, v.isShortBig())
				}
				if round%10 == 0 {
					kept, wants = append(kept, b), append(wants, want)
				}
			}
		}
	}
	runtime.GC()
	for i, b := range kept {
		if b.Cmp(blockOf(wants[i])) != 0 {
			t.Fatalf("kept %v is now %v", wants[i], bigIntOf(b))
		}
	}
}
