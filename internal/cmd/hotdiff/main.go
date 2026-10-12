// Command hotdiff compares the interpreter's hot functions in two builds,
// instruction by instruction. It is how a change shows that it leaves the
// interpreter's code alone -- the JIT's hooks when the JIT is not built in,
// say -- which a benchmark, at 5 to 10% layout noise, cannot.
//
//	go build -o base.exe ./cmd/qjs    # at the base revision
//	go build -o head.exe ./cmd/qjs    # at the change
//	go run ./internal/cmd/hotdiff base.exe head.exe
//
// Each function's instructions are compared with their addresses and the
// layout around them taken out: padding (INT3), the NOPs the compiler leaves
// as marks of calls it inlined to nothing, and data addresses, which move
// with the binary. A jump's target becomes the index of the instruction it
// reaches, so the comparison keeps the control flow. It exits 1 if any
// function differs or is missing.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// hot is what the interpreter and the tree tier run for every instruction,
// call and loop.
var hot = []string{
	"internal/vm.(*Runtime).executeAt",
	"internal/vm.(*Runtime).runTree",
	"internal/vm.(*Runtime).runTreeNested",
	"internal/vm.(*Runtime).callObject",
	"internal/vm.(*Runtime).callTree",
	"internal/vm.(*Runtime).runFD",
	"internal/vm.(*tctx).backEdgeCheck",
}

func main() {
	funcs := flag.String("funcs", "", "comma-separated function names (suffixes of the symbol) instead of the hot set")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: hotdiff [-funcs a,b] base-binary head-binary")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	names := hot
	if *funcs != "" {
		names = strings.Split(*funcs, ",")
	}
	base, err := disassemble(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	head, err := disassemble(flag.Arg(1))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	failed := false
	for _, name := range names {
		b, h := find(base, name), find(head, name)
		switch {
		case b == nil || h == nil:
			fmt.Printf("%-40s missing (base %v, head %v)\n", name, b != nil, h != nil)
			failed = true
		case equal(b, h):
			fmt.Printf("%-40s same (%d instructions)\n", name, len(b))
		default:
			fmt.Printf("%-40s DIFFERS (%d vs %d instructions)\n", name, len(b), len(h))
			report(b, h)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

// function is one function's instructions, normalized.
type function []string

// disassemble runs go tool objdump over a binary and returns each function's
// normalized instructions by symbol.
func disassemble(binary string) (map[string]function, error) {
	out, err := exec.Command("go", "tool", "objdump", "-s", `internal/vm\.`, binary).Output()
	if err != nil {
		return nil, fmt.Errorf("objdump %s: %w", binary, err)
	}
	return parse(out), nil
}

var (
	textLine = regexp.MustCompile(`^TEXT (\S+)\(SB\)`)
	// A data reference: a symbol plus an offset, which moves with the binary.
	dataRef = regexp.MustCompile(`[^\s,()]+\+\d+\(SB\)`)
	hexAddr = regexp.MustCompile(`^0x[0-9a-f]+$`)
)

type line struct {
	addr uint64
	text string
}

func parse(out []byte) map[string]function {
	funcs := map[string]function{}
	var name string
	var lines []line
	flush := func() {
		if name != "" {
			funcs[name] = normalize(lines)
		}
		name, lines = "", nil
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		text := sc.Text()
		if m := textLine.FindStringSubmatch(text); m != nil {
			flush()
			name = m[1]
			continue
		}
		// position, address, encoding, instruction, separated by tabs.
		fields := strings.FieldsFunc(text, func(r rune) bool { return r == '\t' })
		if name == "" || len(fields) < 4 {
			continue
		}
		addr, err := strconv.ParseUint(strings.TrimPrefix(fields[1], "0x"), 16, 64)
		if err != nil {
			continue
		}
		lines = append(lines, line{addr, strings.TrimSpace(fields[3])})
	}
	flush()
	return funcs
}

// normalize drops layout and rewrites addresses as described in the package
// documentation.
func normalize(lines []line) function {
	kept := lines[:0:0]
	for _, l := range lines {
		if strings.HasPrefix(l.text, "NOP") || strings.HasPrefix(l.text, "INT $0x3") || l.text == "?" {
			continue
		}
		kept = append(kept, l)
	}
	// index finds the first kept instruction at or after an address.
	index := func(addr uint64) int {
		for i, l := range kept {
			if l.addr >= addr {
				return i
			}
		}
		return len(kept)
	}
	out := make(function, len(kept))
	for i, l := range kept {
		text := dataRef.ReplaceAllString(l.text, "DATA(SB)")
		if op, target, ok := strings.Cut(text, " "); ok && hexAddr.MatchString(target) {
			if addr, err := strconv.ParseUint(target[2:], 16, 64); err == nil {
				text = fmt.Sprintf("%s @%d", op, index(addr))
			}
		}
		out[i] = text
	}
	return out
}

func find(funcs map[string]function, suffix string) function {
	for name, f := range funcs {
		if strings.HasSuffix(name, suffix) {
			return f
		}
	}
	return nil
}

func equal(a, b function) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// report prints the first instructions where two functions part.
func report(a, b function) {
	shown := 0
	for i := 0; i < max(len(a), len(b)) && shown < 8; i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			fmt.Printf("    %5d  base: %-40s head: %s\n", i, x, y)
			shown++
		}
	}
}
