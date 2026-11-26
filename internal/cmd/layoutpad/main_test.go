package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// go tool nm's names may have spaces: a generic's shape is a struct.
func TestParseNM(t *testing.T) {
	out := []byte("  140261880        224 T github.com/x/vm.joinSize\n" +
		"  140454b20        224 T github.com/x/vm.strAppendNode[go.shape.struct {}].func1\n" +
		"  1420c2a20         16 B runtime.writeBarrier\n")
	syms := parseNM(out)
	s, ok := syms.byName["github.com/x/vm.strAppendNode[go.shape.struct {}].func1"]
	if !ok || s.addr != 0x140454b20 || s.size != 224 || s.kind != "T" {
		t.Fatalf("parsed %+v (%v)", s, ok)
	}
	if w := syms.byName["runtime.writeBarrier"]; w.addr%dataMod != 0x20 || w.kind != "B" {
		t.Fatalf("writeBarrier %+v", w)
	}
}

// A targets file is written back as it was read, its values changed.
func TestTargetsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.txt")
	src := "# comment\ntoolchain go1.27.3\n\nconfig windows/amd64 plain\n" +
		"anchor Q  0x00580 github.com/x/vm.(*Runtime).runReg\n" +
		"anchor W  0x00b60 github.com/x/vm.listSites[go.shape.struct {}].func1\n" +
		"anchor DW 0x020 runtime.writeBarrier\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tg, err := readTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	c := tg.configs[0]
	if c.goos != "windows" || c.jit || len(c.anchors) != 3 || c.anchors[1].symbol != "github.com/x/vm.listSites[go.shape.struct {}].func1" {
		t.Fatalf("read %+v", c)
	}
	c.anchors[0].target = 0x5c0
	if err := writeTargets(path, tg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if want := strings.Replace(src, "0x00580", "0x005c0", 1); string(b) != want {
		t.Fatalf("wrote\n%s\nwant\n%s", b, want)
	}
}

// A pad's stores: as many as the constant says, each to its own byte, none
// next to another.
func TestStores(t *testing.T) {
	var b strings.Builder
	stores(&b, "\t", "n")
	src := b.String()
	if got := strings.Count(src, "layoutPadBytes["); got != maxStores {
		t.Fatalf("%d stores, want %d", got, maxStores)
	}
	last := fmt.Sprintf("layoutPadBytes[%d]", 2*(maxStores-1))
	past := fmt.Sprintf("layoutPadBytes[%d]", 2*maxStores)
	if strings.Contains(src, "layoutPadBytes[1]") || !strings.Contains(src, last) || strings.Contains(src, past) {
		t.Fatal("stores are not every other byte of the array")
	}
	if got := strings.Count(src, "if max(n, 0)&(1<<"); got != storeBits {
		t.Fatalf("%d groups, want %d", got, storeBits)
	}
}

// A checkout on Windows may give go.mod CRLF line endings.
func TestIsModule(t *testing.T) {
	for _, c := range []struct {
		gomod string
		want  bool
	}{
		{"module github.com/go-quickjs/go-quickjs\n\ngo 1.24.0\n", true},
		{"module github.com/go-quickjs/go-quickjs\r\n\r\ngo 1.24.0\r\n", true},
		{"// comment\nmodule github.com/go-quickjs/go-intl\n", false},
		{"go 1.24.0\n", false},
	} {
		if got := isModule([]byte(c.gomod)); got != c.want {
			t.Errorf("isModule(%q) = %v", c.gomod, got)
		}
	}
}

