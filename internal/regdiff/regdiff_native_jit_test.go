//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package regdiff

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// regNativeStats is what TestRegNativeCall found over the corpus.
type regNativeStats struct {
	// calls is how many calls were compared, of which compiled compiled
	// the function's code, native entered native code (osr of them by
	// on-stack replacement at a loop), limited made the runtimes anew;
	// varies is how many functions were not called, their answers a
	// clock's or Math.random's.
	calls, compiled, native, osr, limited, varies int
	// skipped is why a call was made as an ordinary call, by reason.
	skipped map[string]int
	// exits is what native code did, over every call.
	exits vm.RegNativeStats
}

// regNativeCheck runs the top-level functions of top, register code,
// natively with numbers as their arguments (vm.RegNativeCall), compiled at
// their first call and entered at their start or, every other call, by
// on-stack replacement, each answer compared with an ordinary call's in a
// second runtime that has made the same calls before it. Where parts is
// more than 1, it runs the part-th of that many runs of consecutive
// functions, as regLowerCheck does. Its caller holds the register form and
// tier on (regCorpusRun).
func regNativeCheck(t *testing.T, st *regNativeStats, top *bytecode.Function, part, parts int) {
	t.Helper()
	show := regLowerCompile(regLowerShow)
	if top == nil || show == nil {
		t.Logf("not compiled")
		return
	}
	named := map[string]*bytecode.Function{}
	seen := map[string]int{}
	for _, c := range top.Constants {
		if c.Kind == bytecode.ConstFunction && c.Fn != nil && c.Fn.Name != "" {
			seen[c.Fn.Name]++
			named[c.Fn.Name] = c.Fn
		}
	}
	func() {
		var rts [2]*vm.Runtime
		var before vm.RegNativeStats
		fresh := func() bool {
			if rts[0] != nil {
				st.exits = regNativeAdd(st.exits, rts[0].RegNativeStats(), before)
			}
			for i := range rts {
				if rts[i] != nil {
					rts[i].Close()
				}
				rts[i] = vm.New(vm.Config{MemoryLimit: 128 << 20, JIT: i == 0})
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				rts[i].SetContext(ctx)
				_, err := rts[i].Run(top)
				if err == nil {
					_, err = rts[i].Run(show)
				}
				cancel()
				rts[i].SetContext(context.Background())
				if err != nil {
					t.Logf("run: %v", err)
					return false
				}
			}
			before = rts[0].RegNativeStats()
			return true
		}
		if !fresh() {
			return
		}
		defer func() {
			st.exits = regNativeAdd(st.exits, rts[0].RegNativeStats(), before)
			for _, r := range rts {
				r.Close()
			}
		}()
		global := func(r *vm.Runtime, name string) vm.Value {
			v, _ := r.GetProp(vm.Obj(r.Global()), r.Intern(name))
			return v
		}
		text := func(r *vm.Runtime, v vm.Value, err error) string {
			var th *vm.Thrown
			switch {
			case errors.As(err, &th):
				s, e := r.Call(global(r, "__regShow"), vm.Undefined, []vm.Value{th.Value})
				if e != nil {
					return "threw, not shown"
				}
				return "threw " + s.String().Go()
			case err != nil:
				return "error " + err.Error()
			}
			s, err := r.Call(global(r, "__regShow"), vm.Undefined, []vm.Value{v})
			if err != nil {
				return "not shown: " + err.Error()
			}
			return s.String().Go()
		}
		names := slices.Sorted(func(yield func(string) bool) {
			for n := range named {
				if !yield(n) {
					return
				}
			}
		})
		for k, name := range names {
			if parts > 1 && k*parts/len(names) != part {
				continue
			}
			fn := named[name]
			if regCodeOf(fn) == nil || seen[name] != 1 || fn.ParamCount > 4 || fn.Kind != bytecode.KindNormal || !regLowerIdent.MatchString(name) {
				continue
			}
			if strings.Contains(fn.Text, "Date") || strings.Contains(fn.Text, "random") || strings.Contains(fn.Text, "performance") {
				st.varies++
				continue
			}
			same := true
			for _, r := range rts {
				ok, err := r.Call(global(r, "__regIs"), vm.Undefined, []vm.Value{global(r, name), vm.Str(vm.NewString(fn.Text))})
				same = same && err == nil && ok.Truthy()
			}
			if !same {
				continue
			}
			for j := range regLowerTuples() {
				args := make([]string, fn.ParamCount)
				vals := make([]vm.Value, fn.ParamCount)
				for i := range fn.ParamCount {
					x := regLowerArgs[(j*7+i*3+k)%len(regLowerArgs)]
					if j == 0 {
						x = 3
					}
					vals[i] = vm.Float(x)
					args[i] = strconv.FormatFloat(x, 'g', -1, 64)
				}
				var got, want string
				for i, r := range rts {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					r.SetContext(ctx)
					if i == 0 {
						res := r.RegNativeCall(global(r, name), vals, j%2 == 1)
						got = text(r, res.Value, res.Err)
						switch {
						case res.Skipped != "":
							why := res.Skipped
							if n := strings.Index(why, " at pc"); n > 0 {
								why = why[:n]
							}
							st.skipped[why]++
						case res.Entered >= 0:
							st.native++
						}
						if res.Compiled {
							st.compiled++
						}
						if res.OSR {
							st.osr++
						}
					} else {
						v, err := r.Call(global(r, name), vm.Undefined, vals)
						want = text(r, v, err)
					}
					cancel()
					r.SetContext(context.Background())
				}
				if limited := func(s string) bool {
					return strings.Contains(s, "memory limit") || strings.Contains(s, "deadline exceeded") || strings.Contains(s, "interrupted")
				}; limited(got) || limited(want) {
					st.limited++
					if !fresh() {
						return
					}
					break
				}
				st.calls++
				if got != want {
					t.Errorf("%s(%s): natively, %.300s; called, %.300s\n%s", name, strings.Join(args, ", "), got, want, regCodeOf(fn))
				}
			}
		}
	}()
}

