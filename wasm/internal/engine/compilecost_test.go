//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ijit "github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/wast"
)

// TestCompileCost times what a module costs before its code runs, phase by
// phase, over a corpus: decoding, validating, the interpreter's compile of
// every defined function, the record of each one's frame from validation's
// bound, the baseline JIT's compile of each in that frame and the
// placement of its code. WASM_COMPILE_COST names the corpus, a
// comma-separated list of .wasm files and "spec" (every module of
// WASM_SPEC_DIR's core and legacy scripts); WASM_COMPILE_N is how many
// times each phase runs (3), its best time reported; WASM_COMPILE_HASHES
// names a file each function's baseline code is written to as a hash, so
// that two builds' files compare the code they make (the addresses the
// code is given -- the write barrier's, the i31 mark's -- are fixed for
// it).
func TestCompileCost(t *testing.T) {
	spec := os.Getenv("WASM_COMPILE_COST")
	if spec == "" {
		t.Skip("WASM_COMPILE_COST is not set")
	}
	n := 3
	if s := os.Getenv("WASM_COMPILE_N"); s != "" {
		var err error
		if n, err = strconv.Atoi(s); err != nil || n < 1 {
			t.Fatalf("WASM_COMPILE_N=%q", s)
		}
	}
	type input struct {
		name string
		bin  []byte
	}
	var inputs []input
	for _, item := range strings.Split(spec, ",") {
		switch item = strings.TrimSpace(item); item {
		case "":
		case "spec":
			for _, in := range specModules(t) {
				inputs = append(inputs, input{in.name, in.bin})
			}
		default:
			b, err := os.ReadFile(item)
			if err != nil {
				t.Fatal(err)
			}
			inputs = append(inputs, input{filepath.Base(item), b})
		}
	}

	// Fixed addresses, so that the code is the same from one build to the
	// next; it is never run.
	wb, sr, lay := jitWriteBarrier, jitStoreRef, jitLayout
	fixed := *jitLayout
	fixed.I31Mark = 0x1_0000_1000
	jitWriteBarrier, jitStoreRef, jitLayout = 0x1_0000_2000, 0x1_0000_3000, &fixed
	t.Cleanup(func() { jitWriteBarrier, jitStoreRef, jitLayout = wb, sr, lay })

	lim := decoder.CoreLimits()
	var bodyBytes, funcs int
	mods := make([]*module.Module, len(inputs))
	for i, in := range inputs {
		m, err := decoder.Decode(in.bin, lim)
		if err != nil {
			t.Fatalf("%s: %v", in.name, err)
		}
		if err := decoder.ValidateFunctions(m, lim); err != nil {
			t.Fatalf("%s: %v", in.name, err)
		}
		mods[i] = m
		for _, f := range m.Functions {
			if !f.Imported {
				funcs++
				bodyBytes += int(f.Code.Len)
			}
		}
	}

	type phase struct {
		name          string
		best          time.Duration
		bytes, allocs uint64
	}
	var phases []*phase
	measure := func(name string, run func()) {
		p := &phase{name: name, best: -1}
		phases = append(phases, p)
		for range n {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			run()
			d := time.Since(start)
			runtime.ReadMemStats(&after)
			if p.best < 0 || d < p.best {
				p.best = d
			}
			p.bytes, p.allocs = after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs
		}
	}

	measure("decode", func() {
		for _, in := range inputs {
			if _, err := decoder.Decode(in.bin, lim); err != nil {
				t.Fatal(err)
			}
		}
	})
	measure("validate", func() {
		for _, m := range mods {
			if err := decoder.ValidateFunctions(m, lim); err != nil {
				t.Fatal(err)
			}
		}
	})
	measure("interp compile", func() {
		for _, m := range mods {
			c := &Code{Module: m, fns: make([]atomic.Pointer[compiled], len(m.Functions))}
			for fn, f := range m.Functions {
				if !f.Imported {
					c.fns[fn].Store(compileFunction(c, uint32(fn)))
				}
			}
		}
	})
	// The records native code runs a function in from its first call,
	// which the baseline compiles from: their frames validation's bounds.
	codes := make([]*Code, len(mods))
	measure("bound frame", func() {
		for i, m := range mods {
			c := &Code{Module: m, fns: make([]atomic.Pointer[compiled], len(m.Functions))}
			for fn, f := range m.Functions {
				if !f.Imported {
					c.fns[fn].Store(c.boundRecord(uint32(fn)))
				}
			}
			codes[i] = c
		}
	})
	type result struct {
		code   *wjit.Code
		reason string
	}
	results := make([][]result, len(mods))
	measure("jit compile", func() {
		for i, c := range codes {
			rs := make([]result, len(c.Module.Functions))
			for fn, f := range c.Module.Functions {
				if f.Imported {
					continue
				}
				code, err := compileNative(c, uint32(fn), c.get(uint32(fn)), 0)
				if err != nil {
					reason := "internal"
					if r, ok := err.(*wjit.Refusal); ok {
						reason = r.Reason
					}
					rs[fn].reason = reason
					continue
				}
				rs[fn].code = code
			}
			results[i] = rs
		}
	})
	var codeBytes, compiled int
	refused := map[string]int{}
	for _, rs := range results {
		for _, r := range rs {
			switch {
			case r.code != nil:
				compiled++
				codeBytes += len(r.code.Bytes)
			case r.reason != "":
				refused[r.reason]++
			}
		}
	}
	measure("place", func() {
		a := ijit.NewArena()
		var raws []*ijit.RawCode
		for _, rs := range results {
			for _, r := range rs {
				if r.code == nil {
					continue
				}
				raw, err := ijit.PlaceRaw(a, r.code.Bytes)
				if err != nil {
					t.Fatal(err)
				}
				raws = append(raws, raw)
			}
		}
		for _, r := range raws {
			r.Close()
		}
	})

	t.Logf("%d modules, %d functions (%d body bytes); baseline compiled %d (%d code bytes), refused %v", len(inputs), funcs, bodyBytes, compiled, codeBytes, refused)
	t.Logf("%-16s %12s %12s %12s %12s", "phase", "total", "per fn", "B/fn", "allocs/fn")
	for _, p := range phases {
		per := funcs
		if p.name == "place" {
			per = compiled
		}
		t.Logf("%-16s %12v %10.2fµs %12.0f %12.1f", p.name, p.best.Round(time.Microsecond),
			float64(p.best)/float64(per)/1e3, float64(p.bytes)/float64(per), float64(p.allocs)/float64(per))
	}

	if out := os.Getenv("WASM_COMPILE_HASHES"); out != "" {
		f, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		w := bufio.NewWriter(f)
		for i, rs := range results {
			for fn, r := range rs {
				switch {
				case r.code != nil:
					fmt.Fprintf(w, "%s %d %x %d\n", inputs[i].name, fn, codeHash(r.code), len(r.code.Bytes))
				case r.reason != "":
					fmt.Fprintf(w, "%s %d refused %s\n", inputs[i].name, fn, r.reason)
				}
			}
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// codeHash is the first bytes of a hash of everything the compiler made of
// a function: its bytes, its entries, its loops' entries and its handlers.
func codeHash(c *wjit.Code) []byte {
	h := sha256.New()
	h.Write(c.Bytes)
	var w [8]byte
	word := func(v int) {
		binary.LittleEndian.PutUint64(w[:], uint64(v))
		h.Write(w[:])
	}
	word(c.GoEntry)
	word(c.Entry)
	word(c.ReturnStub)
	loops := make([]uint32, 0, len(c.Loops))
	for at := range c.Loops {
		loops = append(loops, at)
	}
	slices.Sort(loops)
	for _, at := range loops {
		word(int(at))
		word(c.Loops[at])
	}
	fmt.Fprintf(h, "%v", c.Handlers)
	return h.Sum(nil)[:8]
}

// specModule is a module of the spec scripts.
type specModule struct {
	name string
	bin  []byte
}

// specModules returns every module of WASM_SPEC_DIR's core and legacy
// scripts that decodes and validates, by file and line.
func specModules(t *testing.T) []specModule {
	dir := os.Getenv("WASM_SPEC_DIR")
	if dir == "" {
		t.Fatal("WASM_SPEC_DIR is not set")
	}
	var files []string
	for _, sub := range []string{"core", "legacy"} {
		filepath.WalkDir(filepath.Join(dir, "test", sub), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".wast") {
				files = append(files, p)
			}
			return nil
		})
	}
	sort.Strings(files)
	lim := decoder.CoreLimits()
	var out []specModule
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		cmds, err := wast.Parse(src)
		if err != nil {
			continue
		}
		for _, cmd := range cmds {
			c, ok := cmd.(*wast.ModuleCommand)
			if !ok {
				continue
			}
			b, err := c.Module.Binary()
			if err != nil {
				continue
			}
			m, derr := decoder.Decode(b, lim)
			if derr != nil || decoder.ValidateFunctions(m, lim) != nil {
				continue
			}
			out = append(out, specModule{fmt.Sprintf("%s:%d", filepath.Base(file), c.Line()), b})
		}
	}
	return out
}
