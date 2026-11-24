package engine

import (
	"strings"
	"sync"
	"testing"
	"weak"
)

// A test compiles and runs its Codes in a mode other than the process's --
// native code seeing the collector marking, its exits counted, a clock of
// its own -- by making them so (Code's test, a codeTest), never by setting
// a package's variable, which every other Code in the process would read.
// Every Code the package's tests make is given its test's switches
// (testCode); a switch a test sets (codeSwitch) is its Codes', those it
// makes from then on and those it and the tests it is a subtest of made
// before, until it ends, and no other test's.

// codeTests is the tests' switches and Codes, by the test's name, which is
// unique among the tests running: each test's own, dropped when it ends.
var codeTests sync.Map // string -> *codeTestEntry

// codeTestEntry is a test's switches, the changes it made in order, its
// Codes, held weakly, and, a top-level test's, the counts of its Codes'
// native exits.
type codeTestEntry struct {
	tb     testing.TB
	mu     sync.Mutex
	sets   []func(*codeTest)
	codes  []weak.Pointer[Code]
	counts jitExitCounts
}

// codeTestOf is tb's entry, made at the first switch it sets or Code it
// makes.
func codeTestOf(tb testing.TB) *codeTestEntry {
	name := tb.Name()
	e := &codeTestEntry{tb: tb}
	for {
		v, loaded := codeTests.LoadOrStore(name, e)
		if !loaded {
			break
		}
		if old := v.(*codeTestEntry); old.tb == tb {
			return old
		} else if codeTests.CompareAndSwap(name, old, e) {
			break
		}
	}
	tb.Cleanup(func() { codeTests.CompareAndDelete(name, e) })
	return e
}

// codeTestChain is the entries of tb and of the tests it is a subtest of,
// the outermost first.
func codeTestChain(tb testing.TB) []*codeTestEntry {
	codeTestOf(tb)
	var chain []*codeTestEntry
	for name := tb.Name(); ; {
		if v, ok := codeTests.Load(name); ok {
			chain = append([]*codeTestEntry{v.(*codeTestEntry)}, chain...)
		}
		i := strings.LastIndexByte(name, '/')
		if i < 0 {
			return chain
		}
		name = name[:i]
	}
}

// testCode gives c, a Code tb made, tb's switches (codeSwitch), and
// counts its native exits into tb's top-level test's counts (countExits).
func testCode(tb testing.TB, c *Code) *Code {
	chain := codeTestChain(tb)
	t := &codeTest{counts: &chain[0].counts}
	for _, e := range chain {
		e.mu.Lock()
		for _, set := range e.sets {
			set(t)
		}
		e.mu.Unlock()
	}
	c.setTest(t)
	// A Code the test made itself is run as NewCode's are.
	c.native = regNativeDefault
	e := chain[len(chain)-1]
	e.mu.Lock()
	e.codes = append(e.codes, weak.Make(c))
	e.mu.Unlock()
	return c
}

// codeSwitch applies set to the Codes tb makes from now on, and to those
// tb and the tests it is a subtest of have made, until tb ends, when those
// are as they were.
func codeSwitch(tb testing.TB, set func(*codeTest)) {
	chain := codeTestChain(tb)
	own := chain[len(chain)-1]
	own.mu.Lock()
	own.sets = append(own.sets, set)
	own.mu.Unlock()
	for _, e := range chain {
		e.mu.Lock()
		codes := append([]weak.Pointer[Code](nil), e.codes...)
		e.mu.Unlock()
		for _, w := range codes {
			c := w.Value()
			if c == nil || c.test == nil {
				continue
			}
			was := *c.test
			set(c.test)
			tb.Cleanup(func() { *c.test = was })
		}
	}
}

// countExits returns the exits the native code of t's Codes takes while f
// runs: of the Codes of t's top-level test.
func countExits(t testing.TB, f func()) int64 {
	c := &codeTestChain(t)[0].counts
	before := c.exits.Load()
	f()
	return c.exits.Load() - before
}

// countAllocExits returns the allocations the native code of t's Codes
// leaves to Go while f runs -- its calls of the allocation stub and its
// ExitAllocs -- and all its exits.
func countAllocExits(t testing.TB, f func()) (alloc, all int64) {
	stubs, exits, all := countAllocs(t, f)
	return stubs + exits, all
}

// countAllocs returns the calls of the allocation stub the native code of
// t's Codes makes while f runs, its ExitAllocs, and all its exits.
func countAllocs(t testing.TB, f func()) (stubs, allocs, all int64) {
	c := &codeTestChain(t)[0].counts
	s, a := c.stubs.Load(), c.allocs.Load()
	all = countExits(t, f)
	return c.stubs.Load() - s, c.allocs.Load() - a, all
}

// TestCodeSwitchesAreTheTests checks that a switch a test sets reaches the
// Codes it and its subtests make and those it made before, and no other
// test's, and that those made before are as they were once it ends.
func TestCodeSwitchesAreTheTests(t *testing.T) {
	const src = `(module (func (export "f")))`
	before := instantiateReg(t, src).Code
	t.Run("set", func(t *testing.T) {
		codeSwitch(t, func(c *codeTest) { c.maxRegCode = 7 })
		if got := instantiateReg(t, src).Code.test.maxRegCode; got != 7 {
			t.Errorf("a Code made after the switch: %d", got)
		}
		if before.test.maxRegCode != 7 {
			t.Errorf("a Code made before the switch: %d", before.test.maxRegCode)
		}
	})
	if before.test.maxRegCode != 0 {
		t.Errorf("after the subtest, the Code made before it: %d", before.test.maxRegCode)
	}
	if got := instantiateReg(t, src).Code.test.maxRegCode; got != 0 {
		t.Errorf("a Code of the parent after the subtest: %d", got)
	}
}
