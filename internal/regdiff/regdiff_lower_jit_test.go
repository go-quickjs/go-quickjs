//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package regdiff

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/parser"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// regLowerStats is what TestRegLowerEvaluate found over the corpus.
type regLowerStats struct {
	functions, lowered, invalid int
	refused                     map[string]int
	// runs is how many calls were evaluated: by the evaluator alone, of
	// which returned ran to the end; and with the VM making what the
	// evaluator does not (vm.RegLowerCall), of which ranToEnd ran there
	// without leaving for runReg, compared how many were compared.
	runs, returned, generic, budget int
	ranToEnd, compared, limited     int
	// varies is how many functions were not called, their answers a
	// clock's or Math.random's.
	varies int
	// exits are where the evaluator alone left, resumed where it left
	// the frame to runReg with the VM, skipped why a call was not
	// evaluated, by register instruction or reason.
	exits, resumed, skipped map[string]int
}

// regLowerArgs are the numbers the corpus's functions are called with: the
// edges of int32 arithmetic, -0, NaN and the infinities among them.
var regLowerArgs = []float64{0, 1, 2, 3, 5, 7, 10, 31, 32, 100, -1, -7, 0.5, -0.5, 2147483647, -2147483648,
	4294967295, math.NaN(), math.Copysign(0, -1), 1e10, math.Inf(1), 12, 64, 255}

// regLowerIdent is a name a call can be written with.
var regLowerIdent = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// regLowerShow is the helpers both runtimes are given: what a value is,
// as text, and whether a global is the program's function of a source
// text.
const regLowerShow = `globalThis.__regShow = function (v) {
	var t = typeof v;
	if (t === "number") return Object.is(v, -0) ? "number -0" : "number " + String(v);
	if (t === "string" || t === "boolean" || t === "undefined" || t === "bigint") return t + " " + String(v);
	if (t === "symbol") return "symbol";
	if (t === "function") return "function " + v.name;
	if (v === null) return "null";
	try { return "object " + JSON.stringify(v) } catch (e) { return "object " + Object.prototype.toString.call(v) }
};
globalThis.__regIs = function (f, text) { return typeof f === "function" && Function.prototype.toString.call(f) === text };`

// regLowerCompile is src compiled, with the register form on, or nil
// where it does not compile.
func regLowerCompile(src string) *bytecode.Function {
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		return nil
	}
	top, err := compiler.Compile(prog, compiler.Options{Text: src})
	if err != nil {
		return nil
	}
	return top
}

// regLowerLimited reports whether s, a call's answer as text or its
// error, is a runtime's past its time or its memory.
func regLowerLimited(s string) bool {
	return strings.Contains(s, "memory limit") || strings.Contains(s, "deadline exceeded") || strings.Contains(s, "interrupted")
}