// The assembly pad is whole 32-byte slots of int3 bytes, none where the
// lever is left out, and amd64's alone.
func TestAsmSource(t *testing.T) {
	c := &config{goos: "linux", goarch: "amd64", jit: true}
	h, _ := asmPadOf("H")
	hj, _ := asmPadOf("HJ")
	src, err := asmSource(c, h, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(src, "QUAD $0xcccccccccccccccc"); got != 3*slot/8 {
		t.Fatalf("%d quads, want %d", got, 3*slot/8)
	}
	if !strings.Contains(src, "//go:build quickjs_jit && !quickjs_wasm\n") || !strings.Contains(src, "TEXT ·layoutPadH(SB), NOSPLIT|NOFRAME, $0-0\n") {
		t.Fatalf("source\n%s", src)
	}
	if src, err := asmSource(c, h, -1); err != nil || strings.Contains(src, "TEXT") {
		t.Fatalf("left out: %v\n%s", err, src)
	}
	if _, err := asmSource(&config{goos: "linux", goarch: "arm64"}, h, 1); err == nil {
		t.Fatal("arm64 pad written")
	}
	if got := filepath.Base(asmFile(c, h)); got >= "native_amd64.s" || got <= "jit_leaffast_amd64.s" {
		t.Fatalf("%s does not sort between the JIT's assembly and the native handlers'", got)
	}
	if got := filepath.Base(asmFile(c, hj)); got >= "jit_leaffast_amd64.s" || got <= "jit_bigleaf_amd64.s" {
		t.Fatalf("%s does not sort between the JIT's BigInt leaves and its leaf stubs", got)
	}
}

// A configuration's kind names its tags, its files and its build
// constraint, which no other configuration of its platform builds.
func TestConfigKinds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.txt")
	src := "toolchain go1.27.3\n"
	for _, k := range []string{"plain", "jit", "wasm", "jit+wasm"} {
		src += "config linux/amd64 " + k + "\nanchor Q  0x00580 github.com/x/vm.(*Runtime).runReg\n"
	}
	if err := os.WriteFile(path, []byte(src+"config linux/amd64 wasm+jit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTargets(path); err == nil {
		t.Fatal("read a configuration of no kind")
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tg, err := readTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ name, tags, file, build string }{
		{"linux/amd64 plain", "", "layoutpad_linux_amd64.go", "//go:build !quickjs_jit && !quickjs_wasm\n\n"},
		{"linux/amd64 jit", "quickjs_jit", "layoutpad_jit_linux_amd64.go", "//go:build quickjs_jit && !quickjs_wasm\n\n"},
		{"linux/amd64 wasm", "quickjs_wasm", "layoutpad_wasm_linux_amd64.go", "//go:build !quickjs_jit && quickjs_wasm\n\n"},
		{"linux/amd64 jit+wasm", "quickjs_jit,quickjs_wasm", "layoutpad_jit_wasm_linux_amd64.go", "//go:build quickjs_jit && quickjs_wasm\n\n"},
	}
	if len(tg.configs) != len(want) {
		t.Fatalf("%d configurations", len(tg.configs))
	}
	for i, c := range tg.configs {
		w := want[i]
		if c.String() != w.name || c.tags() != w.tags || filepath.Base(configFile(c)) != w.file || c.buildLine() != w.build {
			t.Errorf("%s: tags %q, file %s, %q; want %+v", c, c.tags(), filepath.Base(configFile(c)), c.buildLine(), w)
		}
	}
}

// A code anchor's recorded address is read and written back with it, and
// one off its own target is refused.
func TestTargetsAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.txt")
	src := "toolchain go1.27.3\nconfig windows/amd64 plain\n" +
		"anchor E  0x008c0 @0x1402f28c0 github.com/x/vm.firstElem\n" +
		"anchor W  0x00b60 @0x1404a0b60 github.com/x/vm.listSites[go.shape.struct {}].func1\n" +
		"anchor DW 0x020 runtime.writeBarrier\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tg, err := readTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	a := tg.configs[0].anchors
	if a[0].at != 0x1402f28c0 || a[0].symbol != "github.com/x/vm.firstElem" || a[1].symbol != "github.com/x/vm.listSites[go.shape.struct {}].func1" || a[2].at != 0 {
		t.Fatalf("read %+v", a)
	}
	a[0].at += codeMod
	if err := writeTargets(path, tg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if want := strings.Replace(src, "@0x1402f28c0", "@0x1402f38c0", 1); string(b) != want {
		t.Fatalf("wrote\n%s\nwant\n%s", b, want)
	}
	bad := strings.Replace(src, "@0x1402f28c0", "@0x1402f28e0", 1)
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTargets(path); err == nil {
		t.Fatal("read an address off its anchor's target")
	}
}

// c7c7830a's layout, windows/amd64 plain: three keyOrder calls took
// firstElem 96 bytes past E's least, and E's pad went from none to 4,011
// bytes. Every anchor stayed at its target modulo 4 KB, so the targets
// alone pass; firstElem and everything after it were a page further than
// at f18011fa, which the recorded addresses catch, naming E.
func TestPageDriftWrappedPad(t *testing.T) {
	const vm = "github.com/x/vm."
	c := &config{goos: "windows", goarch: "amd64", anchors: []anchor{
		{lever: "A", symbol: vm + "joinSize", target: 0xcc0, at: 0x1402e2cc0},
		{lever: "I", symbol: vm + "bigParts", target: 0x4c0, at: 0x1402e64c0},
		{lever: "E", symbol: vm + "firstElem", target: 0x8c0, at: 0x1402f28c0},
		{lever: "T", symbol: vm + "(*jsonEncoder).encode", target: 0x960, at: 0x140313960},
		{lever: "C", symbol: vm + "(*Runtime).asciiCaseMemo", target: 0x800, at: 0x14033d800},
		{lever: "DW", symbol: "runtime.writeBarrier", target: 0x10},
	}}
	nm := func(lines ...string) *symtab { return parseNM([]byte(strings.Join(lines, "\n") + "\n")) }
	pad := func(name string, addr, size uint64) string {
		return fmt.Sprintf("  %x %10d T %slayoutPad%s", addr, size, vmPrefix, name)
	}
	f18011fa := nm(
		pad("E", 0x1402ead60, 64),
		"  1402e2cc0        224 T "+vm+"joinSize",
		"  1402e64c0        512 T "+vm+"bigParts",
		"  1402f28c0        864 T "+vm+"firstElem",
		"  140313960        800 T "+vm+"(*jsonEncoder).encode",
		"  14033d800        320 T "+vm+"(*Runtime).asciiCaseMemo",
		"  1420c2a10         16 B runtime.writeBarrier")
	c7c7830a := nm(
		pad("E", 0x1402ead60, 4064),
		"  1402e2cc0        224 T "+vm+"joinSize",
		"  1402e64c0        512 T "+vm+"bigParts",
		"  1402f38c0        864 T "+vm+"firstElem",
		"  140314960        800 T "+vm+"(*jsonEncoder).encode",
		"  14033e800        320 T "+vm+"(*Runtime).asciiCaseMemo",
		"  1420c3a10         16 B runtime.writeBarrier")
	for name, syms := range map[string]*symtab{"f18011fa": f18011fa, "c7c7830a": c7c7830a} {
		if lines := compare(c, syms); len(lines) != 0 {
			t.Fatalf("%s: the targets alone %v", name, lines)
		}
	}
	if lines := pageDrift(c, f18011fa); len(lines) != 0 {
		t.Fatalf("the recorded layout drifted: %v", lines)
	}
	lines := pageDrift(c, c7c7830a)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want one at E:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, want := range []string{"anchor E (" + vm + "firstElem)", "a page further", "the anchors after it moved with it",
		"its pad E (layoutPadE, 4064 bytes)", "builtin_layoutpad.go", "anchor I (" + vm + "bigParts) before it is where it was"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("%q lacks %q", lines[0], want)
		}
	}
}

