//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// jitBackgroundWork is what TestJITBackgroundRuntimes runs: micro-benchmarks
// of every kind of code -- properties, arrays and typed arrays, calls and
// closures, strings, regular expressions, BigInts, generators, iterators,
// exceptions, private fields, spreads, the callbacks of built-ins, sorts --
// each with a count small enough for a run under -race, large enough for
// its loops to be compiled. None reads the clock or Math.random.
var jitBackgroundWork = []struct {
	name string
	n    int
}{
	{"prop_read", 8192}, {"prop_write", 8192}, {"prop_update", 4096}, {"prop_create", 512},
	{"prop_delete", 128}, {"array_read", 4096}, {"array_write", 2048}, {"array_push", 32},
	{"array_pop", 16}, {"array_slice", 32}, {"typed_array_read", 2048}, {"arguments_read", 512},
	{"global_read", 8192}, {"local_destruct", 1024}, {"func_call", 4096}, {"func_closure_call", 4096},
	{"int_arith", 32}, {"float_arith", 16}, {"map_set_string", 2}, {"map_delete", 4},
	{"weak_map_set", 8}, {"array_for_in", 64}, {"array_for_of", 64}, {"regexp_ascii", 4},
	{"regexp_replace", 2}, {"string_build1", 8}, {"string_build2", 8}, {"int_to_string", 2048},
	{"float_toFixed", 1024}, {"string_to_float", 2048}, {"bigint64_arith", 16}, {"bigint256_arith", 8},
	{"generator_for_of", 64}, {"generator_next", 32}, {"generator_delegate", 16}, {"try_throw", 8192},
	{"try_finally", 8192}, {"iterator_for_of", 32}, {"object_for_in", 512}, {"closure_make", 1024},
	{"closure_callback", 512}, {"tail_mutual", 8192}, {"method_call", 4096}, {"string_switch_mixed", 4096},
	{"has_own_call_for_in", 512}, {"array_push_args5", 4}, {"private_field", 8192}, {"prop_in", 2048},
	{"object_spread", 512}, {"object_rest", 512}, {"spread_call", 512}, {"array_map", 64},
	{"array_filter", 64}, {"array_reduce", 64}, {"typed_forEach", 64}, {"typed_map", 64},
}

// jitBackgroundRuntimes is how many runtimes TestJITBackgroundRuntimes runs
// at once, and jitBackgroundRounds how many times each runs all the work.
const (
	jitBackgroundRuntimes = 4
	jitBackgroundRounds   = 4
)

// A compile in the background on a real schedule: several runtimes, on
// goroutines of their own, run the same code at once, sharing its bytecode
// and the process's workers, each compiling in the background with nothing
// scheduled -- a compile installed at whichever check finds its worker done,
// a worker still compiling when one comes, others' compiles queued before
// it -- where QJS_JIT_STRESS's background installs each a fixed number of
// checks after it was handed over, the same in every run. Each answer must
// be the interpreter's, and each runtime must have compiled in the
// background. CI runs it under -race.
func TestJITBackgroundRuntimes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := jitMicrobenchForTest(t)
	script := compileForTest(t, src)
	calls := make([]*bytecode.Function, len(jitBackgroundWork))
	for i, w := range jitBackgroundWork {
		calls[i] = compileForTest(t, fmt.Sprintf(
			"(function () { global_res = undefined; var n = %s(%d); var g = global_res; return String(n) + ':' + (typeof g === 'object' && g !== null ? JSON.stringify(g) : String(g)) })()",
			w.name, w.n))
	}
	// The interpreter's answers, twice: the work must give the same each
	// time for the runtimes' to be compared with them.
	want := make([]string, len(calls))
	for pass := 0; pass < 2; pass++ {
		r := New(Config{})
		if _, err := r.Run(script); err != nil {
			t.Fatal(err)
		}
		for i, c := range calls {
			v, err := r.Run(c)
			if err != nil {
				t.Fatalf("%s: %v", jitBackgroundWork[i].name, err)
			}
			got := v.String().Go()
			if pass == 1 && got != want[i] {
				t.Fatalf("%s gives %q, then %q: not deterministic", jitBackgroundWork[i].name, want[i], got)
			}
			want[i] = got
		}
		r.Close()
		r.ReleaseClosed()
	}
	var wg sync.WaitGroup
	errs := make(chan string, jitBackgroundRuntimes*len(calls)+jitBackgroundRuntimes)
	for k := 0; k < jitBackgroundRuntimes; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			r := New(Config{JIT: true})
			defer func() { r.Close(); r.ReleaseClosed() }()
			// Unscheduled, whatever QJS_JIT_STRESS says; half the runtimes
			// compile at a function's first call, half at the JIT's own
			// thresholds.
			r.SetJITBackground(true)
			r.jitStress = jitStressConfig{}
			if k%2 == 0 {
				r.jitCallThreshold, r.jitICThreshold = 1, 1
			}
			if _, err := r.Run(script); err != nil {
				errs <- fmt.Sprintf("runtime %d: %v", k, err)
				return
			}
			for round := 0; round < jitBackgroundRounds; round++ {
				// Each runtime starts the work at a place of its own.
				for j := range calls {
					i := (j + k*len(calls)/jitBackgroundRuntimes) % len(calls)
					v, err := r.Run(calls[i])
					if err != nil {
						errs <- fmt.Sprintf("runtime %d, %s: %v", k, jitBackgroundWork[i].name, err)
						continue
					}
					if got := v.String().Go(); got != want[i] {
						errs <- fmt.Sprintf("runtime %d, round %d, %s = %q, want the interpreter's %q", k, round, jitBackgroundWork[i].name, got, want[i])
					}
				}
				// Its compiles in flight finished, so that the next round
				// installs them, the other runtimes running meanwhile: on
				// one CPU, or one worker for them all, a runtime can
				// otherwise run all its work before a compile of its is done.
				if s := r.jit; s != nil {
					for _, j := range s.jobs {
						<-j.done
					}
					for _, l := range s.lowerings {
						<-l.done
					}
				}
			}
			if s := r.jit; s != nil {
				st := r.JITStats()
				t.Logf("runtime %d: submitted %d, discarded %d, missed %d, stale %d, compiled %d, entries %d, SSA entries %d", k, s.bgSubmitted, s.bgDiscarded, s.bgMissed, s.bgStale, s.compiled, st.Entries, st.SSAEntries)
			}
			if s := r.jit; s == nil || s.bgSubmitted == 0 || s.compiled == 0 || r.JITStats().Entries == 0 {
				errs <- fmt.Sprintf("runtime %d compiled or entered nothing in the background: %+v", k, r.JITStats())
			}
		}(k)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// jitMicrobenchForTest is QuickJS's micro-benchmarks and ours, as the
// v8bench runner's micro mode loads them: microbench.js without the call
// of its main that ends it, then microbench_extra.js.
func jitMicrobenchForTest(t *testing.T) string {
	t.Helper()
	base, err := os.ReadFile("../v8bench/microbench.js")
	if err != nil {
		t.Fatal(err)
	}
	extra, err := os.ReadFile("../v8bench/microbench_extra.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(base)
	end := strings.LastIndex(s, "\nif (typeof scriptArgs === \"undefined\")")
	if end < 0 {
		t.Fatal("microbench.js: no call of main to leave out")
	}
	return s[:end+1] + string(extra)
}
