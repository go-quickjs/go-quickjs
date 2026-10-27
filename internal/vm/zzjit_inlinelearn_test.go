//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// inlineLearnSetup is the Game Boy emulator's memory read: high, a method
// that calls one of a table's readers in tail position, is inlined in op,
// and op in drive, with the reader the table's call met, rd. Its memory is
// an array, then a typed array, whose element the code inlining rd was not
// compiled to read: native code leaves there for Go.
const inlineLearnSetup = `
	"use strict"
	var mem = [], typed = new Uint8Array(64)
	for (let i = 0; i < 64; i++) { mem[i] = i * 3; typed[i] = i * 5 }
	function rd(p, a) { return p.on ? p.memory[a] : 0 }
	function Core() { this.memory = mem; this.on = true; this.readers = []; for (let i = 0; i < 8; i++) this.readers[i] = rd }
	Core.prototype.high = function (a) { return this.readers[a & 7](this, a) }
	var core = new Core()
	function op(p, i) { return p.high(i) }
	function drive(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + op(core, i & 63)) | 0; return s }
`

// An exit inside an inlined callee teaches the callee's feedback what its
// own code's exit would -- rd's element read, a typed array's -- and the
// code that inlined it is invalidated for it, as for an inlined callee's
// shape miss, and compiled again with it, still inlining rd: drive then
// runs with no exit at all, rather than leaving at every read, the rest of
// each call the interpreter's, until the inlining is given up
// (jitInlineLeft) and rd called natively.
func TestJITInlinedCalleeLearns(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{}
	for range 20 {
		rounds = append(rounds, `drive(200)`)
	}
	rounds = append(rounds, `(core.memory = typed, 0)`)
	for range 40 {
		rounds = append(rounds, `drive(200)`)
	}
	r := jitAgreesForTest(t, inlineLearnSetup, rounds...)
	rd, drive := jitCallbackEntry(r, "rd"), jitCallbackEntry(r, "drive")
	if rd == nil || drive == nil {
		t.Fatalf("no entries: rd %v drive %v", rd, drive)
	}
	if drive.ssa == nil || len(drive.ssaInlinedAt) == 0 || len(drive.notInline) != 0 {
		t.Fatalf("drive does not inline op, rd with it: inlined at %v, given up at %v", drive.ssaInlinedAt, drive.notInline)
	}
	if len(rd.typedReads) == 0 {
		t.Fatalf("rd did not learn its typed array's read: %+v", rd)
	}
	before := r.JITStats()
	if _, err := r.Run(compileForTest(t, `drive(200)`)); err != nil {
		t.Fatal(err)
	}
	if after := r.JITStats(); after.Hosts != before.Hosts || after.Interpreted != before.Interpreted {
		t.Errorf("drive still leaves native code: %d exits, %d finished in the interpreter", after.Hosts-before.Hosts, after.Interpreted-before.Interpreted)
	}
	for _, c := range []jitStressConfig{{threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}} {
		jitAgreesForTestWith(t, c, inlineLearnSetup, rounds...)
	}
}
