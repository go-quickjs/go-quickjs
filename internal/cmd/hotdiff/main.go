// Command hotdiff compares the register interpreter's hot functions in two
// builds, instruction by instruction. It is how a change shows that it leaves
// the interpreter's code alone -- the JIT's hooks when the JIT is not built
// in, say -- which a benchmark, at 5 to 10% layout noise, cannot.
//
//	go build -o base.exe ./cmd/qjs    # at the base revision
//	go build -o head.exe ./cmd/qjs    # at the change
//	go run ./internal/cmd/hotdiff base.exe head.exe
//
// The hot set is the Go table's loop (runReg), its call handler and the
// frames it enters (rhCallW, regEnterW, regCallSlow, regRunFD, callObject),
// the native dispatcher's Go side (nativeRun, nativeLoop, its loop
// nativeDrive, and nativeCallbackRun, a kept callback's entry) and its
// assembly:
// the entry, the exit's call of Go and the generated handler blocks, which
// are position-independent and so compare as they are.
//
// Each function's instructions are compared with their addresses and the
// layout around them taken out: padding (INT3), the NOPs the compiler leaves
// as marks of calls it inlined to nothing, and data addresses, which move
// with the binary. A jump's target becomes the index of the instruction it
// reaches, so the comparison keeps the control flow. A function neither
// build has -- the native block on an architecture without it, or the
// feedback handlers without the JIT -- is reported and skipped. It exits 1
// if any function differs or only one build has it.
//
// With -parity it compares a build without the JIT (base) with one with it
// (head), which must run the interpreter as fast with the JIT off
// (docs/jit-design.md, "The JIT built in and off: parity held by test"),
// both built as the layout lock builds, their pads regenerated:
//
//	CGO_ENABLED=0 GOAMD64=v1 go build -buildvcs=false -o plain.exe ./internal/cmd/v8bench
//	CGO_ENABLED=0 GOAMD64=v1 go build -buildvcs=false -tags quickjs_jit -o jit.exe ./internal/cmd/v8bench
//	go run ./internal/cmd/hotdiff -parity plain.exe jit.exe
//
// The hot set's instructions are the same but for the JIT's entry from a
// call (parityHotSlack); each function of the hot set and of the paths the
// parity rows run in Go (parityPlaced) is at the same address modulo 4 KB,
// the op cache's sets, in both, but those the hooks before them in their
// range move (parityUnplaced), which it notes; and each slow function that carries one of
// the JIT's hooks (paritySlow) is within -slowslack instructions of base's:
// the hook's test and its call, not its body inlined.
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

// hot is what the register interpreter runs for every instruction, call
// and loop.
var hot = []string{
	"internal/vm.(*Runtime).runReg",
	"internal/vm.rhCallW",
	"internal/vm.(*Runtime).regEnterW",
	"internal/vm.(*Runtime).regCallSlow",
	"internal/vm.(*Runtime).regRunFD",
	"internal/vm.(*Runtime).callObject",
	"internal/vm.(*Runtime).nativeRun",
	"internal/vm.(*Runtime).nativeLoop",
	"internal/vm.(*Runtime).nativeDrive",
	"internal/vm.(*Runtime).nativeCallbackRun",
	"internal/vm.nativeEnter",
	"internal/vm.nativeCallH",
	"internal/vm.nativeStoreRef",
	"internal/vm.regNativeHandlers",
	"internal/vm.regNativeFeedbackHandlers",
}

// parityHotSlack is how many instructions more a hot function may have
// with the JIT built in: a call's test of the JIT's entry of its callee
// (jitEnters) and that call, and a native frame stack's growth taking back
// the feedback dispatcher's count. -1 is a function of the JIT's alone.
var parityHotSlack = map[string]int{
	"internal/vm.rhCallW":                   16,
	"internal/vm.(*Runtime).regEnterW":      16,
	"internal/vm.(*Runtime).nativeDrive":    16,
	"internal/vm.regNativeFeedbackHandlers": -1,
}

