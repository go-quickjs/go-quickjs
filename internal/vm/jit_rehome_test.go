//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// rehomeSetup's functions run loops long enough to be entered natively at
// their headers and to poll many times there, each with registers that
// the loop reassigns.
const rehomeSetup = `
	function fill(turns, len) {
		let pool = mkpool(turns), a, s = 0
		for (let j = 0; j < turns; j++) {
			a = pool[j]
			pool[j] = null
			for (let i = 0; i < len; i++) a[i] = i + j
			s += a[len - 1]
		}
		return s
	}
	function swap(n) {
		let a = mk(), b = a, s = 0
		for (let i = 0; i < n; i++) {
			b = a
			a = [i, i + 1]
			s += b.length * 3 + (b[0] | 0)
		}
		return s + ':' + b[0] + ':' + a[0]
	}
	function walk(n) {
		let p = mklist(n), s = 0, k = 0
		while (p !== null) {
			s += p.v
			p = p.next
			k++
		}
		return s + ':' + k
	}
	function nested(n) {
		let a = [mkobj(0)], s = 0
		for (let j = 0; j < 4; j++) {
			let x = a[0]
			a = [mkobj(j + 1)]
			for (let i = 0; i < n; i++) s += x.v
		}
		return s + ':' + a[0].v
	}
	function hold(a, n) {
		let b = a, s = 0
		a = null
		for (let i = 0; i < n; i++) s += b.length + i
		return s + ':' + b.length + ':' + a
	}
	function kept(n) {
		let o = mkobj(7), a = mk(), s = 0
		for (let i = 0; i < n; i++) {
			if (i % 97 === 0) a = [i]
			s += o.v + a[0] + (i === n - 3 ? 'z' : 0)
		}
		return s.length + ':' + s.slice(-12) + ':' + o.v + ':' + a[0]
	}`

// rehomeRuntime is a runtime that has run rehomeSetup, its JIT at the
// runtime's own thresholds, so that the loops are entered natively at
// their headers, and polling every 64 back edges, Go giving each a new
// budget and native code going on; noRehome has its polls bring no frame
// up to date. mk's and mkpool's arrays are known by their weak
// pointers. Its polls run the collector, at every eighth, or at each
// while *check is set, when they count in *kept those where an array of
// the last pool fill took is still alive after one it took later has
// been collected: the entry's, kept in a's slot.
func rehomeRuntime(t *testing.T, jit, noRehome bool, every int, made *[]weak.Pointer[Object], check *bool, kept *int) *Runtime {
	t.Helper()
	pool := new(int)
	// A stack of collectingStack's size, which each collection marks.
	r := newTestRuntime(t, Config{JIT: jit, StackSize: collectingStack})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if jit {
		jitDefaultsForTest(r)
		r.jitState().stressBudget = 64
		// This runtime's alone, not the test's (jitTestSwitch), which
		// would reach the others it made.
		polls := 0
		r.jitTest.noRehome = noRehome
		// Every poll that may bring the frame up to date does, so that
		// the collector forced at each sees it so; the runtime's own
		// rate for the test's last runtime (every < 0).
		r.jitTest.rehomeEvery = max(every, 0)
		r.jitTest.pollMade = func(bool) {
			// The collector forced at the polls: what Go wrote, and what
			// native code reads after, while the objects the frame no
			// longer holds are collected (cleared, in the child that runs
			// with GODEBUG=clobberfree=1).
			if polls++; polls%8 != 0 && !*check {
				return
			}
			runtime.GC()
			if !*check {
				return
			}
			// The pool's arrays still alive are those of the turns to come
			// and the turn's own; one alive before one collected is kept.
			gone := false
			for i := len(*made) - 1; i >= *pool; i-- {
				if (*made)[i].Value() == nil {
					gone = true
				} else if gone {
					*kept++
					break
				}
			}
		}
	}
	def := func(name string, n int, fn func(rt *Runtime, args []Value) Value) {
		r.global.setOwnRaw(r.atoms.intern(name), Obj(r.newNativeFunc(name, n, func(rt *Runtime, this Value, args []Value) (Value, error) {
			return fn(rt, args), nil
		})), propDefault)
	}
	def("mk", 0, func(rt *Runtime, args []Value) Value {
		a := rt.newArrayFrom(make([]Value, 1<<14))
		*made = append(*made, weak.Make(a))
		return Obj(a)
	})
	def("mkpool", 1, func(rt *Runtime, args []Value) Value {
		// Made by Go, so that no register of the VM's stack holds any of
		// its arrays: fill's a alone does, each in its turn.
		arrays := make([]Value, int(args[0].Number()))
		*pool = len(*made)
		for i := range arrays {
			a := rt.newArrayFrom(make([]Value, 1<<14))
			*made = append(*made, weak.Make(a))
			arrays[i] = Obj(a)
		}
		return Obj(rt.newArrayFrom(arrays))
	})
	obj := func(rt *Runtime, v, next Value) *Object {
		o := rt.NewObject()
		if err := rt.DefineProp(o, rt.atoms.intern("v"), v); err != nil {
			t.Fatal(err)
		}
		if next != Undefined {
			if err := rt.DefineProp(o, rt.atoms.intern("next"), next); err != nil {
				t.Fatal(err)
			}
		}
		return o
	}
	def("mkobj", 1, func(rt *Runtime, args []Value) Value { return Obj(obj(rt, args[0], Undefined)) })
	def("mklist", 1, func(rt *Runtime, args []Value) Value {
		// Made by Go, so that no register of the VM's stack holds any of
		// its nodes: only the walk's does, the head first.
		head := Null
		for i := int(args[0].Number()) - 1; i >= 0; i-- {
			head = Obj(obj(rt, Int(i), head))
		}
		return head
	})
	if _, err := r.Run(compileForTest(t, rehomeSetup)); err != nil {
		t.Fatal(err)
	}
	return r
}

