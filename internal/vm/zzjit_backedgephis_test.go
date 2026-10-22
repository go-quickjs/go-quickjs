//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// jitListSource is AWFY List's walk, a loop whose phis, read at its back
// edge's poll, are each written there by the edge's moves.
const jitListSource = `
	class Element { constructor(v) { this.val = v; this.next = null } }
	function makeList(n) { if (n === 0) return null; const e = new Element(n); e.next = makeList(n - 1); return e }
	function isShorterThan(x, y) {
		let xTail = x, yTail = y
		while (yTail !== null) {
			if (xTail === null) return true
			xTail = xTail.next
			yTail = yTail.next
		}
		return false
	}
	const lists = []
	for (let i = 0; i <= 12; i++) lists.push(makeList(i))
	function run(rounds) {
		let s = ""
		for (let k = 0; k < rounds; k++)
			for (let i = 0; i <= 12; i++)
				for (let j = 0; j <= 12; j++) s += isShorterThan(lists[i], lists[j]) ? "1" : "0"
		return s
	}`

// jitListWant is run(rounds)'s result, as the interpreter makes it.
func jitListWant(rounds int) string {
	var b strings.Builder
	for range rounds {
		for i := 0; i <= 12; i++ {
			for j := 0; j <= 12; j++ {
				if i < j {
					b.WriteByte('1')
				} else {
					b.WriteByte('0')
				}
			}
		}
	}
	return b.String()
}

// A loop's back edge polls after its phi moves, so its header's phis are
// not used at the edge's end (mir's useState): each lives only as long as
// the loop's body reads it, and the arguments the edge moves into them take
// registers of their own. Used there, the four phis of AWFY List's
// isShorterThan, live to the end of the loop's body beside the three
// values it made, took all seven registers, and the second property read's
// cell address went to a spill slot, stored and loaded back each time
// round. The loop's code is now spilled nowhere; and the values polls read
// at the back edge -- every back edge, under a budget of one, each an exit
// -- are the loop's next ones, collector running or not.
func TestJITBackEdgePhisNotSpilled(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var listing, last string
	mir.Annotate = true
	JITCodeHook = func(f JITFunction, _ []byte, c *mir.Code) {
		if f.Name == "isShorterThan" {
			listing, last = c.Listing, c.Locations()
		}
	}
	defer func() { JITCodeHook, mir.Annotate = nil, false }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, jitListSource)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		v, err := r.Run(compileForTest(t, `run(20)`))
		if err != nil || !v.IsString() || v.String().Go() != jitListWant(20) {
			t.Fatalf("run(20) = %v, %v", v, err)
		}
	}
	if !strings.Contains(listing, "propcell") || !strings.Contains(listing, "loadcell") {
		t.Fatalf("isShorterThan not compiled with its property reads:\n%s", listing)
	}
	if strings.Contains(last, "spill") {
		t.Errorf("isShorterThan's values spilled: %s", last)
	}
	for _, budget := range []uint16{1, 2, 3} {
		for _, gc := range []bool{false, true} {
			t.Run(fmt.Sprintf("budget %d gc %v", budget, gc), func(t *testing.T) {
				if gc {
					// The collector marks now and then, native code running.
					var stop atomic.Bool
					done := make(chan struct{})
					go func() {
						defer close(done)
						for !stop.Load() {
							runtime.GC()
						}
					}()
					defer func() { stop.Store(true); <-done }()
				}
				r := jitRuntimeForTest(t, Config{JIT: true})
				r.jitStress = jitStressConfig{threshold: true, budget: budget}
				if _, err := r.Run(compileForTest(t, jitListSource)); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					v, err := r.Run(compileForTest(t, `run(4)`))
					if err != nil || !v.IsString() || v.String().Go() != jitListWant(4) {
						t.Fatalf("run(4) = %v, %v", v, err)
					}
				}
				if st := r.JITStats(); st.Budgets == 0 {
					t.Fatalf("no loop left native code at its budget's end: %+v", st)
				}
			})
		}
	}
}