// regCodeOf is fn's register code: the one place the test reads it, as
// the register form's representation of it changes.
func regCodeOf(fn *bytecode.Function) *bytecode.RegCode { return fn.RegCode() }

// regNativeAdd is sum plus what a runtime's counts went from before to now.
func regNativeAdd(sum, now, before vm.RegNativeStats) vm.RegNativeStats {
	sum.Entries += now.Entries - before.Entries
	sum.Hosts += now.Hosts - before.Hosts
	sum.Guards += now.Guards - before.Guards
	sum.Polls += now.Polls - before.Polls
	sum.Interpreted += now.Interpreted - before.Interpreted
	sum.Records += now.Records - before.Records
	sum.Materialized += now.Materialized - before.Materialized
	sum.GoCalls += now.GoCalls - before.GoCalls
	for k, n := range now.HostOps {
		if d := n - before.HostOps[k]; d != 0 {
			if sum.HostOps == nil {
				sum.HostOps = map[string]uint64{}
			}
			sum.HostOps[k] += d
		}
	}
	return sum
}

// TestRegNativeCall is the native code made from register code's
// differential test (docs/register-interpreter.md, Stage 3, 3a-3's step
// A): the register interpreter's corpus (the regDiff tests) compiled, and
// its top-level functions run with numbers as their arguments by native
// code compiled at their first call, from their start and by on-stack
// replacement (vm.RegNativeCall), each answer compared with an ordinary
// call's. QUICKJS_REGLOWER_SHARD and QUICKJS_REGLOWER_ARGS shard and size
// it as they do TestRegLowerEvaluate.
func TestRegNativeCall(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the register interpreter's whole corpus natively")
	}
	st := &regNativeStats{skipped: map[string]int{}}
	var mu sync.Mutex
	regCorpusRun(t, func(t *testing.T, top *bytecode.Function, part, parts int) {
		own := &regNativeStats{skipped: map[string]int{}}
		regNativeCheck(t, own, top, part, parts)
		mu.Lock()
		st.add(own)
		mu.Unlock()
	}, func() { regNativeReport(t, st) })
}

// add adds o's counts to s's.
func (s *regNativeStats) add(o *regNativeStats) {
	s.calls += o.calls
	s.compiled += o.compiled
	s.native += o.native
	s.osr += o.osr
	s.limited += o.limited
	s.varies += o.varies
	for k, n := range o.skipped {
		s.skipped[k] += n
	}
	s.exits = regNativeAdd(s.exits, o.exits, vm.RegNativeStats{})
}

// regNativeReport logs what TestRegNativeCall found, and fails it where
// nothing ran natively.
func regNativeReport(t *testing.T, st *regNativeStats) {
	type row struct {
		k string
		n int
	}
	var rows []row
	skipped := 0
	for k, n := range st.skipped {
		rows = append(rows, row{k, n})
		skipped += n
	}
	slices.SortFunc(rows, func(a, b row) int {
		if c := cmp.Compare(b.n, a.n); c != 0 {
			return c
		}
		return strings.Compare(a.k, b.k)
	})
	var out strings.Builder
	fmt.Fprintf(&out, "%d calls compared, all agreeing unless reported: %d compiled first, %d entered natively, %d of them by OSR, %d made as ordinary calls; the runtimes made anew %d times at a limit; %d functions of a clock's or Math.random's not called\n",
		st.calls, st.compiled, st.native, st.osr, skipped, st.limited, st.varies)
	e := st.exits
	fmt.Fprintf(&out, "native code: %d entries; exits: %d host, %d deoptimizations, %d polls; %d finished by runReg; %d records Go applied; %d objects made; %d calls of Go that ran script\n",
		e.Entries, e.Hosts, e.Guards, e.Polls, e.Interpreted, e.Records, e.Materialized, e.GoCalls)
	for i, r := range rows {
		if i < 15 {
			fmt.Fprintf(&out, "  %6d %s\n", r.n, r.k)
		}
	}
	ops := slices.SortedFunc(maps.Keys(e.HostOps), func(a, b string) int {
		if c := cmp.Compare(e.HostOps[b], e.HostOps[a]); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	fmt.Fprintf(&out, "host exits by register instruction:\n")
	for i, k := range ops {
		if i < 20 {
			fmt.Fprintf(&out, "  %10d %s\n", e.HostOps[k], k)
		}
	}
	t.Log("\n" + out.String())
	if st.native == 0 {
		t.Error("no call ran natively")
	}
}
