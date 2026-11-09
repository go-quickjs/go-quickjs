package decoder

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// verdict returns what compiling b gives: "OK", or the CompileError's
// message as Node prints it.
func verdict(b []byte) string {
	l := V8Limits()
	m, err := Decode(b, l)
	if err == nil {
		err = ValidateFunctions(m, l)
	}
	if err != nil {
		return "WebAssembly.Module(): " + jsString(err.Error())
	}
	return "OK"
}

// jsString returns s as a JavaScript string made from its bytes reads it:
// each byte that is not part of a valid UTF-8 sequence becomes U+FFFD. A
// function name cut at 50 bytes can end in part of a character.
func jsString(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		b.WriteRune(r)
	}
	return b.String()
}

const nodeScript = `
const fs = require('fs');
const dir = process.argv[2];
const n = +process.argv[3];
const out = [];
for (let i = 0; i < n; i++) {
  const bytes = fs.readFileSync(dir + '/' + i + '.wasm');
  try {
    new WebAssembly.Module(bytes);
    out.push('OK');
  } catch (e) {
    out.push(e instanceof WebAssembly.CompileError ? e.message : 'other: ' + e);
  }
}
fs.writeFileSync(dir + '/out.json', JSON.stringify(out));
`

// nodeVerdicts compiles each module in Node, one process for all of them.
func nodeVerdicts(t *testing.T, cases []corpusCase) []string {
	t.Helper()
	dir := t.TempDir()
	for i, c := range cases {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.wasm", i)), c.b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(dir, "run.js")
	if err := os.WriteFile(script, []byte(nodeScript), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", script, dir, fmt.Sprint(len(cases)))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.json"))
	if err != nil {
		t.Fatal(err)
	}
	var res []string
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// splitSections returns the [start, end) of each section of a module whose
// header is valid, and the [start, end) of each section's payload.
type sectionSpan struct {
	id                  byte
	start, payload, end int
}

func splitSections(b []byte) []sectionSpan {
	var out []sectionSpan
	p := 8
	for p < len(b) {
		start := p
		id := b[p]
		p++
		n, l := peekU32v(b, p)
		if l == 0 || p+l+int(n) > len(b) {
			return out
		}
		p += l
		out = append(out, sectionSpan{id, start, p, p + int(n)})
		p += int(n)
	}
	return out
}

// codeBodies returns the spans of the function bodies (after their size)
// of a module's code section.
func codeBodies(b []byte) (sec sectionSpan, bodies [][2]int, ok bool) {
	for _, s := range splitSections(b) {
		if s.id != 10 {
			continue
		}
		p := s.payload
		count, l := peekU32v(b, p)
		p += l
		for i := uint32(0); i < count; i++ {
			size, l := peekU32v(b, p)
			if l == 0 {
				return s, nil, false
			}
			p += l
			bodies = append(bodies, [2]int{p, p + int(size)})
			p += int(size)
		}
		return s, bodies, true
	}
	return sectionSpan{}, nil, false
}

// rebuildCode replaces body i of module b by nb, re-encoding the sizes.
func rebuildCode(b []byte, i int, nb []byte) []byte {
	s, bodies, ok := codeBodies(b)
	if !ok {
		return nil
	}
	var payload []byte
	payload = append(payload, uleb(uint64(len(bodies)))...)
	for j, bd := range bodies {
		x := b[bd[0]:bd[1]]
		if j == i {
			x = nb
		}
		payload = append(payload, uleb(uint64(len(x)))...)
		payload = append(payload, x...)
	}
	out := append([]byte{}, b[:s.start]...)
	out = append(out, sec(10, payload)...)
	return append(out, b[s.end:]...)
}

// replaceSection replaces section span s of b by a section of id with
// payload.
func replaceSection(b []byte, s sectionSpan, payload []byte) []byte {
	out := append([]byte{}, b[:s.start]...)
	out = append(out, sec(s.id, payload)...)
	return append(out, b[s.end:]...)
}

var interestingBytes = []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x1a, 0x20, 0x40,
	0x41, 0x60, 0x63, 0x64, 0x6e, 0x6f, 0x70, 0x7f, 0x80, 0xc0, 0xd0, 0xfb, 0xfc, 0xfd, 0xfe, 0xff}

func randByte(r *rand.Rand) byte {
	if r.Intn(2) == 0 {
		return interestingBytes[r.Intn(len(interestingBytes))]
	}
	return byte(r.Intn(256))
}