// regLowerCheck lowers every function of top's register code, validates
// each program, and runs those of its top-level functions with numbers as
// their arguments: through ir's evaluator alone, which leaves at what it
// cannot make; and in a runtime of the VM's with the VM making that
// (vm.RegLowerCall), each answer compared with an ordinary call's in a
// second runtime that has run the same calls before it. Where parts is
// more than 1, it runs the part-th of that many runs of consecutive
// functions, in the order of their names; all of them where parts is 0
// or 1. Its caller holds the register form and tier on for the process
// (TestRegLowerEvaluate).
func regLowerCheck(t *testing.T, st *regLowerStats, top *bytecode.Function, part, parts int) {
	t.Helper()
	show := regLowerCompile(regLowerShow)
	if top == nil || show == nil {
		t.Logf("not compiled")
		return
	}
	programs := map[*bytecode.Function]*ir.Program{}
	// Every part lowers every function, and the first part alone counts
	// them and reports an invalid one.
	lowering := st
	if part != 0 {
		lowering = newRegLowerStats()
	}
	var walk func(fn *bytecode.Function)
	walk = func(fn *bytecode.Function) {
		for _, c := range fn.Constants {
			if c.Kind != bytecode.ConstFunction || c.Fn == nil {
				continue
			}
			f := c.Fn
			walk(f)
			if f.RegCode() == nil {
				continue
			}
			lowering.functions++
			p, err := compile.LowerRegInline(f.RegCode(), f)
			if err != nil {
				var rf *compile.Refusal
				if errors.As(err, &rf) {
					lowering.refused[rf.Reason]++
				} else {
					lowering.refused[err.Error()]++
				}
				continue
			}
			if err := p.Validate(); err != nil {
				lowering.invalid++
				if lowering == st {
					t.Errorf("%s: invalid lowering: %v\n%s", f.Name, err, f.RegCode())
				}
				continue
			}
			lowering.lowered++
			programs[f] = p
		}
	}
	walk(top)
	named := map[string]*bytecode.Function{}
	seen := map[string]int{}
	for _, c := range top.Constants {
		if c.Kind == bytecode.ConstFunction && c.Fn != nil && c.Fn.Name != "" {
			seen[c.Fn.Name]++
			named[c.Fn.Name] = c.Fn
		}
	}
	func() {
		// Two runtimes, the same calls made in each: a's evaluated, b's
		// ordinary.
		var rts [2]*vm.Runtime
		// fresh makes the two runtimes anew, the program run in each.
		fresh := func() bool {
			for i := range rts {
				if rts[i] != nil {
					rts[i].Close()
				}
				rts[i] = vm.New(vm.Config{MemoryLimit: 128 << 20})
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
			return true
		}
		if !fresh() {
			return
		}
		defer func() {
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
				v, err = th.Value, nil
				s, e := r.Call(global(r, "__regShow"), vm.Undefined, []vm.Value{v})
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
			p := programs[fn]
			if p == nil || seen[name] != 1 || p.Params > 4 || fn.Kind != bytecode.KindNormal || !regLowerIdent.MatchString(name) {
				continue
			}
			if strings.Contains(fn.Text, "Date") || strings.Contains(fn.Text, "random") || strings.Contains(fn.Text, "performance") {
				// A clock's or Math.random's answer, which two runtimes give
				// apart, as the regDiff tests leave out a call whose answers
				// with the tier off vary.
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
				slots := make([]ir.Value, p.Locals)
				slots[0] = ir.Value{Kind: ir.Opaque}
				slots[1] = ir.Value{Kind: ir.Opaque, Bits: 1}
				args := make([]string, p.Params)
				vals := make([]vm.Value, p.Params)
				for i := range p.Params {
					x := regLowerArgs[(j*7+i*3+k)%len(regLowerArgs)]
					if j == 0 {
						x = 3
					}
					slots[p.ParamBase+i] = ir.Float(x)
					vals[i] = vm.Float(x)
					args[i] = strconv.FormatFloat(x, 'g', -1, 64)
				}
				for i := p.UpvalueBase; i < p.UpvalueBase+p.Upvalues; i++ {
					slots[i] = ir.Value{Kind: ir.Opaque, Bits: 2}
				}
				st.runs++
				// The evaluator alone, which leaves at what it cannot make.
				exit, err := p.Evaluate(slots, 0, 1<<18)
				switch {
				case errors.Is(err, ir.ErrGeneric):
					st.generic++
				case err != nil:
					t.Errorf("%s(%s): %v", name, strings.Join(args, ", "), err)
				case exit.Kind == ir.BudgetExit:
					st.budget++
				case exit.Kind != ir.Returned:
					st.exits[fn.RegCode().Code[exit.State.PC].Op().String()]++
				default:
					st.returned++
				}
				// With the VM: the call evaluated in one runtime, made in the
				// other, both given five seconds.
				var got, want string
				for i, r := range rts {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					r.SetContext(ctx)
					if i == 0 {
						res := r.RegLowerCall(global(r, name), vals, 1<<20)
						got = text(r, res.Value, res.Err)
						switch {
						case res.Skipped != "":
							st.skipped[res.Skipped]++
						case res.Resumed != "":
							st.resumed[res.Resumed]++
						default:
							st.ranToEnd++
						}
					} else {
						v, err := r.Call(global(r, name), vm.Undefined, vals)
						want = text(r, v, err)
					}
					cancel()
					r.SetContext(context.Background())
				}
				if regLowerLimited(got) || regLowerLimited(want) {
					// A runtime past its time or its memory, which the two
					// reach at different points, the evaluator's slower: the
					// runtimes part there, and are made anew for the next
					// function.
					st.limited++
					if !fresh() {
						return
					}
					break
				}
				st.compared++
				if got != want {
					t.Errorf("%s(%s): evaluated, %.300s; called, %.300s\n%s", name, strings.Join(args, ", "), got, want, fn.RegCode())
				}
			}
		}
	}()
}

// regLowerTuples is how many sets of arguments each function is called
// with: QUICKJS_REGLOWER_ARGS's number; 8, the full sweep, where
// QUICKJS_REGLOWER_FULL is 1; or a sample of 3, the first of them all 3s.
func regLowerTuples() int {
	var n int
	if _, err := fmt.Sscanf(os.Getenv("QUICKJS_REGLOWER_ARGS"), "%d", &n); err == nil && n > 0 {
		return n
	}
	if os.Getenv("QUICKJS_REGLOWER_FULL") == "1" {
		return 8
	}
	return 3
}

// regLowerChunks is how many runs of consecutive functions
// TestRegLowerEvaluate splits the micro-benchmarks' into, each run beside
// the others: the largest of the corpus's programs, most of the test's
// time. 1, the whole, where QUICKJS_REGLOWER_FULL is 1. The calls are
// those of the whole, each compared as there, but a part's program is
// compiled and run anew, and its functions have not been called as the
// whole's calls before them called them, so RegLowerCall finds no
// register frame for some calls it evaluates in the whole, and makes them
// plainly (a pure body's closure has no frame until pureCall has given up
// on it 64 times). Of the corpus's 3,520 calls with 3 sets of arguments,
// 258 ran to the end in the evaluator and 4 went on in runReg with the
// micro-benchmarks whole, 199 and 0 in 16 parts; a call of either is
// compared alike. The full sweep keeps the whole.
func regLowerChunks() int {
	if os.Getenv("QUICKJS_REGLOWER_FULL") == "1" {
		return 1
	}
	return 16
}

// add adds o's counts to s's.
func (s *regLowerStats) add(o *regLowerStats) {
	s.functions += o.functions
	s.lowered += o.lowered
	s.invalid += o.invalid
	s.runs += o.runs
	s.returned += o.returned
	s.generic += o.generic
	s.budget += o.budget
	s.ranToEnd += o.ranToEnd
	s.compared += o.compared
	s.limited += o.limited
	s.varies += o.varies
	for _, m := range [][2]map[string]int{{s.refused, o.refused}, {s.exits, o.exits}, {s.resumed, o.resumed}, {s.skipped, o.skipped}} {
		for k, n := range m[1] {
			m[0][k] += n
		}
	}
}

// newRegLowerStats is a regLowerStats with its maps made.
func newRegLowerStats() *regLowerStats {
	return &regLowerStats{refused: map[string]int{}, exits: map[string]int{}, resumed: map[string]int{}, skipped: map[string]int{}}
}

// regLowerShard reports whether the corpus test name is in the shard
// QUICKJS_REGLOWER_SHARD names, "k/n": the k-th of n, counting from 0, by
// a hash of the name; every test where it is unset. A full sweep holds
// the machine's heavy-work lock for many minutes, which shards share out.
func regLowerShard(name string) bool {
	spec := os.Getenv("QUICKJS_REGLOWER_SHARD")
	if spec == "" {
		return true
	}
	var k, n int
	if _, err := fmt.Sscanf(spec, "%d/%d", &k, &n); err != nil || n <= 0 {
		return true
	}
	h := 0
	for _, c := range name {
		h = h*31 + int(c)
	}
	return (h%n+n)%n == k
}

// TestRegLowerEvaluate is LowerReg's differential test: the register
// interpreter's corpus (the regDiff tests) compiled, every function's
// register code lowered and validated, and its top-level functions run
// with numbers as their arguments -- by ir's evaluator alone, and by the
// evaluator with the VM making what it does not and runReg going on where
// it leaves for good (vm.RegLowerCall) -- each answer of the latter
// compared with an ordinary call's.
//
// The corpus's cases run beside each other, each in runtimes of its own,
// and the micro-benchmarks' functions in regLowerChunks parts: the
// register form and tier are on for the process until every case is done,
// and the counts of each are added up under a lock.
func TestRegLowerEvaluate(t *testing.T) {
	if testing.Short() {
		t.Skip("lowers and evaluates the register interpreter's whole corpus")
	}
	compiler.SetRegisterForm(true)
	vm.SetRegTier(true)
	st := newRegLowerStats()
	var mu sync.Mutex
	check := func(t *testing.T, top *bytecode.Function, part, parts int) {
		own := newRegLowerStats()
		regLowerCheck(t, own, top, part, parts)
		mu.Lock()
		st.add(own)
		mu.Unlock()
	}
	regDiffHook = func(t *testing.T, src string, calls []string) { check(t, regLowerCompile(src), 0, 1) }
	t.Cleanup(func() {
		// After every case, the parallel ones' included.
		regDiffHook = nil
		env := bytecode.RegDefault()
		compiler.SetRegisterForm(env)
		vm.SetRegTier(env)
		regLowerReport(t, st)
	})
	// micro is the micro-benchmarks' case, in parts.
	micro := func(t *testing.T) {
		src, _ := regDiffMicro(t)
		n := regLowerChunks()
		for c := range n {
			t.Run(strconv.Itoa(c), func(t *testing.T) {
				t.Parallel()
				regDiffAlone.RLock()
				defer regDiffAlone.RUnlock()
				// Each part's program its own, as each case's is.
				check(t, regLowerCompile(src), c, n)
			})
		}
	}
	for _, tc := range []struct {
		name string
		f    func(*testing.T)
	}{
		// The longest first, to start first.
		{"Microbenchmarks", micro}, {"Int32", TestRegDiffInt32}, {"V8Suite", TestRegDiffV8Suite},
		{"Appends", TestRegDiffAppends}, {"ArgumentsForms", TestRegDiffArgumentsForms}, {"BlockFunctions", TestRegDiffBlockFunctions},
		{"CallTargets", TestRegDiffCallTargets}, {"ChainsAndClosures", TestRegDiffChainsAndClosures}, {"CharCodeAt", TestRegDiffCharCodeAt},
		{"Classes", TestRegDiffClasses}, {"Constructions", TestRegDiffConstructions}, {"ElemWrites", TestRegDiffElemWrites},
		{"Eval", TestRegDiffEval}, {"EvalCode", TestRegDiffEvalCode}, {"FarJumps", TestRegDiffFarJumps}, {"FarNames", TestRegDiffFarNames},
		{"FarRegisters", TestRegDiffFarRegisters}, {"FinallyCopies", TestRegDiffFinallyCopies}, {"FinallyDeep", TestRegDiffFinallyDeep},
		{"ForOfIn", TestRegDiffForOfIn}, {"GeneratorSteps", TestRegDiffGeneratorSteps}, {"Generators", TestRegDiffGenerators},
		{"ImportCalls", TestRegDiffImportCalls}, {"LabelsSwitchDelete", TestRegDiffLabelsSwitchDelete},
		{"LeafRun", TestRegDiffLeafRun}, {"Length", TestRegDiffLength}, {"Literals", TestRegDiffLiterals}, {"LoopBindings", TestRegDiffLoopBindings},
		{"Names", TestRegDiffNames}, {"ObjectLiterals", TestRegDiffObjectLiterals},
		{"Patterns", TestRegDiffPatterns}, {"RegExpLiteralCalls", TestRegDiffRegExpLiteralCalls}, {"RegExpLiterals", TestRegDiffRegExpLiterals},
		{"RegExpWideConstants", TestRegDiffRegExpWideConstants}, {"RestArguments", TestRegDiffRestArguments}, {"Sites", TestRegDiffSites},
		{"StackSitesPast16Bits", TestRegDiffStackSitesPast16Bits}, {"StacksAndNames", TestRegDiffStacksAndNames}, {"Steps", TestRegDiffSteps},
		{"StrictGlobalStores", TestRegDiffStrictGlobalStores}, {"TailCalls", TestRegDiffTailCalls}, {"TopLevel", TestRegDiffTopLevel},
		{"Try", TestRegDiffTry}, {"Typed", TestRegDiffTyped}, {"Using", TestRegDiffUsing},
		{"WideForms", TestRegDiffWideForms}, {"With", TestRegDiffWith},
	} {
		if !regLowerShard(tc.name) {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			if !regLowerAlone[tc.name] {
				t.Parallel()
			}
			tc.f(t)
		})
	}
}