// A move of the anchors after N by two pages against those before it, which
// moved by one, is a move of its own: the JIT's builds at 91f885a1, where
// the compiler before package jit grew and N's pad wrapped.
func TestPageDriftTwoMoves(t *testing.T) {
	const vm = "github.com/x/vm."
	c := &config{goos: "windows", goarch: "amd64", jit: true, anchors: []anchor{
		{lever: "XJ", symbol: "github.com/x/jit.ExpandGoCall", target: 0x100, at: 0x140100100},
		{lever: "A", symbol: vm + "joinSize", target: 0xcc0, at: 0x140200cc0},
		{lever: "N", symbol: vm + "(*Runtime).nativeRun", target: 0xce0, at: 0x140300ce0},
		{lever: "Q", symbol: vm + "(*Runtime).runReg", target: 0x580, at: 0x140310580},
	}}
	syms := parseNM([]byte("  140101100 100 T github.com/x/jit.ExpandGoCall\n" +
		"  140201cc0 100 T " + vm + "joinSize\n" +
		"  140302ce0 100 T " + vm + "(*Runtime).nativeRun\n" +
		"  140312580 100 T " + vm + "(*Runtime).runReg\n"))
	lines := pageDrift(c, syms)
	if len(lines) != 2 || !strings.Contains(lines[0], "anchor XJ") || !strings.Contains(lines[0], "what is linked before") ||
		!strings.Contains(lines[1], "anchor N") || !strings.Contains(lines[1], "2 pages further from") || !strings.Contains(lines[1], "anchor A ("+vm+"joinSize) before it moved by 1") {
		t.Fatalf("got\n%s", strings.Join(lines, "\n"))
	}
}

// A pad that can take up a change keeps its anchor on its recorded
// address: one that must grow past a page grows, and one a previous solve
// wrapped grows back, a function pad by stores, an assembly pad by slots.
func TestSolveCodeKeepsRecordedPage(t *testing.T) {
	for _, tc := range []struct {
		name, lever string
		k           int
		width, addr uint64
		want        int
	}{
		{"growing-function", "J", 576, 4096, 0xfff20, 608},
		{"wrapped-function", "J", 2, 32, 0xff000, 587},
		{"growing-assembly", "H", 128, 4096, 0xfff20, 135},
		{"wrapped-assembly", "H", 1, 32, 0xff000, 129},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clear(leverBefore)
			l, _ := leverByName(tc.lever)
			c := &config{anchors: []anchor{{lever: tc.lever, symbol: "anchor", at: 0x100000}}}
			syms := &symtab{byName: map[string]symbol{
				l.pad:    {addr: tc.addr - tc.width, size: tc.width},
				"anchor": {addr: tc.addr},
			}}
			v := map[string]int{tc.lever: tc.k}
			changed, err := solveCode(c, syms, v, false)
			if err != nil || !changed || v[tc.lever] != tc.want {
				t.Fatalf("changed %v, error %v, stores/slots %d, want %d", changed, err, v[tc.lever], tc.want)
			}
		})
	}
}

