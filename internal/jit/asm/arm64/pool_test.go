package arm64

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"slices"
	"testing"
)

// poolBranch is a branch a test made: where it began, where the caller's
// next instruction went, its label and what it was.
type poolBranch struct {
	at, next int
	label    Label
	word     uint32 // the branch as made, its offset 0
}

// follow is where code goes from pc, the branch br's condition holding or
// not: through every jump, conditional branch, ADR, subtraction and BR the
// assembler made for it, to the first instruction that is the branch's
// target or the caller's next (stop), or -1 and why. x is the registers
// the ADRs and subtractions wrote.
func follow(code []uint32, pc int, br poolBranch, holds bool, stop func(int) bool, x *[32]int64) (int, int, string) {
	sext := func(v uint32, bits uint) int { return int(int32(v<<(32-bits)) >> (32 - bits)) }
	for step := 0; step < 10000; step++ {
		if pc < 0 || pc >= len(code) {
			return -1, step, fmt.Sprintf("left the code at %d", pc)
		}
		if step > 0 && stop(pc) {
			return pc, step, ""
		}
		w := code[pc]
		switch {
		case w&0xFC000000 == 0x14000000:
			pc += sext(w&(1<<26-1), 26)
		case w&0xFF000010 == 0x54000000:
			// The branch's own condition, or its opposite.
			taken := holds
			if w&0xF != br.word&0xF {
				taken = !holds
			}
			if taken {
				pc += sext(w>>5&(1<<19-1), 19)
			} else {
				pc++
			}
		case w&0x7E000000 == 0x34000000:
			// The register is zero if a CBZ's condition holds, or a
			// CBNZ's does not.
			zero := holds == (br.word&(1<<24) == 0)
			if zero == (w&(1<<24) == 0) {
				pc += sext(w>>5&(1<<19-1), 19)
			} else {
				pc++
			}
		case w&0x9F000000 == 0x10000000:
			x[w&31] = int64(4*pc + sext(w>>5&(1<<19-1), 19)*4 + int(w>>29&3))
			pc++
		case w&0xFFC00000 == 0xD1400000:
			x[w&31] = x[w>>5&31] - int64(w>>10&0xFFF)<<12
			pc++
		case w&0xFFFFFC1F == 0xD61F0000:
			at := x[w>>5&31]
			if at%4 != 0 {
				return -1, step, fmt.Sprintf("BR to %d", at)
			}
			pc = int(at / 4)
		default:
			return -1, step, fmt.Sprintf("ran into %#08x at %d", w, pc)
		}
	}
	return -1, 0, "looped"
}

