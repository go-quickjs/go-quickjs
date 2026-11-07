package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
)

// nativeRecord is what native compares of a line of v8bench's -jitcode.
type nativeRecord struct {
	Seq          int
	Name, Script string
	Line         int
	Arch         string
	Text         int
	Bytes        []byte
	Marks        json.RawMessage
	Listing      string
	Spills       int
	States       string
}

// compareNative compares two -jitheld runs' files compile by compile, and
// says how many compiles were the same and where the first that was not
// parted. Their code is the same if it is the same but for addresses: the
// words amd64 loads an address from, and the moves arm64 builds one with,
// whose number varies with the address.
func compareNative(a, b string, w io.Writer) (same bool, err error) {
	x, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer x.Close()
	y, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer y.Close()
	dx, dy := json.NewDecoder(bufio.NewReader(x)), json.NewDecoder(bufio.NewReader(y))
	var compiles, exact, addresses, moved, refused, code int
	for {
		var p, q nativeRecord
		ex, ey := dx.Decode(&p), dy.Decode(&q)
		if ex == io.EOF && ey == io.EOF {
			break
		}
		if ex == io.EOF || ey == io.EOF {
			fmt.Fprintf(w, "one run ends at %d compiles: the runs did not compile the same\n", compiles)
			return false, nil
		}
		if ex != nil {
			return false, fmt.Errorf("%s: %w", a, ex)
		}
		if ey != nil {
			return false, fmt.Errorf("%s: %w", b, ey)
		}
		compiles++
		code += len(p.Bytes)
		if len(p.Bytes) == 0 {
			refused++
		}
		if why := differs(&p, &q); why != "" {
			fmt.Fprintf(w, "compile %d, %s line %d of %s %s: %s\n", p.Seq, p.Name, p.Line, p.Script, p.Arch, why)
			fmt.Fprintf(w, "%d compiles the same before it\n", compiles-1)
			return false, nil
		}
		switch {
		case bytes.Equal(p.Bytes, q.Bytes):
			exact++
		case len(p.Bytes) == len(q.Bytes):
			addresses++
		default:
			moved++
		}
	}
	fmt.Fprintf(w, "same code: %d compiles, %d bytes, %d exactly, %d but for addresses, %d but for arm64's length of them, %d with no code\n",
		compiles, code, exact, addresses, moved, refused)
	return true, nil
}

// compareNativeAnyOrder is compareNative for runs that made their compiles
// in different orders, as a program whose work depends on Math.random
// does: the nth compile of a function in one run is compared with the
// nth in the other, and a compile only one run made is counted.
func compareNativeAnyOrder(a, b string, w io.Writer) (same bool, err error) {
	load := func(path string) (map[string][]nativeRecord, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		m := map[string][]nativeRecord{}
		for d := json.NewDecoder(bufio.NewReader(f)); ; {
			var r nativeRecord
			if err := d.Decode(&r); err == io.EOF {
				return m, nil
			} else if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			k := fmt.Sprint(r.Arch, " ", r.Script, " ", r.Line, " ", r.Name)
			m[k] = append(m[k], r)
		}
	}
	x, err := load(a)
	if err != nil {
		return false, err
	}
	y, err := load(b)
	if err != nil {
		return false, err
	}
	var equal, parted, alone int
	for k, l := range x {
		o := y[k]
		alone += max(len(l), len(o)) - min(len(l), len(o))
		for i := range min(len(l), len(o)) {
			if why := differs(&l[i], &o[i]); why != "" {
				parted++
				if parted <= 5 {
					fmt.Fprintf(w, "compile %d of %s: %s\n", i, k, why)
				}
				continue
			}
			equal++
		}
	}
	for k, o := range y {
		if _, ok := x[k]; !ok {
			alone += len(o)
		}
	}
	fmt.Fprintf(w, "%d compiles the same, %d not, %d made by one run alone\n", equal, parted, alone)
	return parted == 0 && alone == 0, nil
}

var hexNumber = regexp.MustCompile(`0x[0-9a-f]{6,}`)

// differs says how two records of one compile differ, or nothing.
func differs(p, q *nativeRecord) string {
	if p.Name != q.Name || p.Line != q.Line || p.Script != q.Script || p.Arch != q.Arch {
		return fmt.Sprintf("the other run compiled %s %s line %d: the runs did not compile the same", q.Arch, q.Name, q.Line)
	}
	if p.Arch == "arm64" {
		// A different length, and every offset the description has, can
		// be the addresses' doing (a64Parse).
		if p.Text == q.Text && p.States != q.States {
			return "what its calls and exits are described by differs"
		}
		if p.Listing != q.Listing {
			return "the error differs"
		}
		return differsARM64(p.Bytes, q.Bytes, p.Text, q.Text)
	}
	switch {
	case p.Text != q.Text:
		return fmt.Sprintf("%d bytes of instructions, against %d", p.Text, q.Text)
	case p.Spills != q.Spills:
		return fmt.Sprintf("%d values spilled, against %d", p.Spills, q.Spills)
	case !bytes.Equal(p.Marks, q.Marks):
		return "the marks differ"
	case p.States != q.States:
		return "what its calls and exits are described by differs"
	}
	if x, y := hexNumber.ReplaceAllString(p.Listing, "0x"), hexNumber.ReplaceAllString(q.Listing, "0x"); x != y {
		return "the SSA listing differs"
	}
	return differsAMD64(p.Bytes, q.Bytes, p.Text)
}

func isAddress(v uint64) bool { return v >= 1<<32 && v < 1<<47 }

