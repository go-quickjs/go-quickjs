package vm

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"unsafe"
	"weak"
)

// A test runs its runtimes in a mode other than the process's -- by the
// native handlers or the Go table's loop, typed code linked or not, native
// code storing as while the collector marks -- by making them so
// (Config's test), never by setting a package's variable, which every
// other runtime in the process would see: tests of different modes then
// run in parallel, each runtime as it was made.
//
// A mode is given to a runtime one of two ways. A test that makes the
// runtime itself says so in its Config (withNative, withTyped). A switch
// a test sets for the rest of it -- nativeMarking, nativeCounting,
// nativeForTest -- is the test's (setTestSwitch): every runtime the test
// and its subtests make from then on is made with it (newTestRuntime),
// whichever helper makes it, and no other test's. A runtime's switches
// are fixed when it is made: one made before the switch is set keeps
// what it was made with.

// newTestRuntime is New for a test or benchmark tb: every runtime the
// package's tests make is made by it, with the switches tb set
// (testConfigFor).
func newTestRuntime(tb testing.TB, cfg Config) *Runtime {
	r := New(testConfigFor(tb, cfg))
	e := testEntryOf(tb)
	e.mu.Lock()
	e.runtimes = append(e.runtimes, weak.Make(r))
	e.mu.Unlock()
	return r
}

// testRuntimesOf is the runtimes tb and the tests it is a subtest of have
// made so far.
func testRuntimesOf(tb testing.TB) []*Runtime {
	var rs []*Runtime
	for name := tb.Name(); ; {
		if v, ok := testSwitches.Load(name); ok {
			e := v.(*testSwitchEntry)
			e.mu.Lock()
			for _, w := range e.runtimes {
				if r := w.Value(); r != nil {
					rs = append(rs, r)
				}
			}
			e.mu.Unlock()
		}
		i := strings.LastIndexByte(name, '/')
		if i < 0 {
			return rs
		}
		name = name[:i]
	}
}

// testSwitches is the switches tests have set for the runtimes they make,
// by the test's name, which is unique among the tests running: each
// test's own, dropped when it ends.
var testSwitches sync.Map // string -> *testSwitchEntry

// testSwitchEntry is a test's switches, the changes it made in order, and
// the runtimes it has made, held weakly: a test may watch one collected.
type testSwitchEntry struct {
	tb       testing.TB
	mu       sync.Mutex
	sets     []func(*testConfig)
	runtimes []weak.Pointer[Runtime]
}

// testEntryOf is tb's entry, made at the first switch it sets or runtime
// it makes, dropped when it ends.
func testEntryOf(tb testing.TB) *testSwitchEntry {
	name := tb.Name()
	e := &testSwitchEntry{tb: tb}
	for {
		v, loaded := testSwitches.LoadOrStore(name, e)
		if !loaded {
			break
		}
		if old := v.(*testSwitchEntry); old.tb == tb {
			return old
		} else if testSwitches.CompareAndSwap(name, old, e) {
			// A test of the same name that ended, run again (-count).
			break
		}
	}
	tb.Cleanup(func() { testSwitches.CompareAndDelete(name, e) })
	return e
}

// setTestSwitch has the runtimes tb and its subtests make from now on
// made with set applied to tb's switches, until tb ends.
func setTestSwitch(tb testing.TB, set func(*testConfig)) {
	e := testEntryOf(tb)
	e.mu.Lock()
	e.sets = append(e.sets, set)
	e.mu.Unlock()
}

// testConfigFor is cfg with the switches tb set, and those the tests it is
// a subtest of set before them, in the order they were set, and cfg's own
// over them all.
func testConfigFor(tb testing.TB, cfg Config) Config {
	var chain []*testSwitchEntry
	for name := tb.Name(); ; {
		if v, ok := testSwitches.Load(name); ok {
			chain = append(chain, v.(*testSwitchEntry))
		}
		i := strings.LastIndexByte(name, '/')
		if i < 0 {
			break
		}
		name = name[:i]
	}
	var t *testConfig
	for i := len(chain) - 1; i >= 0; i-- {
		e := chain[i]
		e.mu.Lock()
		for _, set := range e.sets {
			if t == nil {
				t = new(testConfig)
			}
			set(t)
		}
		e.mu.Unlock()
	}
	if t == nil {
		return cfg
	}
	if cfg.test != nil {
		t.overlay(cfg.test)
	}
	cfg.test = t
	return cfg
}

