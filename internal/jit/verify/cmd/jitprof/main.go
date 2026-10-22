// Command jitprof attributes a CPU profile's samples in the JIT's native
// code to the code's instructions and to what the backend emitted them for.
//
// Go's profiler records the address of every sample, native code's
// included, where it has no symbol for it. v8bench's -jitcode writes each
// function's code as it is placed -- its address, its bytes and the marks
// the backend left at each stretch (mir.Mark: a block, a value and its
// operation, an edge's moves and poll, a cold path, an exit, a shared
// routine) -- and jitprof joins the two: the native samples by operation,
// by function, and each of the hottest functions' busiest instructions,
// disassembled.
//
//	go run -tags quickjs_jit ./internal/cmd/v8bench -jit -bench awfy -dir /tmp/awfy -suite Towers \
//		-mode fixed -n 10 -jitcode /tmp/towers.jsonl -cpuprofile /tmp/towers.cpu
//	cd internal/jit/verify && go run ./cmd/jitprof -code /tmp/towers.jsonl /tmp/towers.cpu
//
// amd64 only: the marks are amd64's, the disassembler x86asm.
package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/arch/x86/x86asm"
)

// code is a line of -jitcode's file (v8bench's jitCode).
type code struct {
	Seq          int
	Name, Script string
	Line         int
	Addr         uint64
	Text         int
	Bytes        []byte
	Marks        []struct {
		Off int
		Tag string
	}
	Listing string

	// samples by offset, and in all.
	at    map[int]int
	total int
}

func (c *code) label() string { return fmt.Sprintf("%s:%d %s #%d", c.Script, c.Line, c.Name, c.Seq) }

// tagAt is the mark whose stretch holds off.
func (c *code) tagAt(off int) string {
	i, found := slices.BinarySearchFunc(c.Marks, off, func(m struct {
		Off int
		Tag string
	}, off int) int {
		return cmp.Compare(m.Off, off)
	})
	if !found {
		i--
	}
	// Of the marks at one offset, the stretches before the last are
	// empty: a block's start where its first value emits nothing.
	for found && i+1 < len(c.Marks) && c.Marks[i+1].Off == off {
		i++
	}
	if i < 0 {
		return "prologue"
	}
	return c.Marks[i].Tag
}

// kind is what a tag's stretch is, without the value or block it names:
// "v12 CheckShape" is "CheckShape", "cold v12 CheckShape" "cold
// CheckShape", "b3" a block's start, "b3 end If" a block's end of kind If,
// "edge b3->b5" an edge.
func kind(tag string) string {
	fields := strings.Fields(tag)
	switch {
	case len(fields) == 0:
		return tag
	case fields[0] == "cold":
		return "cold " + kind(strings.Join(fields[1:], " "))
	case fields[0] == "edge":
		return "edge (phis, poll)"
	case len(fields) >= 2 && fields[0][0] == 'v' && isNum(fields[0][1:]):
		return strings.Join(fields[1:], " ")
	case fields[0][0] == 'b' && isNum(fields[0][1:]):
		if len(fields) >= 3 && fields[1] == "end" {
			return "end " + fields[2]
		}
		return "block start"
	}
	return tag
}

func isNum(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil && s != ""
}