const rehomeClobberChild = "QJS_TEST_REHOME_CLOBBER_CHILD"

// A loop native code entered at its header keeps, in the slot of a
// register it reassigns, the reference the register held at the entry:
// native code writes the frame only at its exits, and reads a reference
// through the slot or the cell it came from. At a poll native code goes on
// after, Go writes the header's values into the frame's registers where
// nothing live reads what they held (jit.Rehome, abi.Rehome), so that the
// array fill's loop took a turn before is collected during the next, with
// the JIT as without it; with the polls bringing nothing up to date, it
// is kept to the loop's end. What native code reads after the polls is
// each answer the interpreter's: a register whose old value another
// still reads (b = a; a = [...]), a list walked through n = n.next, whose
// nodes behind it are collected under it -- the walk reads each node's
// reference from where Go wrote it, not from the cell of the node before,
// which may be gone -- a value read from an object the register held
// before the loop (nested's x, which leaves its loop's polls with nothing
// to write), and an exit after many polls, whose elision of a slot
// unchanged finds what the poll wrote there. The collector runs at the
// polls, in a child too that clears what it frees (clobberfree), and with
// native code seeing it marking.
func TestJITRehomeAtPolls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	if os.Getenv(rehomeClobberChild) == "" && !testing.Short() {
		defer func() {
			godebug := "clobberfree=1"
			if g := os.Getenv("GODEBUG"); g != "" {
				godebug = g + "," + godebug
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestJITRehomeAtPolls$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), rehomeClobberChild+"=1", "GODEBUG="+godebug)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "--- PASS: TestJITRehomeAtPolls") {
				t.Fatalf("child with clobberfree: %v\n%s", err, out)
			}
		}()
	}
	t.Run("plain", func(t *testing.T) { rehomeCases(t) })
	t.Run("marking", func(t *testing.T) {
		jitMarkingForTest(t)
		rehomeCases(t)
	})
}

func rehomeCases(t *testing.T) {
	described := map[string]int{}
	jitTestSwitch(t, func(jc *jitTestConfig) {
		jc.codeHook = func(f JITFunction, _ []byte, code *mir.Code) {
			for _, st := range code.GoCalls {
				if ssa.GoOp(st.Op&0xff) == ssa.GoPoll && st.Rehome != nil {
					described[f.Name]++
				}
			}
		}
	})
	var made, wantMade, offMade []weak.Pointer[Object]
	var check bool
	var kept, offKept int
	r := rehomeRuntime(t, true, false, 1, &made, &check, &kept)
	want := rehomeRuntime(t, false, false, 1, &wantMade, &check, nil)
	off := rehomeRuntime(t, true, true, 1, &offMade, &check, &offKept)
	run := func(rt *Runtime, src string) string {
		t.Helper()
		v, err := rt.Run(compileForTest(t, "''+("+src+")"))
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		return v.String().Go()
	}
	// The array a turn of fill took is collected during the next turn's
	// loop -- at each poll, once a is the next array -- as the
	// interpreter, whose a holds the next, lets it be; kept with polls
	// that write nothing, the loop entered at its header holding the
	// first turn's array in a's slot.
	check = true
	for _, src := range []string{`fill(5, 3000)`, `fill(6, 2500)`} {
		if got, w := run(r, src), run(want, src); got != w {
			t.Errorf("%s: got %s, interpreter %s", src, got, w)
		}
	}
	if kept != 0 {
		t.Errorf("fill: an array of a turn before alive at %d polls, want none", kept)
	}
	run(off, `fill(5, 3000)`)
	check = false
	if offKept == 0 {
		t.Error("fill with polls that write nothing: no array of a turn before alive at any poll; the test no longer reaches a register the loop's entry left its reference in")
	}
	srcs := []string{`swap(5000)`, `swap(7000)`, `walk(20000)`, `walk(30000)`,
		`nested(3000)`, `nested(4000)`, `kept(5000)`, `kept(6000)`}
	for range 12 {
		// hold's code entered at its start, once its calls have it
		// compiled: b is a's load, its reference read from a's slot,
		// which the loop's polls must leave as it is.
		srcs = append(srcs, `hold(mk(), 3000)`)
	}
	for _, src := range srcs {
		if got, w := run(r, src), run(want, src); got != w {
			t.Errorf("%s: got %s, interpreter %s", src, got, w)
		}
	}
	for _, name := range []string{"fill", "swap", "walk", "kept", "hold"} {
		if e := jitRegLifeForTest(r, name); e == nil || e.ssaStats.entries == 0 {
			t.Errorf("%s never ran natively", name)
		}
		if described[name] == 0 {
			t.Errorf("%s's code has no poll that may write the frame", name)
		}
	}
	if r.jit.rehomed == 0 {
		t.Error("no register written at a poll")
	}
	// At the runtime's own rate, one poll in jitRehomeEvery: the same
	// answers, registers written.
	var ownMade []weak.Pointer[Object]
	var ownKept int
	own := rehomeRuntime(t, true, false, -1, &ownMade, new(bool), &ownKept)
	for _, src := range append(srcs, `fill(5, 3000)`) {
		if got, w := run(own, src), run(want, src); got != w {
			t.Errorf("%s at the runtime's own rate: got %s, interpreter %s", src, got, w)
		}
	}
	if own.jit.rehomed == 0 {
		t.Error("no register written at a poll at the runtime's own rate")
	}
	if off.jit.rehomed != 0 {
		t.Errorf("%d registers written at polls told to write none", off.jit.rehomed)
	}
}