// parityUnplaced are the hot functions the JIT's hooks before them in
// their range move, which no position holds: each with the hook that
// moves it, a line of -parity's but no failure.
var parityUnplaced = map[string]string{
	"internal/vm.(*Runtime).nativeCallbackRun": "after nativeDrive, 32 bytes longer for its frame stack's growth's call of nativeGrowUncount",
}

// parityPlaced is what, besides the hot set, the rows the parity rule is
// measured by -- string appends, BigInts, generators -- run in Go: each at
// the same offset modulo 4 KB in both builds.
var parityPlaced = []string{
	"internal/vm.(*Runtime).add",
	"internal/vm.(*Runtime).arith",
	"internal/vm.(*Runtime).regBinary",
	"internal/vm.(*Runtime).bitwise",
	"internal/vm.(*Runtime).concat",
	"internal/vm.(*String).Concat",
	"internal/vm.(*pairMemo).concat",
	"internal/vm.strAppend[go.shape.struct {}]",
	"internal/vm.strAppendOp[go.shape.struct {}]",
	"internal/vm.(*Runtime).bigInPlace",
	"internal/vm.(*Runtime).regBigRelease",
	"internal/vm.(*regState).put",
	"internal/vm.rsAdd",
	"internal/vm.rhWrap",
	"internal/vm.(*Runtime).regColdWrap",
	"internal/vm.(*Runtime).regGenRun",
	"internal/vm.(*Runtime).resume",
	"internal/vm.(*Runtime).iterResult",
	"internal/vm.(*partsBuilder).WriteString",
	"internal/vm.(*jsMap).find",
	"internal/vm.nativeBuiltin",
	"internal/vm.nativeNew",
	"internal/vm.(*Runtime).regLeafRun",
	"internal/vm.(*Runtime).startForOf",
	"internal/vm.(*Runtime).instanceOfKnown",
}

// paritySlow are the slow functions that carry the JIT's hooks, each a
// test of the JIT and a call behind it.
var paritySlow = []string{
	"internal/vm.(*Runtime).regSetProp",
	"internal/vm.(*Runtime).regGenRun",
	"internal/vm.(*Runtime).regAsyncRun",
	"internal/vm.(*Runtime).regBackEdge",
	"internal/vm.(*Runtime).regColdWrap",
	"internal/vm.(*propCache).refill",
	"internal/vm.rsAdd",
	"internal/vm.rsAddI",
	"internal/vm.rsAddProp",
	"internal/vm.rsAddI32",
	"internal/vm.rsAddII32",
	"internal/vm.rsSub",
	"internal/vm.rsMul",
	"internal/vm.rsInc",
	"internal/vm.rsBitwise",
	"internal/vm.rsPostStep",
	"internal/vm.rsBin",
	"internal/vm.rsUnary",
}

