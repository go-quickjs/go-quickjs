package main

import (
	"bufio"
	"bytes"
	"debug/elf"
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register interpreter's hot cases. Q pins runReg's start, so a case's
// address modulo 4 KB -- the op cache's set -- is decided by the code
// before it in the loop alone. The hot cases come first in runReg's switch
// and everything else after them, so that only an edit to a hot case moves
// another; casesRel holds, for each configuration, where each hot case's
// code starts in runReg, from the build's dispatch table, and -check fails
// where one has moved (docs/optimizing.md, "The layout lock"). The file
// names the hot cases itself, on its hot line, and go generate rewrites
// their offsets.

const casesRel = "internal/vm/layoutcases.txt"

// runRegSym is the loop whose dispatch table the cases are read from.
const runRegSym = vmPrefix + "(*Runtime).runReg"

// readCases reads the entry of each opcode's case in runReg's dispatch,
// as an offset from runReg's start: the table the first indirect jump in
// runReg reads, whose address the LEAQ before the jump loads.
func readCases(exe string, syms *symtab, goos string) ([128]uint64, error) {
	var cases [128]uint64
	start, ok := syms.byName[runRegSym]
	if !ok {
		return cases, fmt.Errorf("%s is not in the build", runRegSym)
	}
	out, err := exec.Command("go", "tool", "objdump", "-s", `vm\.\(\*Runtime\)\.runReg$`, exe).Output()
	if err != nil {
		return cases, fmt.Errorf("go tool objdump: %w", err)
	}
	jump := regexp.MustCompile(`JMP 0\(R\w+\)\(R\w+\*8\)`)
	// The table's address as objdump prints the LEAQ: a symbol and an
	// offset where it finds one (PE), the displacement from the next
	// instruction otherwise (ELF).
	leaSym := regexp.MustCompile(`LEAQ ([^\s+()]+)(?:\+(\d+))?\(SB\)`)
	leaIP := regexp.MustCompile(`LEAQ (-?0x[0-9a-f]+)\(IP\)`)
	var table uint64
	var last string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if jump.MatchString(line) {
			if m := leaIP.FindStringSubmatch(last); m != nil {
				// The line: position, address, the instruction's bytes.
				f := strings.Fields(last)
				if len(f) < 3 {
					return cases, fmt.Errorf("runReg's dispatch: %q", last)
				}
				at, err1 := strconv.ParseUint(strings.TrimPrefix(f[1], "0x"), 16, 64)
				disp, err2 := strconv.ParseInt(m[1], 0, 64)
				if err1 != nil || err2 != nil {
					return cases, fmt.Errorf("runReg's dispatch: %q", last)
				}
				table = uint64(int64(at) + int64(len(f[2])/2) + disp)
				break
			}
			m := leaSym.FindStringSubmatch(last)
			if m == nil {
				return cases, errors.New("runReg's dispatch: no LEAQ of its table before the jump")
			}
			s, ok := syms.byName[m[1]]
			if !ok {
				return cases, fmt.Errorf("runReg's dispatch: the table's symbol %s is not in the build", m[1])
			}
			off, _ := strconv.ParseUint(m[2], 10, 64)
			table = s.addr + off
			break
		}
		if strings.Contains(line, "LEAQ") {
			last = line
		}
	}
	if table == 0 {
		return cases, errors.New("runReg has no dispatch table")
	}
	words, err := readAt(exe, goos, table, 8*128)
	if err != nil {
		return cases, err
	}
	for i := range cases {
		a := binary.LittleEndian.Uint64(words[8*i:])
		if a < start.addr || a >= start.addr+start.size {
			return cases, fmt.Errorf("runReg's dispatch table: entry %d at %#x is outside runReg", i, a)
		}
		cases[i] = a - start.addr
	}
	return cases, nil
}