func main() {
	codePath := flag.String("code", "", "v8bench -jitcode's file")
	top := flag.Int("top", 4, "functions whose busiest instructions to print")
	hot := flag.Int("hot", 40, "instructions to print of each")
	listing := flag.Bool("listing", false, "print each printed function's SSA too")
	full := flag.Bool("full", false, "print every instruction of the printed functions' code, not only the busiest")
	flag.Parse()
	if *codePath == "" || flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: jitprof -code FILE.jsonl PROFILE")
		os.Exit(2)
	}
	codes, err := readCodes(*codePath)
	if err != nil {
		fail(err)
	}
	samples, err := readSamples(flag.Arg(0))
	if err != nil {
		fail(err)
	}
	all, native, unmapped := 0, 0, 0
	for _, s := range samples {
		all += s.count
		if !s.unknown {
			continue
		}
		c := find(codes, s.addr)
		if c == nil {
			unmapped += s.count
			continue
		}
		native += s.count
		off := int(s.addr - c.Addr)
		if c.at == nil {
			c.at = map[int]int{}
		}
		c.at[off] += s.count
		c.total += s.count
	}
	fmt.Printf("%d samples: %d in native code (%.1f%%), %d at addresses no code has\n\n", all, native, pct(native, all), unmapped)
	if native == 0 {
		return
	}

	// By operation, over every function.
	byKind := map[string]int{}
	for _, c := range codes {
		for off, n := range c.at {
			byKind[kind(c.tagAt(off))] += n
		}
	}
	fmt.Println("Native samples by what the code was emitted for:")
	printCounts(byKind, native, 40)

	// By function.
	var hotCodes []*code
	for _, c := range codes {
		if c.total > 0 {
			hotCodes = append(hotCodes, c)
		}
	}
	slices.SortFunc(hotCodes, func(a, b *code) int { return cmp.Compare(b.total, a.total) })
	fmt.Println("\nNative samples by function (#: the compile's sequence number):")
	for i, c := range hotCodes {
		if i == 25 {
			fmt.Printf("  ... %d more\n", len(hotCodes)-i)
			break
		}
		fmt.Printf("  %6d %5.1f%%  %s (%d bytes)\n", c.total, pct(c.total, native), c.label(), c.Text)
	}

	for i, c := range hotCodes {
		if i == *top {
			break
		}
		printCode(c, native, *hot, *listing, *full)
	}
}

// printCode prints c's samples by operation, then its busiest
// instructions in address order, each with its stretch's tag.
func printCode(c *code, native, hot int, listing, full bool) {
	fmt.Printf("\n== %s: %d samples (%.1f%% of native)\n", c.label(), c.total, pct(c.total, native))
	byKind := map[string]int{}
	for off, n := range c.at {
		byKind[kind(c.tagAt(off))] += n
	}
	printCounts(byKind, c.total, 15)
	// Disassembled from the start, so that each instruction is decoded at
	// its own boundary; a sample is at the instruction it falls in.
	insts := decode(c)
	if full {
		fmt.Println("  every instruction (samples, offset, instruction, stretch):")
		byInst := map[int]int{}
		for off, n := range c.at {
			if in, ok := instAt(insts, off); ok {
				byInst[in.off] += n
			}
		}
		last := ""
		for _, in := range insts {
			if in.inst.Len == 0 {
				continue
			}
			tag := c.tagAt(in.off)
			if tag == last {
				tag = ""
			} else {
				last = tag
			}
			n := ""
			if byInst[in.off] > 0 {
				n = strconv.Itoa(byInst[in.off])
			}
			fmt.Printf("  %6s  %5x  %-48s %s\n", n, in.off, x86asm.IntelSyntax(in.inst, c.Addr+uint64(in.off), nil), tag)
		}
		if listing {
			fmt.Println(c.Listing)
		}
		return
	}
	offs := make([]int, 0, len(c.at))
	for off := range c.at {
		offs = append(offs, off)
	}
	slices.SortFunc(offs, func(a, b int) int { return cmp.Or(cmp.Compare(c.at[b], c.at[a]), cmp.Compare(a, b)) })
	if len(offs) > hot {
		offs = offs[:hot]
	}
	slices.Sort(offs)
	fmt.Println("  busiest instructions (samples, offset, instruction, stretch):")
	last := ""
	for _, off := range offs {
		in, ok := instAt(insts, off)
		text := "?"
		if ok {
			text = x86asm.IntelSyntax(in.inst, c.Addr+uint64(in.off), nil)
		}
		tag := c.tagAt(off)
		if tag == last {
			tag = ""
		} else {
			last = tag
		}
		fmt.Printf("  %6d  %5x  %-48s %s\n", c.at[off], off, text, tag)
	}
	if listing {
		fmt.Println(c.Listing)
	}
}

type inst struct {
	off  int
	inst x86asm.Inst
}

