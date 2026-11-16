package vm

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler/regcomp"
)

// The register tier's report (QJS_REGREPORT): the calls that cross between
// register code and the other tiers, by kind and by the function called,
// with why the backend left that function to the stack form, printed when a
// runtime closes.

var regReportOn = os.Getenv("QJS_REGREPORT") != ""

const (
	regCrossDirect = iota // callDirect -- a built-in's callback, a tree's call through it -- into register code
	regCrossEnter         // runFD or callTree into register code
	regCrossOut           // register code calling anything else
	regCrossNew           // register code constructing
	regCrossKinds
)

var regCrossNames = [regCrossKinds]string{
	"callDirect -> register (built-ins' callbacks)",
	"runFD/callTree -> register (tree and stack callers)",
	"register -> other tiers and built-ins",
	"register constructs (new)",
}

type regCrossKey struct {
	kind int
	fn   *bytecode.Function
	name string
}

var regCross struct {
	sync.Mutex
	n map[regCrossKey]int64
}

// regNoteCross counts a call crossing a tier's boundary, of the compiled
// function fn, or of the built-in or other callee name.
func regNoteCross(kind int, fn *bytecode.Function, name string) {
	regCross.Lock()
	if regCross.n == nil {
		regCross.n = map[regCrossKey]int64{}
	}
	regCross.n[regCrossKey{kind, fn, name}]++
	regCross.Unlock()
}

// regCrossOf counts a call from register code of callee, whatever it is.
func regCrossOf(kind int, callee Value) {
	if !callee.IsObject() {
		regNoteCross(kind, nil, "(not a function)")
		return
	}
	fd := callee.Object().fn()
	switch {
	case fd == nil:
		regNoteCross(kind, nil, "(proxy or not a function)")
	case fd.closure != nil:
		regNoteCross(kind, fd.closure.fn, "")
	case fd.bound:
		regNoteCross(kind, nil, "(bound function)")
	default:
		regNoteCross(kind, nil, "built-in "+fd.name)
	}
}

// RegReport prints the register tier's report, where QJS_REGREPORT asks
// for it, and clears it: what a runtime's Close calls.
func RegReport() {
	if regReportOn {
		regReportPrint()
	}
}

// regReportPrint prints and clears the report.
func regReportPrint() {
	regCross.Lock()
	n := regCross.n
	regCross.n = nil
	regCross.Unlock()
	if len(n) == 0 {
		return
	}
	var sb strings.Builder
	sb.WriteString("register tier report\n")
	type row struct {
		key regCrossKey
		n   int64
	}
	var totals [regCrossKinds]int64
	rows := make([]row, 0, len(n))
	for k, c := range n {
		totals[k.kind] += c
		rows = append(rows, row{k, c})
	}
	for k, name := range regCrossNames {
		fmt.Fprintf(&sb, "  %-52s %12d\n", name, totals[k])
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].n > rows[j].n })
	sb.WriteString("  the calls crossing most, by kind and callee:\n")
	for i, r := range rows {
		if i == 25 {
			break
		}
		what := r.key.name
		if fn := r.key.fn; fn != nil {
			what = fn.Name
			if what == "" {
				what = "<anonymous>"
			}
			if line := fn.Script; line != nil {
				l, _ := fn.Script.Position(fn.Start)
				what += fmt.Sprintf(" (line %d)", l)
			}
			if fn.RegCode() == nil {
				why := regcomp.Refusal(fn)
				if why == "" {
					why = "no register code"
				}
				what += " -- refused: " + why
			}
		}
		fmt.Fprintf(&sb, "  %10d  %-8s %s\n", r.n, [...]string{"direct", "enter", "out", "new"}[r.key.kind], what)
	}
	os.Stderr.WriteString(sb.String())
}

// The register tier's fallbacks: the times register mode has run code
// in the stack interpreter or the tree tier, or entered register code
// through their frames (docs/register-interpreter.md, "The fallback
// counter"). Register mode's goal is none.
const (
	// regFallNoCode is a function firstForm built a tree for, the
	// backend having given it no register code.
	regFallNoCode = iota
	// regFallStack is a frame regStmt ran in the stack interpreter.
	regFallStack
	// regFallTree is register code entered through runTree: a frame
	// runFD or callTree made, rather than regRunFD's or a register
	// call's own.
	regFallTree
	regFallKinds
)

// regFallName is the name of fallback kind k.
func regFallName(k int) string {
	switch k {
	case regFallNoCode:
		return "no register code"
	case regFallStack:
		return "stack interpreter"
	}
	return "entered through runTree"
}

// regFallbacks is the counts, by kind, and in its last word whether to
// trace them: 64 bytes, a cache line, so that the data laid out after it
// keeps its offsets in theirs (layoutlock.txt).
var regFallbacks [8]atomic.Int64

// regFallback counts a fallback of kind, of function fn.
//
//go:noinline
func regFallback(kind int, fn *bytecode.Function) {
	regFallbacks[kind].Add(1)
	// Whether QJS_REGFALLTRACE asks for the trace, read once (a read of
	// the environment allocates): 1 not, 2 so, in the counts' last word.
	trace := &regFallbacks[len(regFallbacks)-1]
	if trace.Load() == 0 {
		v := int64(1)
		if os.Getenv("QJS_REGFALLTRACE") != "" {
			v = 2
		}
		trace.Store(v)
	}
	if trace.Load() == 2 {
		var l int32
		if fn.Script != nil {
			l, _ = fn.Script.Position(fn.Start)
		}
		println("reg fallback:", regFallName(kind), fn.Name, "line", l)
	}
}

// RegFallbacks returns the register tier's fallbacks counted since the
// last ResetRegFallbacks, in all and by kind, as "kind: n" lines.
func RegFallbacks() (int64, string) {
	var n int64
	var sb strings.Builder
	for k := range regFallKinds {
		c := regFallbacks[k].Load()
		n += c
		if c != 0 {
			fmt.Fprintf(&sb, "%s: %d\n", regFallName(k), c)
		}
	}
	return n, sb.String()
}

// ResetRegFallbacks clears the fallback counts.
func ResetRegFallbacks() {
	for k := range regFallbacks {
		regFallbacks[k].Store(0)
	}
}
