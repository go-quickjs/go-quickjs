// Command jitquality compares what the backend's allocation left in the
// code of runs' compiles: v8bench -jitcode's files, one for each build, of
// the same program. For each file it counts the values spilled (the dump's
// Spills) and the register moves in the edges' code (mir's phi moves: the
// stretches marked "edge"), and, given two, lists the functions -- by
// script, line, name and the how-manieth compile of it -- that have more
// of either in the second than in the first.
//
//	go run -tags quickjs_jit ./internal/cmd/v8bench -jit -bench awfy -dir /tmp/awfy -mode fixed -n 1 -jitcode /tmp/a.jsonl
//	cd internal/jit/verify && go run ./cmd/jitquality /tmp/a.jsonl /tmp/b.jsonl
//
// amd64 only: the disassembler is x86asm.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/arch/x86/x86asm"
)

// code is a line of -jitcode's file (v8bench's jitCode), what of it is
// counted.
type code struct {
	Seq          int
	Name, Script string
	Line         int
	Text         int
	Bytes        []byte
	Marks        []struct {
		Off int
		Tag string
	}
	Spills int
}

// counts are a function's.
type counts struct {
	spills, moves, text int
}

func main() {
	show := flag.Int("show", 20, "functions to list that are worse in the second file")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: jitquality FILE.jsonl [FILE2.jsonl]")
		os.Exit(2)
	}
	var runs []map[string]counts
	var orders [][]string
	for _, path := range flag.Args() {
		m, order, err := read(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "jitquality:", err)
			os.Exit(1)
		}
		var t counts
		for _, c := range m {
			t.spills += c.spills
			t.moves += c.moves
			t.text += c.text
		}
		fmt.Printf("%s: %d compiles, %d values spilled, %d edge moves, %d bytes of code\n", path, len(m), t.spills, t.moves, t.text)
		runs = append(runs, m)
		orders = append(orders, order)
	}
	if len(runs) < 2 {
		return
	}
	a, b := runs[0], runs[1]
	common, worse, better, same := 0, 0, 0, 0
	var ta, tb counts
	for _, k := range orders[1] {
		x, ok := a[k]
		if !ok {
			continue
		}
		y := b[k]
		common++
		ta.spills, ta.moves, ta.text = ta.spills+x.spills, ta.moves+x.moves, ta.text+x.text
		tb.spills, tb.moves, tb.text = tb.spills+y.spills, tb.moves+y.moves, tb.text+y.text
		switch {
		case y.spills > x.spills || y.moves > x.moves:
			if worse++; worse <= *show {
				fmt.Printf("  worse: %s: spills %d -> %d, edge moves %d -> %d\n", k, x.spills, y.spills, x.moves, y.moves)
			}
		case y.spills < x.spills || y.moves < x.moves:
			better++
		default:
			same++
		}
	}
	fmt.Printf("%d compiles in both: %d worse, %d better, %d the same; spills %d -> %d, edge moves %d -> %d, code %d -> %d bytes\n",
		common, worse, better, same, ta.spills, tb.spills, ta.moves, tb.moves, ta.text, tb.text)
}

// read counts each compile of a file, by its key: script, line, name and
// how many compiles of that function came before it.
func read(path string) (map[string]counts, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	out := map[string]counts{}
	var order []string
	seen := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	for sc.Scan() {
		var c code
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, nil, err
		}
		name := fmt.Sprintf("%s:%d %s", c.Script, c.Line, c.Name)
		key := fmt.Sprintf("%s #%d", name, seen[name])
		seen[name]++
		out[key] = counts{spills: c.Spills, moves: edgeMoves(&c), text: c.Text}
		order = append(order, key)
	}
	return out, order, sc.Err()
}

// edgeMoves counts the register-to-register moves in c's stretches for
// edges, as the verifier's TestMirPhisCoalesced counts them.
func edgeMoves(c *code) int {
	n := 0
	for k, m := range c.Marks {
		if !strings.HasPrefix(m.Tag, "edge ") && !strings.HasPrefix(m.Tag, "cold edge ") {
			continue
		}
		end := c.Text
		if k+1 < len(c.Marks) {
			end = min(end, c.Marks[k+1].Off)
		}
		for off := m.Off; off < end && off < len(c.Bytes); {
			in, err := x86asm.Decode(c.Bytes[off:end], 64)
			if err != nil {
				break
			}
			off += in.Len
			_, r0 := in.Args[0].(x86asm.Reg)
			_, r1 := in.Args[1].(x86asm.Reg)
			if (in.Op == x86asm.MOV || in.Op == x86asm.MOVAPD) && r0 && r1 {
				n++
			}
		}
	}
	return n
}