// readAt reads n bytes at address addr of the binary's image.
func readAt(exe, goos string, addr uint64, n int) ([]byte, error) {
	buf := make([]byte, n)
	if goos == "windows" {
		f, err := pe.Open(exe)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		var base uint64
		switch h := f.OptionalHeader.(type) {
		case *pe.OptionalHeader64:
			base = h.ImageBase
		case *pe.OptionalHeader32:
			base = uint64(h.ImageBase)
		}
		for _, s := range f.Sections {
			lo := base + uint64(s.VirtualAddress)
			if addr >= lo && addr+uint64(n) <= lo+uint64(s.VirtualSize) {
				if _, err := s.ReadAt(buf, int64(addr-lo)); err != nil {
					return nil, err
				}
				return buf, nil
			}
		}
		return nil, fmt.Errorf("no section holds %#x", addr)
	}
	f, err := elf.Open(exe)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	for _, s := range f.Sections {
		if s.Type != elf.SHT_NOBITS && addr >= s.Addr && addr+uint64(n) <= s.Addr+s.Size {
			if _, err := s.ReadAt(buf, int64(addr-s.Addr)); err != nil {
				return nil, err
			}
			return buf, nil
		}
	}
	return nil, fmt.Errorf("no section holds %#x", addr)
}

// opByName is the opcode of a case's name, as bytecode.ROp prints it.
func opByName(name string) (int, bool) {
	for v := range 128 {
		if bytecode.ROp(v).String() == name {
			return v, true
		}
	}
	return 0, false
}

// caseFile is the golden file: the hot cases, and their offsets by
// configuration.
type caseFile struct {
	header  []string // the comment lines it starts with
	hot     []string
	offsets map[string]map[string]uint64 // configuration -> case -> offset
}

func readCaseFile() (*caseFile, error) {
	b, err := os.ReadFile(casesRel)
	if err != nil {
		return nil, err
	}
	cf := &caseFile{offsets: map[string]map[string]uint64{}}
	var cur map[string]uint64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "#"):
			cf.header = append(cf.header, line)
		case len(f) > 1 && f[0] == "hot":
			cf.hot = f[1:]
			for _, name := range cf.hot {
				if _, ok := opByName(name); !ok {
					return nil, fmt.Errorf("%s: no register operation is named %s", casesRel, name)
				}
			}
		case len(f) == 3 && f[0] == "config":
			cur = map[string]uint64{}
			cf.offsets[f[1]+" "+f[2]] = cur
		case len(f) == 3 && f[0] == "case" && cur != nil:
			v, err := strconv.ParseUint(f[2], 0, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %q: %v", casesRel, line, err)
			}
			cur[f[1]] = v
		}
	}
	return cf, nil
}

func (cf *caseFile) write(order []*config) error {
	var b strings.Builder
	for _, h := range cf.header {
		b.WriteString(h + "\n")
	}
	b.WriteString("hot " + strings.Join(cf.hot, " ") + "\n")
	for _, c := range order {
		o, ok := cf.offsets[c.String()]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "\nconfig %s\n", c)
		for _, name := range cf.hot {
			fmt.Fprintf(&b, "case %s %#05x\n", name, o[name])
		}
	}
	return os.WriteFile(casesRel, []byte(b.String()), 0o644)
}

// record sets c's offsets to the build's.
func (cf *caseFile) record(c *config, syms *symtab) {
	o := map[string]uint64{}
	for _, name := range cf.hot {
		op, _ := opByName(name)
		o[name] = syms.cases[op]
	}
	cf.offsets[c.String()] = o
}

// compareCases lists each hot case away from its offset in the golden file.
func (cf *caseFile) compareCases(c *config, syms *symtab) []string {
	want, ok := cf.offsets[c.String()]
	if !ok {
		return []string{fmt.Sprintf("%s has no offsets for %s: run go generate ./internal/vm", casesRel, c)}
	}
	var out []string
	for _, name := range cf.hot {
		op, _ := opByName(name)
		got := syms.cases[op]
		if w, ok := want[name]; !ok || got != w {
			out = append(out, fmt.Sprintf("runReg's case %s starts at +%#x, the golden file's +%#x: moved by %+d bytes"+
				" -- an edit to a hot case, or code put before one; a deliberate one regenerates %s and is timed",
				name, got, w, int64(got)-int64(w), casesRel))
		}
	}
	return out
}
