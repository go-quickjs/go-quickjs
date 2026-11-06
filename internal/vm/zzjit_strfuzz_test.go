//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"testing"
)

// strFuzzSettings are the stress settings native code's differential tests
// of an unmetered runtime run under.
var strFuzzSettings = []jitStressConfig{
	{threshold: true},
	{threshold: true, budget: 1},
	{threshold: true, budget: 3, deopt: 7},
	{threshold: true, background: true, checks: 4, budget: 3, deopt: 7},
}

// strFuzz's programs in native code, compiled at once and through every
// exit stress can make, the collector idle and marking.
func TestJITStrAppendRandom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// A few seeds, as CI's jobs run these under checkptr in ten minutes:
	// QJS_STRFUZZ is for a soak.
	count := 12
	if testing.Short() {
		count = 3
	}
	first, count := strFuzzRange(t, count)
	for seed := first; seed < first+count; seed++ {
		src := strFuzzSource(seed)
		ref := New(Config{Debug: true})
		want := strFuzzRun(t, ref, src, true)
		ref.Close()
		for _, c := range strFuzzSettings {
			r := New(Config{JIT: true})
			r.jitCallThreshold, r.jitICThreshold = 1, 1
			r.jitStress = c
			got := strFuzzRun(t, r, src, false)
			r.Close()
			r.ReleaseClosed()
			if got != want {
				t.Fatalf("seed %d, %+v: %s\n%s", seed, c, firstLogDifference(got, want), src)
			}
		}
	}
}

// The same with native code seeing the collector mark.
func TestJITStrAppendRandomMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	count := 5
	if testing.Short() {
		count = 2
	}
	first, count := strFuzzRange(t, count)
	for seed := first; seed < first+count; seed++ {
		src := strFuzzSource(seed)
		ref := New(Config{Debug: true})
		want := strFuzzRun(t, ref, src, true)
		ref.Close()
		r := New(Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		r.jitStress = jitStressConfig{threshold: true, budget: 3, deopt: 7}
		got := strFuzzRun(t, r, src, false)
		r.Close()
		r.ReleaseClosed()
		if got != want {
			t.Fatalf("seed %d: %s\n%s", seed, firstLogDifference(got, want), src)
		}
	}
}

// arrayFuzzSource's programs in native code, under what stress does to the
// room a pool inherits, the collector idle and marking.
func TestJITArrayRoomRandom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	count := 4
	if testing.Short() {
		count = 1
	}
	first, count := strFuzzRange(t, count)
	for seed := first; seed < first+count; seed++ {
		src := arrayFuzzSource(seed)
		ref := New(Config{Debug: true})
		want := strFuzzRun(t, ref, src, true)
		ref.Close()
		for _, c := range strFuzzSettings {
			r := New(Config{JIT: true})
			r.jitCallThreshold, r.jitICThreshold = 1, 1
			r.jitStress = c
			got := strFuzzRun(t, r, src, false)
			r.Close()
			r.ReleaseClosed()
			if got != want {
				t.Fatalf("seed %d, %+v: %s %s", seed, c, firstLogDifference(got, want), src)
			}
		}
	}
}

// An append past the longest string leaves native code for Go, which throws,
// and what the binding held stays as it was. The right side is a rope of a
// quarter of a gigabyte, which an append keeps as it is.
func TestJITStrAppendPastTheLongest(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
function make() {
  var s = 'x'.repeat(70)
  const add = y => { s += y }
  var big = 'y'.repeat(1 << 20)
  for (let i = 0; i < 8; i++) big = big + big
  for (let i = 0; i < 40; i++) add('short')
  return { add, big, length: () => s.length }
}
let last = ''
for (let round = 0; round < 40; round++) {
  const m = make()
  try { m.add(m.big); m.add(m.big); m.add(m.big); last = 'no error' }
  catch (e) { last = e.name + ':' + e.message + ':' + m.length() }
}
last
`
	want := fmt.Sprintf("RangeError:Invalid string length:%d", 270+(1<<28))
	for _, c := range strFuzzSettings {
		r := New(Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		r.jitStress = c
		got := strFuzzRun(t, r, src, false)
		r.Close()
		r.ReleaseClosed()
		if got != want {
			t.Errorf("%+v: got %s, want %s", c, got, want)
		}
	}
}
