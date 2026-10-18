//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"cmp"
	"fmt"
	"slices"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// JITFunction is what the JIT did with one function, as JITReport lists
// it: what a run's coverage is made of. Script and Line say where the
// function is; Status is native (its code runs), slow (it has code, which
// Go stopped entering: its exits cost more than it saved), refused (the
// JIT does not compile it, for Reason), deferred (out of the JIT's memory
// for now), warming (not compiled yet, its budget not spent) or skipped
// (never tried, for Reason).
//
// Native counts the times its code ran -- Go's entries, at its start or
// into a loop, and native callers' calls -- and Declined the calls the
// existing tiers made without entering it, which a slow function's are;
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

// SetJITReport has the JIT note, for JITReport, the functions it does not
// try to compile: too long, or too large for its memory. What it compiles
// or refuses it keeps anyway.
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
	for k, e := range s.cache {
		fn := k.Value()
		if fn == nil {
			continue
		}
		f := jitFunctionOf(fn)
		switch {
		case e.ssa != nil && e.entrySlow:
			f.Status = "slow"
		case e.ssa != nil:
			f.Status = "native"
		case e.deferred:
			f.Status = "deferred"
		case e.warming:
			f.Status = "warming"
		default:
			f.Status, f.Reason = "refused", e.refusal
		}
		st := &e.ssaStats
		f.Native, f.Declined = e.starts+st.entries+e.link.in, e.slowCalls
		f.Hosts, f.Guards, f.Ended = st.hosts, st.guards, st.ended
		f.GoCalls, f.Work, f.Reopts = st.goCalls, st.work, int(e.reopts)
		out = append(out, f)
	}
	for k, why := range s.skipped {
		fn := k.Value()
		if fn == nil || s.cache[k] != nil {
			continue
		}
		f := jitFunctionOf(fn)
		f.Status, f.Reason = "skipped", why
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

// jitSkipped notes, when reporting, that the JIT did not try to compile fn.
func (r *Runtime) jitSkipped(fn *bytecode.Function, format string, args ...any) {
	if s := r.jit; s != nil && s.reporting {
		if s.skipped == nil {
			s.skipped = map[weak.Pointer[bytecode.Function]]string{}
		}
		s.skipped[weak.Make(fn)] = fmt.Sprintf(format, args...)
	}
}