// differsAMD64 finds a difference that is not in a movabs's immediate, or a
// word of the code's constants, that may be an address.
func differsAMD64(a, b []byte, text int) string {
	if len(a) != len(b) {
		return fmt.Sprintf("%d bytes, against %d", len(a), len(b))
	}
	word := func(o int) bool {
		if o+8 > len(a) || !isAddress(binary.LittleEndian.Uint64(a[o:])) || !isAddress(binary.LittleEndian.Uint64(b[o:])) {
			return false
		}
		if o >= text {
			return (o-text)%8 == 0
		}
		return o >= 2 && a[o-2]&^1 == 0x48 && a[o-1]&^7 == 0xB8
	}
next:
	for i := 0; i < len(a); {
		if a[i] == b[i] {
			i++
			continue
		}
		for o := max(0, i-7); o <= i; o++ {
			if word(o) {
				i = o + 8
				continue next
			}
		}
		return fmt.Sprintf("byte %d: %x, against %x", i, a[i:min(i+12, len(a))], b[i:min(i+12, len(b))])
	}
	return ""
}

// a64Item is an instruction of arm64's code, with a load of a constant, a
// movz or a movn and the movks after it, one item.
type a64Item struct {
	word  uint32 // the instruction, a branch's offset left out
	imm   uint64 // a load's constant
	load  bool
	refer bool // to is where it refers
	to    int  // a branch's target, by item, or past the code, by offset
}

// a64Parse splits arm64's instructions into items. A load takes as many
// moves as the constant has halfwords that are not zero, so that code
// with an address in it differs in length with the address, and so do the
// branches over it: items are what can be compared.
func a64Parse(code []byte) []a64Item {
	var items []a64Item
	at := make(map[int]int, len(code)/4)
	var target []int // of each item, by byte
	for i := 0; i+4 <= len(code); {
		w := binary.LittleEndian.Uint32(code[i:])
		at[i] = len(items)
		it := a64Item{word: w}
		if op := w >> 23 & 0xFF; op == 0xA5 || op == 0x25 { // movz, movn
			it.load, it.word = true, w&^0x1FFFE0&^(3<<21)
			it.imm = uint64(w>>5&0xFFFF) << (16 * (w >> 21 & 3))
			for i += 4; i+4 <= len(code); i += 4 {
				k := binary.LittleEndian.Uint32(code[i:])
				if k>>23 != 0x1E5 || k&31 != w&31 {
					break
				}
				it.imm |= uint64(k>>5&0xFFFF) << (16 * (k >> 21 & 3))
			}
			items, target = append(items, it), append(target, -1)
			continue
		}
		off, field, ok := a64Branch(w)
		t := -1
		if ok {
			t = i + off
			it.word = w &^ field
			it.refer = true
		}
		items, target = append(items, it), append(target, t)
		i += 4
	}
	for k, t := range target {
		if t >= 0 {
			to, ok := at[t]
			if !ok {
				to = -t
			}
			items[k].to = to
		}
	}
	return items
}

// a64Branch decodes a pc-relative instruction: its offset in bytes, and the
// bits of the word that hold it.
func a64Branch(w uint32) (off int, field uint32, ok bool) {
	sext := func(v uint32, bits int) int { return int(int32(v<<(32-bits)) >> (32 - bits)) }
	switch {
	case w&0x7C000000 == 0x14000000: // b, bl
		return sext(w&0x3FFFFFF, 26) * 4, 0x3FFFFFF, true
	case w&0xFF000010 == 0x54000000, w&0x7E000000 == 0x34000000, w&0x3B000000 == 0x18000000: // b.cond, cbz, cbnz, ldr literal
		return sext(w>>5&0x7FFFF, 19) * 4, 0x7FFFF << 5, true
	case w&0x7E000000 == 0x36000000: // tbz, tbnz
		return sext(w>>5&0x3FFF, 14) * 4, 0x3FFF << 5, true
	case w&0x9F000000 == 0x10000000: // adr
		return sext(w>>5&0x7FFFF, 19)<<2 | int(w>>29&3), 0x60FFFFE0, true
	}
	return 0, 0, false
}

// differsARM64 finds a difference between two arm64 codes that is not in
// a constant both load that may be an address, in how many moves it takes,
// or in the distances that makes.
func differsARM64(a, b []byte, text int, textB int) string {
	x, y := a64Parse(a[:text]), a64Parse(b[:textB])
	if len(x) != len(y) {
		return fmt.Sprintf("%d instructions, against %d", len(x), len(y))
	}
	for i := range x {
		p, q := x[i], y[i]
		if p.word != q.word || p.load != q.load {
			return fmt.Sprintf("item %d: %08x, against %08x", i, p.word, q.word)
		}
		// A target past the code is an offset from the start of the code,
		// so one in the constants moves with the instructions.
		if p.refer && (p.to > 0) == (q.to > 0) && p.to > 0 && p.to != q.to {
			return fmt.Sprintf("item %d: a branch to item %d, against %d", i, p.to, q.to)
		}
		if p.load && p.imm != q.imm && !(isAddress(p.imm) && isAddress(q.imm)) {
			return fmt.Sprintf("item %d: loads %#x, against %#x", i, p.imm, q.imm)
		}
	}
	c, d := a[text:], b[textB:]
	if len(c) != len(d) {
		return fmt.Sprintf("%d bytes of constants, against %d", len(c), len(d))
	}
	for i := 0; i+8 <= len(c); i += 8 {
		u, v := binary.LittleEndian.Uint64(c[i:]), binary.LittleEndian.Uint64(d[i:])
		if u != v && !(isAddress(u) && isAddress(v)) {
			return fmt.Sprintf("constant %d: %#x, against %#x", i, u, v)
		}
	}
	return ""
}
