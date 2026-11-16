package vm

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

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
			if fn.Reg == nil {
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