// An anchor that no size of its pad brings back to its recorded address
// is left where the solve modulo 4 KB puts it, and the page check reports
// it.
func TestSolveCodeCannotRecoverRecordedPage(t *testing.T) {
	clear(leverBefore)
	l, _ := leverByName("J")
	c := &config{anchors: []anchor{{lever: "J", symbol: "anchor", at: 0x100000}}}
	// Even an empty pad cannot move this anchor back to its recorded page.
	syms := &symtab{byName: map[string]symbol{
		l.pad:    {addr: 0x100fe0, size: 32},
		"anchor": {addr: 0x101000},
	}}
	v := map[string]int{"J": 2}
	changed, err := solveCode(c, syms, v, false)
	if err != nil || changed || v["J"] != 2 {
		t.Fatalf("changed %v, error %v, stores %d", changed, err, v["J"])
	}
	if len(pageDrift(c, syms)) != 1 {
		t.Fatal("unrecoverable page move was not reported")
	}
}

// A pad's growth moves the anchors after it in the same pass: the next
// anchor, on its recorded address once the first pad has grown, leaves
// its own pad as it is.
func TestSolveCodeAccountsForEarlierPad(t *testing.T) {
	clear(leverBefore)
	j, _ := leverByName("J")
	n, _ := leverByName("N")
	c := &config{anchors: []anchor{
		{lever: "J", symbol: "first", at: 0x100000},
		{lever: "N", symbol: "next", at: 0x110000},
	}}
	syms := &symtab{byName: map[string]symbol{
		j.pad:   {addr: 0xfef20, size: 4096},
		"first": {addr: 0xfff20},
		n.pad:   {addr: 0x10ff00, size: 32},
		"next":  {addr: 0x10ff20},
	}}
	v := map[string]int{"J": 576, "N": 2}
	changed, err := solveCode(c, syms, v, false)
	if err != nil || !changed || v["J"] != 608 || v["N"] != 2 {
		t.Fatalf("changed %v, error %v, stores %v", changed, err, v)
	}
}

// TestJITConfigsAtPlainTargets checks the parity rule of the targets file:
// a build with the JIT, run with the JIT off, must be no slower than one
// without it, so the configurations with the JIT put the interpreter's code
// and the runtime's data where those without it do. Each code anchor
// plain has has its target in jit and in jit+wasm (whose code anchors are
// plain's, as wasm's are), and each of plain's data anchors in jit. The
// WebAssembly builds' runtime data is laid out anew, and a data target
// plain's of theirs cannot reach is the nearest that holds (linux/amd64
// jit+wasm keeps its own); the JIT's own anchors -- its leaves, its native
// entries, its compiler -- are its alone.
// With targets of their own, the JIT's configurations had run the same
// handlers' bytes on other sets of the op cache, and string appends and
// BigInts were 7 to 16% slower at every placement with the JIT built in
// and off.
func TestJITConfigsAtPlainTargets(t *testing.T) {
	tg, err := readTargets(filepath.Join("..", "..", "vm", "layoutlock.txt"))
	if err != nil {
		t.Fatal(err)
	}
	find := func(goos, goarch, kind string) *config {
		for _, c := range tg.configs {
			if c.goos == goos && c.goarch == goarch && c.kind() == kind {
				return c
			}
		}
		return nil
	}
	checked := 0
	for _, c := range tg.configs {
		if !c.jit {
			continue
		}
		base := find(c.goos, c.goarch, "plain")
		if base == nil {
			t.Errorf("%s: no plain configuration to take its targets from", c)
			continue
		}
		for _, b := range base.anchors {
			if c.wasm && modOf(b.lever) != codeMod {
				continue
			}
			found := false
			for _, a := range c.anchors {
				if a.lever != b.lever {
					continue
				}
				found = true
				if a.symbol != b.symbol || a.target != b.target {
					t.Errorf("%s: anchor %s is %s at %#x, plain's %s at %#x", c, a.lever, a.symbol, a.target, b.symbol, b.target)
				}
			}
			if !found {
				t.Errorf("%s: no anchor %s, which plain has", c, b.lever)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no configuration with the JIT")
	}
}
