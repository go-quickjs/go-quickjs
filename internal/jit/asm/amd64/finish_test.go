package amd64

import (
	"encoding/binary"
	"math/rand/v2"
	"testing"
)

// Finish over random code -- runs of bytes, jumps and conditional jumps
// forward and back, RIP-relative loads of labels of three to five bytes
// before their displacements, alignments, labels bound anywhere between,
// several at one place -- leaves every byte that is not a fixup's as it
// was, in order; every branch and every load reaches where its label went,
// which Offset gives, as Moved does a position taken before; every aligned
// label is aligned; and a jump shortened is two bytes. Distances are drawn
// around a byte's displacement, so that jumps sit at its bounds, and
// shortened jumps bring others into reach.
func TestFinishRandomCode(t *testing.T) {
	type item struct {
		kind  int // 0 bytes, 1 jmp, 2 jcc, 3 lea, 4 load of a double, 5 align, 6 bind, 7 a position
		n     int
		label Label
		cc    Cond
		at    int // a position's Len
	}
	for seed := range uint64(3000) {
		rng := rand.New(rand.NewPCG(seed, 7))
		var a Asm
		labels := make([]Label, 1+rng.IntN(12))
		for i := range labels {
			labels[i] = a.NewLabel()
		}
		bound := make([]bool, len(labels))
		var items []item
		fill := byte(0)
		emit := func(n int) {
			for range n {
				fill = fill%0x3F + 1 // never a NOP's, a jump's or a prefix's byte
				a.buf = append(a.buf, 0x40|fill)
			}
		}
		steps := 4 + rng.IntN(60)
		for range steps {
			switch k := rng.IntN(20); {
			case k < 5:
				n := rng.IntN(24)
				if rng.IntN(6) == 0 {
					n = 100 + rng.IntN(60)
				}
				items = append(items, item{kind: 0, n: n})
			case k < 9:
				items = append(items, item{kind: 1, label: labels[rng.IntN(len(labels))]})
			case k < 13:
				items = append(items, item{kind: 2, label: labels[rng.IntN(len(labels))], cc: Cond(rng.IntN(16))})
			case k < 14:
				items = append(items, item{kind: 3, label: labels[rng.IntN(len(labels))]})
			case k < 15:
				items = append(items, item{kind: 4, label: labels[rng.IntN(len(labels))], n: rng.IntN(2) * 8})
			case k < 16:
				items = append(items, item{kind: 5, n: 1 << (1 + rng.IntN(6))})
			case k < 19:
				items = append(items, item{kind: 6, label: labels[rng.IntN(len(labels))]})
			default:
				items = append(items, item{kind: 7})
			}
		}
		// Every label bound once, those not bound yet at the end.
		for i, it := range items {
			if it.kind == 6 {
				if bound[it.label] {
					items[i].kind = 7
				}
				bound[it.label] = true
			}
		}
		for i := range labels {
			if !bound[i] {
				items = append(items, item{kind: 6, label: labels[i]})
			}
		}
		// Emit, noting each byte run's original bytes.
		var runs [][]byte
		for i := range items {
			it := &items[i]
			switch it.kind {
			case 0:
				start := len(a.buf)
				emit(it.n)
				runs = append(runs, append([]byte(nil), a.buf[start:]...))
			case 1:
				a.Jmp(it.label)
			case 2:
				a.Jcc(it.cc, it.label)
			case 3:
				a.LeaLabel(RAX, it.label)
			case 4:
				a.LoadSDLabel(XReg(it.n), it.label)
			case 5:
				a.Align(it.n)
			case 6:
				a.Bind(it.label)
			case 7:
				it.at = a.Len()
			}
		}
		code, err := a.Finish()
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		// Walk the code as it was emitted.
		type branch struct {
			to    int
			label Label
		}
		var branches []branch
		pos, run := 0, 0
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("seed %d at %d: "+format, append([]any{seed, pos}, args...)...)
		}
		for _, it := range items {
			switch it.kind {
			case 0:
				want := runs[run]
				run++
				if pos+len(want) > len(code) || string(code[pos:pos+len(want)]) != string(want) {
					fail("a run of %d bytes is not where it was", len(want))
				}
				pos += len(want)
			case 1, 2:
				b := code[pos]
				switch {
				case it.kind == 1 && b == 0xEB, it.kind == 2 && b == 0x70|byte(it.cc):
					branches = append(branches, branch{pos + 2 + int(int8(code[pos+1])), it.label})
					pos += 2
				case it.kind == 1 && b == 0xE9:
					branches = append(branches, branch{pos + 5 + int(int32(binary.LittleEndian.Uint32(code[pos+1:]))), it.label})
					pos += 5
				case it.kind == 2 && b == 0x0F && code[pos+1] == 0x80|byte(it.cc):
					branches = append(branches, branch{pos + 6 + int(int32(binary.LittleEndian.Uint32(code[pos+2:]))), it.label})
					pos += 6
				default:
					fail("no jump of kind %d (cc %x) but % x", it.kind, it.cc, code[pos:min(pos+6, len(code))])
				}
			case 3, 4:
				n := 3
				if it.kind == 4 {
					n = 4 + it.n/8
				}
				branches = append(branches, branch{pos + n + 4 + int(int32(binary.LittleEndian.Uint32(code[pos+n:]))), it.label})
				pos += n + 4
			case 5:
				k := (it.n - pos%it.n) % it.n
				pos += k
			case 6:
				if a.Offset(it.label) != pos {
					fail("label %d is at %d", it.label, a.Offset(it.label))
				}
			case 7:
				if a.Moved(it.at) != pos {
					fail("position %d moved to %d", it.at, a.Moved(it.at))
				}
			}
		}
		if pos != len(code) {
			t.Fatalf("seed %d: the code is %d bytes, its items %d", seed, len(code), pos)
		}
		for i, b := range branches {
			if b.to != a.Offset(b.label) {
				t.Fatalf("seed %d: branch %d reaches %d, its label is at %d", seed, i, b.to, a.Offset(b.label))
			}
		}
	}
}
