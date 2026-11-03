//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitAsked is a proxy of a build's feedback (jitFeedbackHook) that notes
// every question the build asks of it, and of the feedback of the callees
// it may inline (wrapSite), and what each changed of the runtime: the
// inventory phase 5's snapshot is designed from (D8), and its oracle.
type jitAsked struct {
	fb    *jitFeedback
	p     *ir.Program
	log   *jitAskLog
	depth int
}

// jitAskKey is a question: its method, its argument (a PC, mostly), and how
// many callees deep its feedback is.
type jitAskKey struct {
	method string
	arg    int64
	depth  int
}

// jitAskLog is what the builds asked.
type jitAskLog struct {
	mu       sync.Mutex
	builds   int
	asked    map[string]int
	distinct map[jitAskKey]int
	deep     map[string]int
	effects  map[string]map[string]int
	// ops are the program's instructions each method was asked at, by op.
	ops map[string]map[string]int
	// onNote, if set, is told each question and the counts around it.
	onNote func(method string, arg int64, depth int, before, after jitAskCounts)
}

func newJITAskLog() *jitAskLog {
	return &jitAskLog{asked: map[string]int{}, distinct: map[jitAskKey]int{}, deep: map[string]int{}, effects: map[string]map[string]int{},
		ops: map[string]map[string]int{}}
}

// jitAskCounts are what a question may change, counted.
type jitAskCounts map[string]int

func (q *jitAsked) counts() jitAskCounts {
	k := q.fb.keep()
	s := q.fb.r.jit
	c := jitAskCounts{
		"keep.shapes": len(k.shapes), "keep.holders": len(k.holders), "keep.cells": len(k.cells), "keep.inlined": len(k.inlined),
		"fb.links": len(q.fb.links), "fb.inlined": len(q.fb.inlined), "fb.pools": len(q.fb.pools),
		"fb.made": len(q.fb.made), "fb.wanted": len(q.fb.wanted), "fb.linked": len(q.fb.linked),
		"s.lowered": len(s.lowered), "s.links": len(s.links), "s.cache": len(s.cache),
		"s.compiled": int(s.compiled), "s.bgSubmitted": int(s.bgSubmitted),
	}
	if e := q.fb.e; e != nil {
		c["e.literals"], c["e.nativeCalls"], c["e.inlines"] = len(e.literals), len(e.nativeCalls), len(e.inlines)
		if e.cellsPool != nil {
			c["e.cellsPool"] = 1
		}
	}
	return c
}

func (q *jitAsked) note(method string, before jitAskCounts, arg int64, _ ...any) {
	l := q.log
	// Runtimes on goroutines of their own may share a log (QJS_JIT_SNAPOPS).
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked[method]++
	l.distinct[jitAskKey{method, arg, q.depth}]++
	if q.depth > 0 {
		l.deep[method]++
	}
	op := "-"
	if q.p != nil && arg >= 0 && arg < int64(len(q.p.Code)) && jitSnapAtPC[jitSnapMethodNamed(method)] {
		op = fmt.Sprint(int(q.p.Code[arg].Op))
	}
	if l.ops[method] == nil {
		l.ops[method] = map[string]int{}
	}
	l.ops[method][op]++
	// The question's effects: what of the counts changed across it.
	after := q.counts()
	if l.onNote != nil {
		l.onNote(method, arg, q.depth, before, after)
	}
	for name, n := range after {
		if before[name] != n {
			if l.effects[method] == nil {
				l.effects[method] = map[string]int{}
			}
			l.effects[method][name]++
		}
	}
}

// wrapSite has an inline site's feedback, a callee's, noted too.
func (q *jitAsked) wrapSite(s ssa.InlineSite) ssa.InlineSite {
	if f, ok := s.Feedback.(*jitFeedback); ok {
		s.Feedback = &jitAsked{fb: f, p: s.Program, log: q.log, depth: q.depth + 1}
	}
	return s
}

// jitAskingForTest has every build ask a proxy noting into log.
func jitAskingForTest(t *testing.T, log *jitAskLog) {
	old := jitFeedbackHook
	jitFeedbackHook = func(fb *jitFeedback, p *ir.Program) ssa.Feedback {
		log.builds++
		return &jitAsked{fb: fb, p: p, log: log}
	}
	t.Cleanup(func() { jitFeedbackHook = old })
}

// The inventory: what the builds of a workload ask, by method, how many of
// their questions are distinct and asked of callees' feedback, and what
// each method changed of the runtime: a server render, with React's builds
// in V8BENCH_SSR_DIR (v8bench -bench ssr -fetch -dir), and the V8 suite,
// in V8BENCH_V8_DIR (v8bench -fetch -dir), which inlines more.
func TestJITAskedInventory(t *testing.T) {
	for name, src := range jitWorkloadsForTest(t) {
		t.Run(name, func(t *testing.T) { jitInventoryForTest(t, src) })
	}
}