// overlay sets t's fields to those of o that are set.
func (t *testConfig) overlay(o *testConfig) {
	overlayValue(reflect.ValueOf(t).Elem(), reflect.ValueOf(o).Elem())
}

// overlayValue sets the fields of the struct t to those of o that are set,
// a struct's field by field.
func overlayValue(t, o reflect.Value) {
	for i := range t.NumField() {
		f, d := o.Field(i), t.Field(i)
		switch {
		case f.Kind() == reflect.Struct:
			overlayValue(d, f)
		case !f.IsZero():
			reflect.NewAt(d.Type(), unsafe.Pointer(d.UnsafeAddr())).Elem().Set(reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem())
		}
	}
}

// testing is a copy of cfg's test switches, for one of the with methods
// to change.
func (cfg Config) testing() *testConfig {
	t := new(testConfig)
	if cfg.test != nil {
		*t = *cfg.test
	}
	return t
}

// withNative is cfg with its runtime's register frames run by the native
// handlers, where the build has them, or by the Go table's loop.
func (cfg Config) withNative(on bool) Config {
	t := cfg.testing()
	t.native = switchOf(on)
	cfg.test = t
	return cfg
}

// withTyped is cfg with typed code linked or not.
func (cfg Config) withTyped(on bool) Config {
	t := cfg.testing()
	t.typed = switchOf(on)
	cfg.test = t
	return cfg
}

// nativeConfig is a Config whose runtime runs register frames by the
// native handlers or by the Go table's loop.
func nativeConfig(native bool) Config { return Config{}.withNative(native) }

// nativeForTest has the runtimes tb makes from now on run register frames
// by the native handlers, where the build has them, or by the Go table's
// loop, as QJS_NONATIVE has a process's.
func nativeForTest(tb testing.TB, on bool) {
	setTestSwitch(tb, func(c *testConfig) { c.native = switchOf(on) })
}

// typedForTest has the runtimes tb makes from now on link typed code or
// not, as QJS_NOTYPED has a process's.
func typedForTest(tb testing.TB, on bool) {
	setTestSwitch(tb, func(c *testConfig) { c.typed = switchOf(on) })
}

// TestTestSwitchesAreTheTests checks that a switch a test sets reaches
// the runtimes it and its subtests make, by name segment, and no other
// test's -- TestA's never TestAB's -- and that a runtime keeps the
// switches it was made with.
func TestTestSwitchesAreTheTests(t *testing.T) {
	nativeForTest(t, false)
	before := newTestRuntime(t, Config{})
	defer before.Close()
	nativeForTest(t, true)
	after := newTestRuntime(t, Config{})
	defer after.Close()
	if before.runNative || after.runNative != regNativeBuilt {
		t.Errorf("runNative %v before the switch, %v after", before.runNative, after.runNative)
	}
	t.Run("sub", func(t *testing.T) {
		if c := testConfigFor(t, Config{}); c.test == nil || c.test.native != switchOn {
			t.Errorf("a subtest's runtime: %+v", c.test)
		}
		if c := testConfigFor(t, nativeConfig(false)); c.test.native != switchOff {
			t.Errorf("the Config's own switch under the test's: %+v", c.test)
		}
	})
	if c := testConfigFor(prefixTB{t, t.Name() + "B"}, Config{}); c.test != nil {
		t.Errorf("another test's runtime, by a name the test's begins: %+v", c.test)
	}
}

// prefixTB is a testing.TB of another name.
type prefixTB struct {
	testing.TB
	name string
}

func (p prefixTB) Name() string { return p.name }