func main() {
	funcs := flag.String("funcs", "", "comma-separated function names (suffixes of the symbol) instead of the hot set")
	parity := flag.Bool("parity", false, "compare a build without the JIT (base) with one with it (head): the hot set within its slack, its and parityPlaced's offsets modulo 4 KB, the hooks' slow functions within -slowslack")
	slowSlack := flag.Int("slowslack", 96, "with -parity, how many instructions more a hook's slow function may have")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: hotdiff [-funcs a,b] [-parity] base-binary head-binary")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	if *parity {
		base, err := disassemble(flag.Arg(0))
		var head *binary
		if err == nil {
			head, err = disassemble(flag.Arg(1))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		lines, failed := parityCheck(base, head, *slowSlack)
		for _, l := range lines {
			fmt.Println(l)
		}
		if failed {
			os.Exit(1)
		}
		return
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
		b, h := find(base.funcs, name), find(head.funcs, name)
		switch {
		case b == nil && h == nil:
			fmt.Printf("%-40s in neither build\n", name)
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

// parityCheck is -parity's comparison of base, a build without the JIT,
// with head, one with it: a line for each function, and whether any
// failed.
func parityCheck(base, head *binary, slowSlack int) (lines []string, failed bool) {
	say := func(bad bool, format string, args ...any) {
		mark := "ok  "
		if bad {
			mark, failed = "FAIL", true
		}
		lines = append(lines, mark+" "+fmt.Sprintf(format, args...))
	}
	for _, name := range hot {
		b, h := find(base.funcs, name), find(head.funcs, name)
		slack, listed := parityHotSlack[name]
		switch {
		case listed && slack < 0:
			continue
		case b == nil && h == nil:
			lines = append(lines, fmt.Sprintf("     %-44s in neither build", name))
			continue
		case b == nil || h == nil:
			say(true, "%-44s missing (base %v, head %v)", name, b != nil, h != nil)
			continue
		}
		d := len(h) - len(b)
		switch {
		case equal(b, h):
			say(false, "%-44s same (%d instructions)", name, len(b))
		case d >= 0 && d <= slack:
			say(false, "%-44s %d instructions, %+d of the JIT's (slack %d)", name, len(b), d, slack)
		default:
			say(true, "%-44s %d instructions against %d (slack %d)", name, len(b), len(h), slack)
		}
	}
	for _, name := range append(append([]string(nil), hot...), parityPlaced...) {
		if slack, listed := parityHotSlack[name]; listed && slack < 0 {
			continue
		}
		b, okb := findAddr(base.addr, name)
		h, okh := findAddr(head.addr, name)
		if !okb || !okh {
			if okb != okh {
				say(true, "%-44s in one build only", name)
			}
			continue
		}
		if why, ok := parityUnplaced[name]; ok && b%pageMod != h%pageMod {
			lines = append(lines, fmt.Sprintf("note %-44s at %#05x modulo 4 KB, %#05x with the JIT: %s", name, b%pageMod, h%pageMod, why))
			continue
		}
		say(b%pageMod != h%pageMod, "%-44s at %#05x modulo 4 KB, %#05x with the JIT", name, b%pageMod, h%pageMod)
	}
	for _, name := range paritySlow {
		b, h := find(base.funcs, name), find(head.funcs, name)
		if b == nil || h == nil {
			say(b != nil || h != nil, "%-44s missing (base %v, head %v)", name, b != nil, h != nil)
			continue
		}
		d := len(h) - len(b)
		say(d > slowSlack, "%-44s %d instructions, %+d with the JIT (slack %d)", name, len(b), d, slowSlack)
	}
	return lines, failed
}

// pageMod is what -parity compares an address modulo: the op cache's 4 KB.
const pageMod = 4096

// findAddr is find's for a function's address.
func findAddr(addrs map[string]uint64, suffix string) (uint64, bool) {
	for name, a := range addrs {
		if strings.HasSuffix(strings.TrimSuffix(name, ".abi0"), suffix) {
			return a, true
		}
	}
	return 0, false
}

// function is one function's instructions, normalized.
type function []string

// binary is a build's functions: each one's normalized instructions and
// its address, by symbol.
type binary struct {
	funcs map[string]function
	addr  map[string]uint64
}

// disassemble runs go tool objdump over a binary and returns each
// function's normalized instructions and its address by symbol.
func disassemble(path string) (*binary, error) {
	out, err := exec.Command("go", "tool", "objdump", "-s", `internal/vm\.`, path).Output()
	if err != nil {
		return nil, fmt.Errorf("objdump %s: %w", path, err)
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

func parse(out []byte) *binary {
	bin := &binary{funcs: map[string]function{}, addr: map[string]uint64{}}
	var name string
	var lines []line
	flush := func() {
		if name != "" {
			bin.funcs[name] = normalize(lines)
			if len(lines) != 0 {
				bin.addr[name] = lines[0].addr
			}
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
	return bin
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

// find returns the function whose symbol ends with suffix, an assembly
// function's ABI0 symbol included.
func find(funcs map[string]function, suffix string) function {
	for name, f := range funcs {
		if strings.HasSuffix(strings.TrimSuffix(name, ".abi0"), suffix) {
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
