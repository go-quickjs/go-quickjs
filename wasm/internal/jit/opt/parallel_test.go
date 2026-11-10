//go:build amd64 || arm64

package opt

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// TestParallelV makes random parallel moves of v128s and words between
// registers and slots, whose slots meet in part -- a v128's two slots one
// of another's, or a word's -- on a machine of its own, and checks that
// every destination gets what its source held before and nothing else but
// the scratch registers changes.
func TestParallelV(t *testing.T) {
	rnd := rand.New(rand.NewPCG(21, 22))
	const nslots, nregs = 12, 6
	scratch := []loc{{kind: lXMM, r: 15}, {kind: lXMM, r: 14}, {kind: lGPR, r: 10}}
	for iter := range 200000 {
		// The machine: words of slots, general registers, and float
		// registers of two words.
		var slots [nslots]uint64
		var gprs [16]uint64
		var xmms [16][2]uint64
		for i := range slots {
			slots[i] = rnd.Uint64()
		}
		for i := range gprs {
			gprs[i] = rnd.Uint64()
			xmms[i] = [2]uint64{rnd.Uint64(), rnd.Uint64()}
		}
		read := func(l loc, ty Type) [2]uint64 {
			switch l.kind {
			case lGPR:
				return [2]uint64{gprs[l.r], 0}
			case lXMM:
				if ty == TV128 {
					return xmms[l.r]
				}
				return [2]uint64{xmms[l.r][0], 0}
			case lSlot:
				if ty == TV128 {
					return [2]uint64{slots[l.slot], slots[l.slot+1]}
				}
				return [2]uint64{slots[l.slot], 0}
			}
			if ty == TV128 {
				return [2]uint64{l.c, l.hi}
			}
			return [2]uint64{l.c, 0}
		}
		write := func(l loc, ty Type, w [2]uint64) {
			switch l.kind {
			case lGPR:
				gprs[l.r] = w[0]
			case lXMM:
				xmms[l.r] = w
			case lSlot:
				slots[l.slot] = w[0]
				if ty == TV128 {
					slots[l.slot+1] = w[1]
				}
			}
		}
		move := func(dst, src loc, ty Type) { write(dst, ty, read(src, ty)) }

		// Locations of a type apart from those taken.
		var taken []struct {
			l  loc
			ty Type
		}
		free := func(l loc, ty Type) bool {
			if l.kind == lSlot && l.slot+ty.Slots() > nslots {
				return false
			}
			for _, x := range taken {
				if meets(x.l, x.ty, l, ty) {
					return false
				}
			}
			return true
		}
		pick := func(ty Type, allowConst bool) (loc, bool) {
			for range 20 {
				var l loc
				switch k := rnd.IntN(10); {
				case k < 5:
					l = loc{kind: lSlot, slot: int32(rnd.IntN(nslots))}
				case k < 7 && ty != TV128:
					l = loc{kind: lGPR, r: rnd.IntN(nregs)}
				case k < 9:
					l = loc{kind: lXMM, r: rnd.IntN(nregs)}
				case allowConst:
					return loc{kind: lConst, c: rnd.Uint64(), hi: rnd.Uint64()}, true
				default:
					continue
				}
				if free(l, ty) {
					return l, true
				}
			}
			return loc{}, false
		}
		// Sources first, apart or shared; then destinations apart.
		type src struct {
			l  loc
			ty Type
		}
		var srcs []src
		for range 1 + rnd.IntN(5) {
			ty := TI64
			if rnd.IntN(2) == 0 {
				ty = TV128
			}
			if l, ok := pick(ty, true); ok {
				srcs = append(srcs, src{l, ty})
				if l.kind != lConst {
					taken = append(taken, struct {
						l  loc
						ty Type
					}{l, ty})
				}
			}
		}
		taken = taken[:0]
		var moves []pmove
		for range 1 + rnd.IntN(6) {
			if len(srcs) == 0 {
				break
			}
			s := srcs[rnd.IntN(len(srcs))]
			if d, ok := pick(s.ty, false); ok {
				moves = append(moves, pmove{dst: d, src: s.l, t: s.ty})
				taken = append(taken, struct {
					l  loc
					ty Type
				}{d, s.ty})
			}
		}
		if len(moves) == 0 {
			continue
		}
		want := make([][2]uint64, len(moves))
		for i, m := range moves {
			want[i] = read(m.src, m.t)
		}
		beforeSlots, beforeG, beforeX := slots, gprs, xmms
		desc := fmt.Sprint(moves)
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("iteration %d: %v: %s", iter, p, desc)
				}
			}()
			parallelV(moves, nil, move, scratch)
		}()
		for i, m := range moves {
			if got := read(m.dst, m.t); got != want[i] {
				t.Fatalf("iteration %d: %s: %v holds %x, want %x", iter, desc, m.dst, got, want[i])
			}
		}
		// Nothing else changed but the scratch registers.
		isDst := func(l loc, ty Type) bool {
			for _, m := range moves {
				if meets(m.dst, m.t, l, ty) {
					return true
				}
			}
			return false
		}
		for i := range slots {
			if slots[i] != beforeSlots[i] && !isDst(loc{kind: lSlot, slot: int32(i)}, TI64) {
				t.Fatalf("iteration %d: %s: slot %d changed", iter, desc, i)
			}
		}
		for i := range nregs {
			if gprs[i] != beforeG[i] && !isDst(loc{kind: lGPR, r: i}, TI64) {
				t.Fatalf("iteration %d: %s: general register %d changed", iter, desc, i)
			}
			if xmms[i] != beforeX[i] && !isDst(loc{kind: lXMM, r: i}, TI64) {
				t.Fatalf("iteration %d: %s: float register %d changed", iter, desc, i)
			}
		}
	}
}