// poolProgram is code of n instructions, plain ones and, at random, a
// branch of every kind to a label bound behind or still to be bound, the
// labels bound at random, some data among them, an AdrJump and a BR
// through it, and an Adr of a table, old or new; each branch goes where
// its label is when taken, and where the caller's next instruction is
// when not, whatever pools came between, and each Adr of a table is its
// address, where its words are.
func poolProgram(t *testing.T, seed int64, n, every int, narrow uint) {
	rng := rand.New(rand.NewSource(seed))
	a := Asm{narrow: narrow}
	labels := make([]Label, 64)
	for i := range labels {
		labels[i] = a.NewLabel()
	}
	var brs, adrs []poolBranch
	var tables []Label
	table := func(l Label) []uint32 { return []uint32{0xDA7A0000 | uint32(l), uint32(l) * 7, 0} }
	for a.Len()/4 < n {
		switch r := rng.Intn(every); {
		case r == 3:
			l := labels[rng.Intn(len(labels))]
			at := a.Len() / 4
			a.AdrJump(3, l)
			a.Br(3)
			brs = append(brs, poolBranch{at, a.Len() / 4, l, 0x10000003})
		case r == 4:
			if len(tables) == 0 || rng.Intn(4) == 0 {
				var l Label
				l = a.Table(make([]byte, 12))
				b := a.tables[l]
				for i, w := range table(l) {
					binary.LittleEndian.PutUint32(b[4*i:], w)
				}
				tables = append(tables, l)
			}
			l := tables[rng.Intn(len(tables))]
			at := a.Len() / 4
			a.Adr(3, l)
			adrs = append(adrs, poolBranch{at, a.Len() / 4, l, 0x10000003})
		case r == 0:
			l := labels[rng.Intn(len(labels))]
			at := a.Len() / 4
			var word uint32
			switch rng.Intn(4) {
			case 0:
				a.B(l)
				word = 0x14000000
			case 1:
				c := Cond(rng.Intn(14))
				a.BCond(c, l)
				word = 0x54000000 | uint32(c)
			case 2:
				a.Cbz(3, l, true)
				word = 0x34000000 | sf(true) | 3
			default:
				a.Cbnz(3, l, false)
				word = 0x35000000 | 3
			}
			brs = append(brs, poolBranch{at, a.Len() / 4, l, word})
		case r == 1:
			// A label bound here, and a new one in its place.
			k := rng.Intn(len(labels))
			if a.labels[labels[k]] < 0 {
				a.Bind(labels[k])
			}
			labels[k] = a.NewLabel()
		case r == 2:
			a.Data(make([]byte, 4*rng.Intn(64)))
		default:
			a.MovRR(4, 5)
		}
	}
	for _, l := range labels {
		if a.labels[l] < 0 {
			a.Bind(l)
		}
	}
	a.MovRR(4, 5)
	end := a.Len() / 4
	a.Tables()
	b, err := a.Finish()
	if err != nil {
		t.Fatal(err)
	}
	code := make([]uint32, len(b)/4)
	for i := range code {
		code[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	hops, pooled, built := 0, 0, 0
	for _, br := range brs {
		to := a.labels[br.label]
		var x [32]int64
		got, steps, why := follow(code, br.at, br, true, func(pc int) bool { return pc == to }, &x)
		if steps > 1 {
			hops++
		}
		if got != to {
			t.Fatalf("branch at %d to %d (%#08x): taken, %d: %s", br.at, to, br.word, got, why)
		}
		if br.word&0xFC000000 == 0x14000000 || br.word&0x9F000000 == 0x10000000 {
			continue
		}
		got, _, why = follow(code, br.at, br, false, func(pc int) bool { return pc == br.next }, &x)
		if got != br.next {
			t.Fatalf("branch at %d to %d (%#08x): not taken, %d, want %d: %s", br.at, to, br.word, got, br.next, why)
		}
	}
	for _, adr := range adrs {
		to := a.labels[adr.label]
		var x [32]int64
		got, steps, why := follow(code, adr.at, adr, true, func(pc int) bool { return pc == adr.next }, &x)
		if got != adr.next {
			t.Fatalf("adr at %d of table %d: %d, want %d: %s", adr.at, to, got, adr.next, why)
		}
		if x[3] != int64(4*to) || !slices.Equal(code[to:to+3], table(adr.label)) {
			t.Fatalf("adr at %d: %d, want table %d (%#x)", adr.at, x[3], 4*to, code[to:to+3])
		}
		if to < end {
			pooled++
		}
		if code[adr.at+1]&0xFFC00000 == 0xD1400000 && steps > 1 {
			built++
		}
	}
	if hops == 0 || pooled == 0 || built == 0 {
		t.Fatalf("%d branches through others, %d addresses of tables in pools, %d built", hops, pooled, built)
	}
	t.Logf("%d instructions, %d branches, %d through others, %d table addresses, %d in pools, %d built", n, len(brs), hops, len(adrs), pooled, built)
}

// Code longer than every reach: each branch, forward and back, of every
// kind, goes where it should -- through veneers, veneers of veneers, the
// opposite condition over a B, and a jump through the temporary.
func TestPoolsNarrowed(t *testing.T) {
	// The reaches 8 bits narrower than the instructions': pools, veneers
	// of veneers and jumps back past a B's reach, in code of a few
	// megabytes.
	for seed := range int64(4) {
		poolProgram(t, seed, 3<<20, 50, 8)
	}
}

// At the instructions' own reaches: 12 MB of code with conditional
// branches across all of it.
func TestPoolsLong(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	poolProgram(t, 9, 3<<20, 2000, 0)
}

// Code shorter than a quarter of a conditional branch's reach has no
// pool: it is as it was without them.
func TestNoPoolInShortCode(t *testing.T) {
	var a Asm
	end := a.NewLabel()
	n := a.nearVeneer() - 16
	for range n {
		a.BCond(NE, end)
	}
	a.Bind(end)
	b, err := a.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 4*n {
		t.Fatalf("%d words, want %d", len(b)/4, n)
	}
	for i := range n {
		if w := binary.LittleEndian.Uint32(b[4*i:]); w != 0x54000001|uint32(n-i)<<5 {
			t.Fatalf("word %d: %#08x", i, w)
		}
	}
}

// LoadQTable's loads read their tables' bytes in code longer than a
// literal's reach: each table placed in the pool ahead of its first load,
// or after the code, and loaded again from behind, past the reach through
// the temporary.
func TestLoadQTable(t *testing.T) {
	for _, narrow := range []uint{8, 4, 0} {
		a := Asm{narrow: narrow}
		const tables = 40
		labels := make([]Label, tables)
		want := make([][]byte, tables)
		for i := range labels {
			b := make([]byte, 16)
			binary.LittleEndian.PutUint64(b, uint64(i)*0x0101010101010101)
			binary.LittleEndian.PutUint64(b[8:], ^uint64(i))
			labels[i], want[i] = a.Table(b), b
		}
		type use struct{ at, table int }
		var uses []use
		r := rand.New(rand.NewSource(int64(narrow)))
		total := 3 * a.nearReach()
		for len(a.buf) < total {
			i := r.Intn(tables)
			uses = append(uses, use{len(a.buf), i})
			a.LoadQTable(FReg(r.Intn(32)), labels[i])
			for range r.Intn(a.nearReach() / 8) {
				a.MovRR(X1, X2)
			}
		}
		a.Tables()
		b, err := a.Finish()
		if err != nil {
			t.Fatalf("narrow %d: %v", narrow, err)
		}
		code := make([]uint32, len(b)/4)
		for i := range code {
			code[i] = binary.LittleEndian.Uint32(b[4*i:])
		}
		sext := func(v uint32, bits uint) int { return int(int32(v<<(32-bits)) >> (32 - bits)) }
		far := 0
		for _, u := range uses {
			pc := u.at
			var addr int
			switch w := code[pc]; {
			case w&0xFF000000 == 0x9C000000:
				addr = 4*pc + 4*sext(w>>5&(1<<19-1), 19)
			case w&0x9F00001F == 0x1000001B:
				// ADR of X27, the subtractions, then LDR Q from X27.
				far++
				addr = 4*pc + 4*sext(w>>5&(1<<19-1), 19)
				for pc++; code[pc]&0xFFC003FF == 0xD140037B; pc++ {
					addr -= 4096 * int(code[pc]>>10&0xFFF)
				}
				if code[pc]&0xFFFFFFE0 != 0x3DC00360 {
					t.Fatalf("narrow %d: a far load's word %#08x at %d", narrow, code[pc], pc)
				}
			default:
				t.Fatalf("narrow %d: word %#08x at %d, not a load", narrow, w, pc)
			}
			if addr < 0 || addr+16 > len(b) || !slices.Equal(b[addr:addr+16], want[u.table]) {
				t.Fatalf("narrow %d: the load at %d reads %d, not table %d's bytes", narrow, u.at, addr, u.table)
			}
		}
		if narrow > 0 && far == 0 {
			t.Fatalf("narrow %d: no load from behind past the reach", narrow)
		}
		t.Logf("narrow %d: %d words, %d loads, %d from behind past the reach", narrow, len(code), len(uses), far)
	}
}