func decode(c *code) []inst {
	var out []inst
	end := min(c.Text, len(c.Bytes))
	for off := 0; off < end; {
		in, err := x86asm.Decode(c.Bytes[off:end], 64)
		if err != nil || in.Len == 0 {
			out = append(out, inst{off: off})
			off++
			continue
		}
		out = append(out, inst{off: off, inst: in})
		off += in.Len
	}
	return out
}

// instAt is the instruction off falls in.
func instAt(insts []inst, off int) (inst, bool) {
	i, found := slices.BinarySearchFunc(insts, off, func(in inst, off int) int { return cmp.Compare(in.off, off) })
	if !found {
		i--
	}
	if i < 0 || insts[i].inst.Len == 0 {
		return inst{}, false
	}
	return insts[i], true
}

func printCounts(m map[string]int, total, limit int) {
	type kv struct {
		k string
		n int
	}
	var kvs []kv
	for k, n := range m {
		kvs = append(kvs, kv{k, n})
	}
	slices.SortFunc(kvs, func(a, b kv) int { return cmp.Or(cmp.Compare(b.n, a.n), cmp.Compare(a.k, b.k)) })
	for i, x := range kvs {
		if i == limit {
			rest := 0
			for _, y := range kvs[i:] {
				rest += y.n
			}
			fmt.Printf("  %6d %5.1f%%  (%d more)\n", rest, pct(rest, total), len(kvs)-i)
			break
		}
		fmt.Printf("  %6d %5.1f%%  %s\n", x.n, pct(x.n, total), x.k)
	}
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}

func readCodes(path string) ([]*code, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var codes []*code
	dec := json.NewDecoder(bufio.NewReader(f))
	for dec.More() {
		c := &code{}
		if err := dec.Decode(c); err != nil {
			return nil, err
		}
		codes = append(codes, c)
	}
	return codes, nil
}

// find is the code holding addr: of those that did, the last placed, since
// code flushed leaves its addresses to code placed after it.
func find(codes []*code, addr uint64) *code {
	for i := len(codes) - 1; i >= 0; i-- {
		c := codes[i]
		if addr >= c.Addr && addr < c.Addr+uint64(len(c.Bytes)) {
			return c
		}
	}
	return nil
}

// sample is a profile's sample: its count, its leaf's address, and whether
// the profile knows no function there.
type sample struct {
	count   int
	addr    uint64
	unknown bool
}

var (
	sampleLine   = regexp.MustCompile(`^\s*(\d+)\s+\d+:\s*(\d+)`)
	locationLine = regexp.MustCompile(`^\s*(\d+):\s+0x([0-9a-f]+)\s*(.*)$`)
)

// readSamples reads a profile's samples by its leaves, from go tool pprof
// -raw, which needs no profile parser of this module's own.
func readSamples(path string) ([]sample, error) {
	out, err := exec.Command("go", "tool", "pprof", "-raw", path).Output()
	if err != nil {
		return nil, fmt.Errorf("go tool pprof -raw: %w", err)
	}
	type loc struct {
		addr    uint64
		unknown bool
	}
	locs := map[string]loc{}
	var leaves []struct {
		count int
		id    string
	}
	section := ""
	for _, line := range strings.Split(string(out), "\n") {
		switch strings.TrimSpace(line) {
		case "Samples:", "Locations", "Mappings":
			section = strings.TrimSpace(line)
			continue
		}
		switch section {
		case "Samples:":
			if m := sampleLine.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[1])
				leaves = append(leaves, struct {
					count int
					id    string
				}{n, m[2]})
			}
		case "Locations":
			if m := locationLine.FindStringSubmatch(line); m != nil {
				a, _ := strconv.ParseUint(m[2], 16, 64)
				// A location of no mapping and no function: ":0:0 s=0".
				locs[m[1]] = loc{addr: a, unknown: !strings.Contains(m[3], "M=")}
			}
		}
	}
	samples := make([]sample, 0, len(leaves))
	for _, l := range leaves {
		lc := locs[l.id]
		samples = append(samples, sample{count: l.count, addr: lc.addr, unknown: lc.unknown})
	}
	return samples, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "jitprof:", err)
	os.Exit(1)
}
