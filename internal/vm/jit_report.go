//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"cmp"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// JITFunction is what the JIT did with one function, as JITReport lists
// it: what a run's coverage is made of. Script and Line say where the
// function is; Status is native (its code runs), slow (it has code, which
// Go stopped entering: its exits cost more than it saved), invalid (its
// code was invalidated by a deoptimization), refused (the JIT does not
// compile it, for Reason) or skipped (never tried, for Reason).
//
// Native counts the times its code ran -- Go's entries, at its start or
// into a loop, and native callers' calls -- and Declined the calls the
// interpreter made without entering it, which a slow function's are;
// Hosts, Guards and Ended count its exits: to Go for an operation, at a
// failed guard, and finished in the interpreter; GoCalls the calls of Go
// it made; Work the work done natively, in instructions, as the JIT
// estimates it; and Reopts the times it was compiled again for what it
// learned.
type JITFunction struct {
	Name, Script                                          string
	Line                                                  int
	Status                                                string
	Reason                                                string
	Native, Declined, Hosts, Guards, Ended, GoCalls, Work uint64
	Reopts                                                int
}

// JITCodeHook, if set, is called with each function's native code as it
// is placed: the function, the code's instructions where they run, and
// what the backend made them from (mir.Code's Marks and Listing, while
// mir.Annotate is set) -- for a profiler of native code, which maps a
// sample's address to an instruction. Set it before any runtime compiles;
// it is called on the runtime's goroutine.
var JITCodeHook func(f JITFunction, text []byte, code *mir.Code)

// JITCrossHook, if set, is called for each compile JITCompileRepeat
// repeats with what compiling it for arm64 made, on any machine: the code
// is not run, which is how a change to arm64's backend is compared where
// it cannot run. Set it before any runtime compiles.
var JITCrossHook func(f JITFunction, code *mir.Code, err error)

// JITHoldClocks stops the clocks the JIT's cache ages code by: what a run
// compiles, and with what, then depends on the run alone if the collector
// is off, which ends weak references' part in it, and two builds' code
// can be compared compile by compile. Call it before any runtime compiles,
// and restore, which it returns, after the last is done: the clocks run
// again.
func JITHoldClocks() (restore func()) {
	epoch, flush := jitEpochNow, jitFlushNow
	jitEpochNow = func() uint64 { return 0 }
	jitFlushNow = func() uint64 { return 0 }
	return func() { jitEpochNow, jitFlushNow = epoch, flush }
}

// SetJITReport has the JIT note why its runs ended in the interpreter and
// what its calls of Go made (RegNativeStats). What it compiles or refuses,
// which JITReport lists, it keeps anyway.
func (r *Runtime) SetJITReport(on bool) {
	if r.jitEnabled {
		r.jitState().reporting = on
	}
}

// JITReport lists what the JIT did with each function it has met that is
// still alive, by script and line.
func (r *Runtime) JITReport() []JITFunction {
	s := r.jit
	if s == nil {
		return nil
	}
	var out []JITFunction
	for k, e := range s.regCache {
		// Register code's, its name marked so: what it compiled, what it
		// refused and why.
		fn := k.Value()
		if fn == nil {
			continue
		}
		f := jitFunctionOf(fn)
		f.Name += " [reg]"
		switch {
		case e.code != nil && e.invalid:
			f.Status = "invalid"
		case e.code != nil && e.life != nil && e.life.entrySlow:
			f.Status = "slow"
		case e.code != nil:
			f.Status = "native"
		default:
			f.Status, f.Reason = "refused", e.refusal
		}
		if l := e.life; l != nil {
			// What the function's register code did, over its compiles:
			// Go's entries at its start and into its loops, native
			// callers' calls through its link; its exits; its compiles
			// again.
			st := &l.ssaStats
			f.Native = l.starts + st.entries
			if link := s.regLinks[k]; link != nil {
				f.Native += link.in
			}
			f.Hosts, f.Guards, f.Ended, f.Declined = st.hosts, st.guards, st.ended, l.slowCalls
			f.GoCalls, f.Work, f.Reopts = st.goCalls, st.work, int(l.reopts)
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b JITFunction) int {
		return cmp.Or(cmp.Compare(a.Script, b.Script), cmp.Compare(a.Line, b.Line), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// jitFunctionOf is a report's line for fn, where it is.
func jitFunctionOf(fn *bytecode.Function) JITFunction {
	f := JITFunction{Name: fn.Name}
	if f.Name == "" {
		f.Name = "(anonymous)"
	}
	if fn.TopLevel {
		f.Name = "(top level)"
	}
	if fn.Script != nil {
		f.Script = fn.Script.Name
		line, _ := fn.Script.Position(fn.Start)
		f.Line = int(line)
	}
	return f
}
