//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestJITCompileCorpus compiles, as the JIT compiles them, the functions
// of Octane's benchmarks after a warm-up in the existing tiers -- each
// function's code for Go to enter and, where that is refused, for native
// callers -- and reports how long each phase of a compile took (the SSA
// build, its optimization, the backend) and what it allocated, per compile:
// what a change to the compiler's speed is measured by. It writes a hash
// of each function's code, its bytes and its descriptions, to the file
// QUICKJS_JIT_CORPUS_OUT names, and compares them with those of the file
// QUICKJS_JIT_CORPUS_WANT names: a change that is to make the compiler
// faster, and nothing else, keeps every hash. Addresses in the code -- of
// objects, shapes, descriptions, the runtime's own -- differ from one
// process to the next, and are hashed by the order each first appears in.
//
// QUICKJS_JIT_CORPUS is the directory of Octane's files (v8bench -bench
// octane -fetch); QUICKJS_JIT_CORPUS_SUITES the benchmarks, by name, or
// all of them; QUICKJS_JIT_CORPUS_RUNS how many times to compile them for
// the times, the fastest taken (5).
func TestJITCompileCorpus(t *testing.T) {
	dir := os.Getenv("QUICKJS_JIT_CORPUS")
	if dir == "" {
		t.Skip("QUICKJS_JIT_CORPUS names no directory of Octane's files")
	}
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	files := []string{"base.js", "richards.js", "deltablue.js", "crypto.js", "raytrace.js",
		"earley-boyer.js", "regexp.js", "splay.js", "navier-stokes.js", "pdfjs.js",
		"box2d.js",
		"typescript.js", "typescript-input.js", "typescript-compiler.js"}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	// The existing tiers alone: the caches learn, and nothing is compiled.
	r.jitState().unavailable = true
	run := func(name, src string) {
		t.Helper()
		prog, err := parser.Parse(src, parser.Options{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		fn, err := compiler.Compile(prog, compiler.Options{Source: name, Text: src})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := r.Run(fn); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	run("host.js", `var print = function () {}, alert = function () {};`)
	for _, f := range files {
		src, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		run(f, string(src))
	}
	suites := os.Getenv("QUICKJS_JIT_CORPUS_SUITES")
	run("warmup.js", fmt.Sprintf(`(() => {
		const only = %q;
		for (const s of BenchmarkSuite.suites) {
			if (only !== "" && !(","+only+",").includes(","+s.name+",")) continue;
			if (BenchmarkSuite.ResetRNG) BenchmarkSuite.ResetRNG();
			for (const b of s.benchmarks) {
				b.Setup();
				for (let i = 0; i < 2; i++) b.run();
				b.TearDown();
			}
		}
	})()`, suites))
	// The functions the global object reaches, through properties, prototypes
	// and functions' own properties: what the warm-up ran is among them.
	run("collect.js", `var __corpus = (() => {
		const seen = new Set(), fns = [], todo = [globalThis];
		while (todo.length) {
			const o = todo.shift();
			if (o === null || (typeof o !== "object" && typeof o !== "function") || seen.has(o)) continue;
			seen.add(o);
			if (typeof o === "function") fns.push(o);
			let names;
			try { names = Object.getOwnPropertyNames(o); } catch (e) { continue; }
			for (const k of names) {
				let d;
				try { d = Object.getOwnPropertyDescriptor(o, k); } catch (e) { continue; }
				if (d && "value" in d) todo.push(d.value);
			}
			todo.push(Object.getPrototypeOf(o));
		}
		return fns;
	})();`)
	arr := r.global.getOwn(r.atoms.intern("__corpus")).value().Object()
	var cls []*closure
	seen := map[*bytecode.Function]bool{}
	for _, v := range arr.elemsOf() {
		if !v.IsObject() {
			continue
		}
		fd := v.Object().fn()
		if fd == nil || fd.closure == nil {
			continue
		}
		cl := fd.closure
		fn := cl.fn
		if seen[fn] || fn.TopLevel || fn.Script == nil || fn.Script.Name == "collect.js" {
			continue
		}
		seen[fn] = true
		cls = append(cls, cl)
	}
	slices.SortFunc(cls, func(a, b *closure) int {
		return cmp.Or(strings.Compare(a.fn.Script.Name, b.fn.Script.Name), cmp.Compare(a.fn.Start, b.fn.Start))
	})
	// The programs, lowered as the JIT lowers them, and the calls each
	// makes, decided as a first compile decides them (jitSeedCalls).
	type item struct {
		name string
		cl   *closure
		p    *ir.Program
		e    *jitEntry
	}
	var items []item
	for _, cl := range cls {
		fn := cl.fn
		j := jitFunctionOf(fn)
		name := fmt.Sprintf("%s:%d:%s", j.Script, j.Line, j.Name)
		for _, kind := range []jitLowerKind{jitLowerSSA, jitLowerInline} {
			p, err := r.jitLower(fn, kind)
			if err != nil {
				continue
			}
			e := &jitEntry{ssaCallee: kind == jitLowerInline}
			r.jit.linkFor(fn, e)
			r.jitSeedCalls(cl, e, p)
			items = append(items, item{name + "/" + strconv.Itoa(int(kind)), cl, p, e})
			break
		}
	}
	if len(items) == 0 {
		t.Fatal("no function to compile")
	}
	// The hashes, and the times.
	runs := 5
	if n, err := strconv.Atoi(os.Getenv("QUICKJS_JIT_CORPUS_RUNS")); err == nil && n > 3 {
		runs = n
	}
	hashes := make([]string, len(items))
	// The first compile may decide what the next ones take as decided -- a
	// callee's link made -- as a compile of it again would: the second is
	// hashed, and the third checked.
	for run := range 3 {
		for i, it := range items {
			h, err := r.jitCorpusCompile(it.cl, it.p, it.e, run > 0)
			if err != nil {
				h = "refused: " + err.Error()
			}
			switch run {
			case 1:
				hashes[i] = h
			case 2:
				if h != hashes[i] {
					t.Fatalf("%s compiled to other code the second time", it.name)
				}
			}
		}
	}
	// The times: of every compile's first phase, its first two, all three,
	// each the fastest of the runs -- a clock read around each compile's
	// phases had read every few hundred microseconds on Windows -- and a
	// phase's the difference.
	var best [3]time.Duration
	for run := range runs {
		for k := range 3 {
			t0 := time.Now()
			for _, it := range items {
				r.jitCorpusPhases(it.cl, it.p, it.e, k+1)
			}
			if d := time.Since(t0); run == 0 || d < best[k] {
				best[k] = d
			}
		}
	}
	best[2] -= best[1]
	best[1] -= best[0]
	// What each phase allocates, from a run of the phases before it and one
	// with it.
	var allocs [3]uint64
	var bytes [3]uint64
	for k := range 3 {
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		for _, it := range items {
			r.jitCorpusPhases(it.cl, it.p, it.e, k+1)
		}
		runtime.ReadMemStats(&m1)
		allocs[k], bytes[k] = m1.Mallocs-m0.Mallocs, m1.TotalAlloc-m0.TotalAlloc
	}
	n := float64(len(items))
	names := [3]string{"build", "optimize", "backend"}
	var total time.Duration
	for k := range 3 {
		a, b := allocs[k], bytes[k]
		if k > 0 {
			a, b = a-allocs[k-1], b-bytes[k-1]
		}
		total += best[k]
		t.Logf("%-8s %8.1f µs %8.1f allocs %9.0f bytes per compile", names[k], float64(best[k].Microseconds())/n, float64(a)/n, float64(b)/n)
	}
	t.Logf("%d compiles, %.1f µs each, %.1f allocs, %.0f bytes", len(items), float64(total.Microseconds())/n, float64(allocs[2])/n, float64(bytes[2])/n)
	if out := os.Getenv("QUICKJS_JIT_CORPUS_OUT"); out != "" {
		var sb strings.Builder
		for i, it := range items {
			fmt.Fprintf(&sb, "%s %s\n", it.name, hashes[i])
		}
		if err := os.WriteFile(out, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := os.Getenv("QUICKJS_JIT_CORPUS_WANT"); want != "" {
		// Files of the build compared with, separated as a path list, each
		// from a process of its own: a few of a warm-up's caches end as
		// the collector's timing has them -- some of Typescript's -- and
		// the compiles they differ in are not compared.
		var files [][]string
		for _, w := range filepath.SplitList(want) {
			b, err := os.ReadFile(w)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) != len(items) {
				t.Fatalf("%d compiles, %d in %s", len(items), len(lines), w)
			}
			files = append(files, lines)
		}
		diff, unstable := 0, 0
		for i, it := range items {
			if slices.ContainsFunc(files, func(lines []string) bool { return lines[i] != files[0][i] }) {
				// Compiled to other code by the build compared with, from
				// one process to the next: what its warm-up left differs.
				unstable++
				continue
			}
			if l := it.name + " " + hashes[i]; l != files[0][i] {
				if diff++; diff <= 10 {
					t.Errorf("got %s, want %s", l, files[0][i])
				}
			}
		}
		t.Logf("%d compiles the same, %d differ, %d not the same from one process to the next", len(items)-diff-unstable, diff, unstable)
		if diff != 0 {
			t.Fatalf("%d of %d compiles differ", diff, len(items))
		}
	}
}

// jitCorpusCompile compiles p, cl's function's program, as compileSSA
// does, without placing its code, and returns the code's hash, when
// hashing.
func (r *Runtime) jitCorpusCompile(cl *closure, p *ir.Program, e *jitEntry, hashing bool) (string, error) {
	s := r.jit
	defer s.ssaWork.Rewind()
	defer s.mirWork.Rewind()
	f, err := jitCorpusBuild(r, cl, p, e)
	if err != nil {
		return "", err
	}
	ssa.Optimize(f)
	mc, err := mir.CompileIn(&s.mirWork, f, jitEncoding)
	if err != nil || !hashing {
		return "", err
	}
	return jitCodeHash(mc), nil
}

// jitCorpusPhases runs the first phases of jitCorpusCompile's.
func (r *Runtime) jitCorpusPhases(cl *closure, p *ir.Program, e *jitEntry, phases int) {
	s := r.jit
	defer s.ssaWork.Rewind()
	defer s.mirWork.Rewind()
	f, err := jitCorpusBuild(r, cl, p, e)
	if err != nil || phases < 2 {
		return
	}
	ssa.Optimize(f)
	if phases < 3 {
		return
	}
	mir.CompileIn(&s.mirWork, f, jitEncoding)
}

// jitCorpusBuild is compileSSAKeeping's build.
func jitCorpusBuild(r *Runtime, cl *closure, p *ir.Program, e *jitEntry) (*ssa.Func, error) {
	fn := cl.fn
	fb := &jitFeedback{r: r, fn: fn, cl: cl, e: e, prog: p}
	f, err := ssa.BuildIn(&r.jit.ssaWork, p, fb)
	if err != nil {
		return nil, err
	}
	f.FrameLocals = fn.LocalCount
	f.Params, f.StartOnly = fn.ParamCount, jitcompile.StartOnly(fn)
	f.CallEntry = !fn.TopLevel && !fn.IsModule && !fn.Async && !fn.Generator && !isGeneratorTemplate(fn)
	if p.This {
		f.ThisSlot = fn.LocalCount + len(fn.Upvalues)
	}
	return f, nil
}

// jitCodeHash hashes what the backend made: its code, every word in it
// that may be an address taken by the order it first appears in, and what
// describes it, its addresses so.
func jitCodeHash(mc *mir.Code) string {
	h := sha256.New()
	ids := map[uint64]int{}
	id := func(v uint64) uint64 {
		if v < 1<<32 || v >= 1<<47 {
			return v
		}
		n, ok := ids[v]
		if !ok {
			n = len(ids)
			ids[v] = n
		}
		return uint64(n) | 0xadd<<48
	}
	// The instructions, a 64-bit immediate's address (amd64's movabs) by id;
	// past them the constants they load, of eight bytes each (movImm), the
	// addresses among them so.
	b := mc.Bytes
	for i := 0; i < mc.Text; {
		if i+10 <= mc.Text && b[i]&0xfe == 0x48 && b[i+1] >= 0xb8 && b[i+1] <= 0xbf {
			if v := binary.LittleEndian.Uint64(b[i+2:]); v >= 1<<32 && v < 1<<47 {
				h.Write(b[i : i+2])
				binary.Write(h, binary.LittleEndian, id(v))
				i += 10
				continue
			}
		}
		h.Write(b[i : i+1])
		i++
	}
	for i := mc.Text; i < len(b); i += 8 {
		if i+8 > len(b) {
			h.Write(b[i:])
			break
		}
		binary.Write(h, binary.LittleEndian, id(binary.LittleEndian.Uint64(b[i:])))
	}
	entries := make([]int, 0, len(mc.Entries))
	for pc := range mc.Entries {
		entries = append(entries, pc)
	}
	slices.Sort(entries)
	for _, pc := range entries {
		fmt.Fprintf(h, "e%d:%d,", pc, mc.Entries[pc])
	}
	fmt.Fprintf(h, "c%d r%d t%d refs%d;", mc.CallEntry, mc.Resume, mc.Text, mc.CallRefs)
	seen := map[uintptr]bool{}
	jitHashValue(h, reflect.ValueOf(mc.Calls), id, seen)
	jitHashValue(h, reflect.ValueOf(mc.GoCalls), id, seen)
	jitHashValue(h, reflect.ValueOf(mc.Exits), id, seen)
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// jitHashValue hashes v, its pointers by what they point to, once, and
// its words that may be addresses by id.
func jitHashValue(h hash.Hash, v reflect.Value, id func(uint64) uint64, seen map[uintptr]bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			h.Write([]byte("nil"))
			return
		}
		p := v.Pointer()
		fmt.Fprintf(h, "p%d", id(uint64(p)))
		if seen[p] {
			return
		}
		seen[p] = true
		jitHashValue(h, v.Elem(), id, seen)
	case reflect.Slice, reflect.Array:
		fmt.Fprintf(h, "[%d", v.Len())
		for i := range v.Len() {
			jitHashValue(h, v.Index(i), id, seen)
		}
		h.Write([]byte("]"))
	case reflect.Struct:
		h.Write([]byte("{"))
		for i := range v.NumField() {
			jitHashValue(h, v.Field(i), id, seen)
		}
		h.Write([]byte("}"))
	case reflect.Map:
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
		for _, k := range keys {
			jitHashValue(h, k, id, seen)
			jitHashValue(h, v.MapIndex(k), id, seen)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fmt.Fprintf(h, "%d,", v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		fmt.Fprintf(h, "%d,", id(v.Uint()))
	case reflect.Bool:
		fmt.Fprintf(h, "%t,", v.Bool())
	case reflect.Float32, reflect.Float64:
		fmt.Fprintf(h, "%v,", v.Float())
	case reflect.String:
		fmt.Fprintf(h, "%q,", v.String())
	case reflect.Interface:
		if v.IsNil() {
			h.Write([]byte("nil"))
			return
		}
		jitHashValue(h, v.Elem(), id, seen)
	case reflect.Invalid:
		h.Write([]byte("-"))
	default:
		panic(errors.New("jitHashValue: " + v.Kind().String()))
	}
}
