//go:build quickjs_jit && !android && !ios && amd64 && (linux || windows)

package engine

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/v8bench"
	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	wopt "github.com/go-quickjs/go-quickjs/wasm/internal/jit/opt"
	"github.com/go-quickjs/go-quickjs/wasm/internal/wast"
)

// corpusFunc is a function of the corpus TestOptCompileCorpus compiles.
type corpusFunc struct {
	key  string
	code *Code
	idx  uint32
	body int
	req  *wjit.Request
}

// TestOptCompileCorpus measures the optimizing tier's compiles: it compiles
// every defined function of the modules WASM_OPT_CORPUS names -- .wasm
// files, and "spec" for every module of the spec scripts (WASM_SPEC_DIR)
// -- once, then WASM_OPT_REPS-1 more times (default 4), and logs the
// repeated compiles' time per function and per byte of body, by phase,
// and what they allocated per function. WASM_OPT_HASHES names a file to
// write each function's code's hash to, a line each, and WASM_OPT_BASE one
// written so to compare with: the test fails where a function's code, or
// its refusal, differs. -cpuprofile and -memprofile profile the test,
// which is mostly the compiles.
//
//	WASM_OPT_CORPUS=/path/goprog.wasm,spec WASM_SPEC_DIR=/path/spec \
//	  go test -tags quickjs_jit ./wasm/internal/engine -run TestOptCompileCorpus -v
func TestOptCompileCorpus(t *testing.T) {
	corpus := os.Getenv("WASM_OPT_CORPUS")
	if corpus == "" {
		t.Skip("WASM_OPT_CORPUS is not set")
	}
	reps := 5
	if s := os.Getenv("WASM_OPT_REPS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("WASM_OPT_REPS=%q", s)
		}
		reps = n
	}
	var fns []*corpusFunc
	add := func(name string, bin []byte) {
		lim := decoder.CoreLimits()
		m, derr := decoder.Decode(bin, lim)
		if derr != nil {
			return
		}
		if derr := decoder.ValidateFunctions(m, lim); derr != nil {
			return
		}
		c := &Code{Module: m, fns: make([]atomic.Pointer[compiled], len(m.Functions))}
		for i := range m.Functions {
			if m.Functions[i].Imported {
				continue
			}
			cd := c.get(uint32(i))
			fns = append(fns, &corpusFunc{key: fmt.Sprintf("%s#%d", name, i), code: c, idx: uint32(i),
				body: int(m.Functions[i].Code.Len), req: request(c, uint32(i), cd, 0)})
		}
	}
	for _, src := range strings.Split(corpus, ",") {
		if src == "spec" {
			addSpec(t, add)
			continue
		}
		bin, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		add(filepath.Base(src), bin)
	}

	// The first compile of each: its hash, or its refusal.
	hashes := make(map[string]string, len(fns))
	var ok []*corpusFunc
	refused := map[string]int{}
	codeBytes := 0
	for _, f := range fns {
		code, err := wopt.Compile(f.req)
		if err != nil {
			r := err.(*wjit.Refusal)
			refused[r.Reason]++
			hashes[f.key] = "refused:" + r.Reason
			continue
		}
		ok = append(ok, f)
		codeBytes += len(code.Bytes)
		h := sha256.New()
		h.Write(code.Bytes)
		var w [24]byte
		binary.LittleEndian.PutUint64(w[0:], uint64(code.GoEntry))
		binary.LittleEndian.PutUint64(w[8:], uint64(code.Entry))
		binary.LittleEndian.PutUint64(w[16:], uint64(code.ReturnStub))
		h.Write(w[:])
		hashes[f.key] = hex.EncodeToString(h.Sum(nil)[:12])
	}
	body := 0
	for _, f := range ok {
		body += f.body
	}
	t.Logf("%d functions compiled (%d bytes of body, %d of code), %d refused: %v", len(ok), body, codeBytes, len(fns)-len(ok), refused)

	if out := os.Getenv("WASM_OPT_HASHES"); out != "" {
		keys := make([]string, 0, len(hashes))
		for k := range hashes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&sb, "%s %s\n", k, hashes[k])
		}
		if err := os.WriteFile(out, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if base := os.Getenv("WASM_OPT_BASE"); base != "" {
		fh, err := os.Open(base)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		same, differ := 0, 0
		for sc.Scan() {
			k, h, _ := strings.Cut(sc.Text(), " ")
			got, there := hashes[k]
			switch {
			case !there:
				differ++
				if differ <= 20 {
					t.Errorf("%s: in the base, not in the corpus", k)
				}
			case got != h:
				differ++
				if differ <= 20 {
					t.Errorf("%s: %s, the base's %s", k, got, h)
				}
			default:
				same++
			}
		}
		fh.Close()
		t.Logf("code identity: %d functions the same, %d different", same, differ)
	}
	if reps < 2 || len(ok) == 0 {
		return
	}

	// The repeated compiles, timed by phase.
	times := &wopt.Times{Clock: func() int64 { return int64(v8bench.Clock()) }}
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	wopt.Timing = times
	t0 := v8bench.Clock()
	for range reps - 1 {
		for _, f := range ok {
			if _, err := wopt.Compile(f.req); err != nil {
				wopt.Timing = nil
				t.Fatalf("%s: %v on a compile again", f.key, err)
			}
		}
	}
	elapsed := v8bench.Clock() - t0
	wopt.Timing = nil
	runtime.ReadMemStats(&m1)
	n := float64(len(ok) * (reps - 1))
	nb := float64(body * (reps - 1))
	var sum int64
	for _, tk := range times.Ticks {
		sum += tk
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d compiles: %.1f µs a function, %.0f ns a byte of body (phases %.1f µs); %.0f bytes and %.0f objects a function, %d collections\n",
		len(ok)*(reps-1), float64(elapsed.Nanoseconds())/n/1e3, float64(elapsed.Nanoseconds())/nb,
		float64(sum)/n/1e3, float64(m1.TotalAlloc-m0.TotalAlloc)/n, float64(m1.Mallocs-m0.Mallocs)/n, m1.NumGC-m0.NumGC)
	for p := wopt.Phase(0); p < wopt.NumPhases; p++ {
		tk := times.Ticks[p]
		fmt.Fprintf(&sb, "  %-9s %8.2f µs %5.1f%%\n", p, float64(tk)/n/1e3, 100*float64(tk)/float64(sum))
	}
	t.Log("\n" + sb.String())
}

// addSpec adds every module of the spec scripts in WASM_SPEC_DIR.
func addSpec(t *testing.T, add func(name string, bin []byte)) {
	dir := os.Getenv("WASM_SPEC_DIR")
	if dir == "" {
		t.Fatal("spec in WASM_OPT_CORPUS without WASM_SPEC_DIR")
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
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		cmds, err := wast.Parse(src)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(dir, file)
		for _, cmd := range cmds {
			c, ok := cmd.(*wast.ModuleCommand)
			if !ok {
				continue
			}
			b, err := c.Module.Binary()
			if err != nil {
				continue
			}
			add(fmt.Sprintf("%s:%d", filepath.ToSlash(rel), c.Line()), b)
		}
	}
}