// regLowerAlone are the cases of regDiffAll, which sets regcomp.OnRefuse
// for the process and holds regDiffAlone to itself: TestRegLowerEvaluate
// runs them one after another before the others, which run beside each
// other, so that none waits for them nor they for it.
var regLowerAlone = map[string]bool{
	"CallTargets": true, "EvalCode": true, "FarJumps": true, "FinallyCopies": true,
	"FinallyDeep": true, "StackSitesPast16Bits": true, "WideForms": true,
}

// regLowerReport logs what TestRegLowerEvaluate found, and fails it where
// nothing was compared.
func regLowerReport(t *testing.T, st *regLowerStats) {
	type row struct {
		k string
		n int
	}
	sorted := func(m map[string]int) []row {
		var rows []row
		for k, n := range m {
			rows = append(rows, row{k, n})
		}
		slices.SortFunc(rows, func(a, b row) int {
			if c := cmp.Compare(b.n, a.n); c != 0 {
				return c
			}
			return strings.Compare(a.k, b.k)
		})
		return rows
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%d functions: %d lowered, %d refused, %d invalid\n", st.functions, st.lowered, st.functions-st.lowered-st.invalid, st.invalid)
	for _, r := range sorted(st.refused) {
		fmt.Fprintf(&out, "  %6d refused: %s\n", r.n, r.k)
	}
	fmt.Fprintf(&out, "%d calls. The evaluator alone: %d returned, %d out of budget, %d Go's after a commit; left at\n",
		st.runs, st.returned, st.budget, st.generic)
	for i, r := range sorted(st.exits) {
		if i < 15 {
			fmt.Fprintf(&out, "  %6d %s\n", r.n, r.k)
		}
	}
	resumed, skipped := 0, 0
	for _, n := range st.resumed {
		resumed += n
	}
	for _, n := range st.skipped {
		skipped += n
	}
	fmt.Fprintf(&out, "With the VM: %d compared, all agreeing unless reported; the runtimes made anew %d times at a limit; %d functions of a clock's or Math.random's not called; %d ran to the end in the evaluator, %d left for runReg, %d not evaluated; left at\n",
		st.compared, st.limited, st.varies, st.ranToEnd, resumed, skipped)
	for i, r := range sorted(st.resumed) {
		if i < 25 {
			fmt.Fprintf(&out, "  %6d %s\n", r.n, r.k)
		}
	}
	fmt.Fprintf(&out, "not evaluated:\n")
	for i, r := range sorted(st.skipped) {
		if i < 10 {
			fmt.Fprintf(&out, "  %6d %s\n", r.n, r.k)
		}
	}
	t.Log("\n" + out.String())
	if st.compared == 0 {
		t.Error("no evaluation was compared")
	}
}