// The workloads, run with no proxy of the test's own: what the snapshot's
// oracle (QJS_JIT_SNAPCHECK) checks of them.
func TestJITWorkloads(t *testing.T) {
	for name, src := range jitWorkloadsForTest(t) {
		t.Run(name, func(t *testing.T) {
			r := New(Config{JIT: true})
			defer func() { r.Close(); r.ReleaseClosed() }()
			r.jitStress = jitStressConfig{}
			for _, s := range src {
				if _, err := r.Run(compileForTest(t, s)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// jitWorkloadsForTest are the scripts of a server render, with React's
// builds in V8BENCH_SSR_DIR (v8bench -bench ssr -fetch -dir), and of the V8
// suite, in V8BENCH_V8_DIR (v8bench -fetch -dir), which inlines more, by
// name: those whose directories are set.
func jitWorkloadsForTest(t *testing.T) map[string][]string {
	out := map[string][]string{}
	if dir := os.Getenv("V8BENCH_SSR_DIR"); dir != "" {
		func() {
			page, err := os.ReadFile("../v8bench/ssr.js")
			if err != nil {
				t.Fatal(err)
			}
			src := []string{"globalThis.self = globalThis;"}
			for _, f := range []string{"react.production.min.js", "react-dom-server-legacy.browser.production.min.js"} {
				b, err := os.ReadFile(filepath.Join(dir, f))
				if err != nil {
					t.Fatal(err)
				}
				src = append(src, string(b))
			}
			src = append(src, string(page))
			for range 20 {
				src = append(src, `__ssrRender(200).length`)
			}
			out["ssr"] = src
		}()
	}
	if dir := os.Getenv("V8BENCH_V8_DIR"); dir != "" {
		func() {
			var src []string
			for _, f := range []string{"base.js", "richards.js", "deltablue.js", "crypto.js", "raytrace.js",
				"earley-boyer.js", "regexp.js", "splay.js", "navier-stokes.js"} {
				b, err := os.ReadFile(filepath.Join(dir, f))
				if err != nil {
					t.Fatal(err)
				}
				src = append(src, string(b))
			}
			src = append(src, `for (const s of BenchmarkSuite.suites) for (const b of s.benchmarks) { b.Setup(); for (let i = 0; i < 3; i++) b.run(); b.TearDown(); }`)
			out["v8"] = src
		}()
	}
	return out
}

// jitInventoryForTest runs src's scripts in a runtime compiling on the
// spot, every build asking a noting proxy, and logs what they asked.
func jitInventoryForTest(t *testing.T, src []string) {
	log := newJITAskLog()
	jitAskingForTest(t, log)
	r := New(Config{JIT: true})
	defer func() { r.Close(); r.ReleaseClosed() }()
	r.jitStress = jitStressConfig{}
	for _, s := range src {
		if _, err := r.Run(compileForTest(t, s)); err != nil {
			t.Fatal(err)
		}
	}
	methods := make([]string, 0, len(log.asked))
	total, distinct := 0, map[string]int{}
	for k := range log.distinct {
		distinct[k.method]++
	}
	for m, n := range log.asked {
		methods = append(methods, m)
		total += n
	}
	sort.Slice(methods, func(i, j int) bool { return log.asked[methods[i]] > log.asked[methods[j]] })
	var b strings.Builder
	fmt.Fprintf(&b, "%d builds, %d questions, %d distinct\n", log.builds, total, len(log.distinct))
	fmt.Fprintf(&b, "%-16s %8s %8s %8s  effects\n", "method", "asked", "distinct", "callees'")
	for _, m := range methods {
		var eff []string
		for name, n := range log.effects[m] {
			eff = append(eff, fmt.Sprintf("%s:%d", name, n))
		}
		sort.Strings(eff)
		fmt.Fprintf(&b, "%-16s %8d %8d %8d  %s\n", m, log.asked[m], distinct[m], log.deep[m], strings.Join(eff, " "))
		var ops []string
		for op, n := range log.ops[m] {
			ops = append(ops, fmt.Sprintf("%s:%d", op, n))
		}
		sort.Strings(ops)
		fmt.Fprintf(&b, "%-16s   at %s\n", "", strings.Join(ops, " "))
	}
	t.Log("\n" + b.String())
}

// jitSnapMethodNamed is the method of name.
func jitSnapMethodNamed(name string) jitSnapMethod {
	for i, n := range jitSnapNames {
		if n == name {
			return jitSnapMethod(i)
		}
	}
	panic("no method " + name)
}

// jitWriteOps writes to file, a line for each, the methods of a PC log
// noted at each op, by its number.
func jitWriteOps(file string, log *jitAskLog) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# %d builds\n", log.builds)
	for m, ops := range log.ops {
		if !jitSnapAtPC[jitSnapMethodNamed(m)] {
			continue
		}
		for op, n := range ops {
			fmt.Fprintf(&b, "%s %s %d\n", m, op, n)
		}
	}
	return os.WriteFile(file, []byte(b.String()), 0o644)
}