// mutate returns n mutations of module b, function bodies and sections.
func mutate(name string, b []byte, n int, r *rand.Rand) []corpusCase {
	var out []corpusCase
	add := func(kind string, x []byte) {
		if x != nil {
			out = append(out, corpusCase{fmt.Sprintf("%s/%s/%d", name, kind, len(out)), x})
		}
	}
	_, bodies, _ := codeBodies(b)
	sections := splitSections(b)
	for len(out) < n {
		switch k := r.Intn(12); {
		case k < 5 && len(bodies) > 0:
			i := r.Intn(len(bodies))
			bd := append([]byte{}, b[bodies[i][0]:bodies[i][1]]...)
			if len(bd) == 0 {
				continue
			}
			p := r.Intn(len(bd))
			switch r.Intn(5) {
			case 0, 1:
				bd[p] = randByte(r)
				add("body-flip", rebuildCode(b, i, bd))
			case 2:
				add("body-trunc", rebuildCode(b, i, bd[:p]))
			case 3:
				nb := append(append(append([]byte{}, bd[:p]...), randByte(r)), bd[p:]...)
				add("body-insert", rebuildCode(b, i, nb))
			case 4:
				nb := append(append([]byte{}, bd[:p]...), bd[p+1:]...)
				add("body-delete", rebuildCode(b, i, nb))
			}
		case k < 8:
			x := append([]byte{}, b...)
			p := 8 + r.Intn(len(x)-8)
			switch r.Intn(4) {
			case 0, 1:
				x[p] = randByte(r)
				add("flip", x)
			case 2:
				add("trunc", x[:p])
			case 3:
				x = append(append(append([]byte{}, b[:p]...), randByte(r)), b[p:]...)
				add("insert", x)
			}
		default:
			if len(sections) == 0 {
				continue
			}
			s := sections[r.Intn(len(sections))]
			payload := b[s.payload:s.end]
			count, l := peekU32v(payload, 0)
			if l == 0 {
				continue
			}
			rest := payload[l:]
			switch r.Intn(8) {
			case 6, 7:
				// A LEB in the payload made longer: a byte that ends one
				// padded to the same value, or to one with too many bits.
				var ends []int
				for i, c := range payload {
					if c < 0x80 {
						ends = append(ends, i)
					}
				}
				if len(ends) == 0 {
					continue
				}
				i := ends[r.Intn(len(ends))]
				pad := bs(payload[i]|0x80, 0x80, 0)
				kind := "leb-overlong"
				if r.Intn(2) == 0 {
					pad = bs(payload[i]|0x80, 0xff, 0xff, 0xff, 0x7f)
					kind = "leb-overflow"
				}
				add(kind, replaceSection(b, s, cat(payload[:i], pad, payload[i+1:])))
			case 0:
				add("count-inc", replaceSection(b, s, cat(uleb(uint64(count)+1), rest)))
			case 1:
				if count > 0 {
					add("count-dec", replaceSection(b, s, cat(uleb(uint64(count)-1), rest)))
				}
			case 2:
				// An overlong count: padded to five bytes.
				c := uleb(uint64(count))
				for len(c) < 5 {
					c[len(c)-1] |= 0x80
					c = append(c, 0)
				}
				add("count-overlong", replaceSection(b, s, cat(c, rest)))
			case 3:
				add("count-overflow", replaceSection(b, s, cat(bs(0xff, 0xff, 0xff, 0xff, 0x7f), rest)))
			case 4:
				add("count-huge", replaceSection(b, s, cat(uleb(uint64(r.Uint32())), rest)))
			case 5:
				// A payload byte flipped, the section's size kept right.
				if len(rest) == 0 {
					continue
				}
				nr := append([]byte{}, rest...)
				nr[r.Intn(len(nr))] = randByte(r)
				add("payload-flip", replaceSection(b, s, cat(payload[:l], nr)))
			}
		}
	}
	return out
}

// corpus returns the hand-written cases, the seeds and their mutations:
// QUICKJS_WASM_MUTATIONS of each (3,000 by default), from random seed
// QUICKJS_WASM_SEED (1 by default).
func corpus() []corpusCase {
	n, seed := 3000, int64(1)
	if v, err := strconv.Atoi(os.Getenv("QUICKJS_WASM_MUTATIONS")); err == nil {
		n = v
	}
	if v, err := strconv.ParseInt(os.Getenv("QUICKJS_WASM_SEED"), 10, 64); err == nil {
		seed = v
	}
	cs := handCases()
	r := rand.New(rand.NewSource(seed))
	for _, s := range seeds() {
		cs = append(cs, s)
		cs = append(cs, mutate(s.name, s.b, n, r)...)
	}
	return cs
}

// TestCompareNode validates the corpus here and in Node and compares the
// verdicts and messages, module by module. It needs node on PATH and runs
// when QUICKJS_COMPARE_NODE_WASM=1. With QUICKJS_WASM_UPDATE_GOLDEN=1 it
// also rewrites testdata/hand.golden from Node's verdicts.
func TestCompareNode(t *testing.T) {
	if os.Getenv("QUICKJS_COMPARE_NODE_WASM") != "1" {
		t.Skip("set QUICKJS_COMPARE_NODE_WASM=1 to compare with node")
	}
	cs := corpus()
	got := nodeVerdicts(t, cs)
	if len(got) != len(cs) {
		t.Fatalf("node returned %d verdicts for %d modules", len(got), len(cs))
	}
	diffs := 0
	kinds := map[string]int{}
	for i, c := range cs {
		ours := verdict(c.b)
		if ours != got[i] {
			diffs++
			kind := c.name
			if j := strings.LastIndexByte(kind, '/'); j > 0 {
				kind = kind[:j]
			}
			kinds[kind]++
			if diffs <= 40 {
				t.Errorf("%s:\n  node: %s\n  ours: %s\n  module: %x", c.name, got[i], ours, c.b)
			}
		}
	}
	if os.Getenv("QUICKJS_WASM_UPDATE_GOLDEN") == "1" {
		var b strings.Builder
		for i, c := range cs {
			if strings.HasPrefix(c.name, "hand/") {
				fmt.Fprintf(&b, "%s\t%s\n", c.name, got[i])
			}
		}
		if err := os.WriteFile(filepath.Join("testdata", "hand.golden"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var ks []string
	for k := range kinds {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		t.Logf("%s: %d differ", k, kinds[k])
	}
	t.Logf("%d modules, %d differ", len(cs), diffs)
}
